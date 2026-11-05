package payment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"ecommerce/internal/audit"
)

type Options struct {
	// BaseURL is the public site URL (no trailing slash) used for the
	// provider's "finish" redirect; "" leaves it to the provider dashboard.
	BaseURL string
	// AppFinishURL is the finish redirect for payments started from the
	// mobile API (an app link; {order_number} is replaced). Server config
	// only, never from a client. "" uses the web finish page.
	AppFinishURL string
	// Sandbox marks test payments (no real money), shown in the UI.
	Sandbox bool
	Log     *slog.Logger
	// Now is the clock (tests); time.Now when nil.
	Now func() time.Time
}

type Service struct {
	repo     *Repository
	provider Provider // nil when no provider is configured
	opts     Options
	locks    keyedMutex
}

// NewService builds the service. provider may be nil: online payment is then
// unavailable, but cancelling and expiring unpaid orders still work.
func NewService(repo *Repository, provider Provider, opts Options) *Service {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{repo: repo, provider: provider, opts: opts}
}

// Enabled reports whether online payment is available.
func (s *Service) Enabled() bool { return s.provider != nil }

// Sandbox reports whether payments are test payments.
func (s *Service) Sandbox() bool { return s.opts.Sandbox }

// Attempts returns an order's payment attempts, oldest first.
func (s *Service) Attempts(ctx context.Context, orderID int64) ([]Attempt, error) {
	list, err := s.repo.Attempts(ctx, orderID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].PaymentType != "" && s.provider != nil {
			list[i].Method = s.provider.MethodLabel(list[i].PaymentType)
		} else {
			list[i].Method = list[i].PaymentType
		}
	}
	return list, nil
}

// Pay returns the provider checkout URL for the user's unpaid order. It
// reuses the pending attempt when there is one, so repeated clicks open the
// same checkout; otherwise it creates attempt n+1 with a new provider order
// ID. The amount is always orders.total, never a browser value.
func (s *Service) Pay(ctx context.Context, userID int64, number string) (string, error) {
	return s.pay(ctx, userID, number, false)
}

// PayFromApp is Pay for the mobile API: same attempts, amounts and state
// machine; only the provider's finish redirect points at the configured app
// link. A pending attempt started on the web is reused as it is.
func (s *Service) PayFromApp(ctx context.Context, userID int64, number string) (string, error) {
	return s.pay(ctx, userID, number, true)
}

func (s *Service) pay(ctx context.Context, userID int64, number string, app bool) (string, error) {
	if s.provider == nil {
		return "", ErrNotConfigured
	}
	orderID, err := s.repo.OrderID(ctx, userID, number)
	if err != nil {
		return "", err
	}
	unlock := s.locks.lock(orderID)
	defer unlock()

	var (
		o       orderRow
		a       Attempt
		items   []LineItem
		reuse   bool
		start   time.Time
		expires time.Time
	)
	err = s.inTx(ctx, func(tx *Repository) error {
		var err error
		reuse = false
		if o, err = tx.Order(ctx, orderID); err != nil {
			return err
		}
		if o.Status != orderWaitingPayment {
			return ErrNotPayable
		}
		if items, err = lineItems(ctx, tx, o); err != nil {
			return err
		}
		now := s.opts.Now()
		p, err := tx.Pending(ctx, orderID)
		if err != nil {
			return err
		}
		if p != nil && p.RedirectURL != "" {
			if now.Before(p.ExpiresAt) && p.Amount == o.Total {
				a, reuse = *p, true
				return nil
			}
			return ErrOrderExpired
		}
		var ok bool
		if start, expires, ok = attemptWindow(now, o.DueAt); !ok {
			return ErrOrderExpired
		}
		if p != nil {
			// Created earlier but never got a checkout (the provider call
			// failed or the process stopped): nobody can pay it, close it.
			if _, err := tx.SetStatus(ctx, p.ID, StatusFailed, nil); err != nil {
				return err
			}
			if err := tx.Event(ctx, event{OrderID: o.ID, PaymentID: p.ID, Source: sourceUser, Kind: "abandoned",
				From: StatusPending, To: StatusFailed, Detail: "attempt without provider checkout"}); err != nil {
				return err
			}
		}
		n, err := tx.NextAttemptNo(ctx, orderID)
		if err != nil {
			return err
		}
		if n > MaxAttempts {
			return ErrTooManyAttempts
		}
		a, err = tx.CreateAttempt(ctx, orderID, n, s.provider.Name(), ProviderOrderID(o.Number, n), o.Total, expires)
		if err != nil {
			return err
		}
		return tx.Event(ctx, event{OrderID: o.ID, PaymentID: a.ID, Source: sourceUser, Kind: "created", To: StatusPending,
			Detail: fmt.Sprintf("attempt %d, amount %d", n, o.Total)})
	})
	if errors.Is(err, ErrOrderExpired) {
		if _, xerr := s.expireOverdue(ctx, orderID, sourceSystem); xerr != nil {
			s.opts.Log.Error("payment: lazy expiry", "order", number, "error", xerr)
		}
		return "", ErrOrderExpired
	}
	if err != nil {
		return "", err
	}
	if reuse {
		return a.RedirectURL, nil
	}

	co, err := s.provider.CreateTransaction(ctx, TransactionRequest{
		ProviderOrderID: a.ProviderOrderID,
		Amount:          a.Amount,
		Items:           items,
		Customer:        Customer{Name: o.Recipient, Email: o.Email, Phone: o.Phone},
		Start:           start,
		ExpiresAt:       expires,
		FinishURL:       s.finishURL(o.Number, app),
	})
	if err != nil {
		s.opts.Log.Warn("payment: create transaction failed", "order", o.Number, "attempt", a.No,
			"provider_order_id", a.ProviderOrderID, "error", err)
		if cerr := s.closeUnstarted(ctx, a, err); cerr != nil {
			s.opts.Log.Error("payment: close failed attempt", "provider_order_id", a.ProviderOrderID, "error", cerr)
		}
		if !errors.Is(err, ErrProviderRejected) {
			err = errors.Join(ErrProviderUnavailable, err)
		}
		return "", err
	}
	ok, err := s.repo.SetCheckout(ctx, a.ID, co)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotPayable
	}
	s.opts.Log.Info("payment: attempt opened", "order", o.Number, "attempt", a.No,
		"provider_order_id", a.ProviderOrderID, "amount", a.Amount)
	return co.RedirectURL, nil
}

// closeUnstarted marks an attempt whose provider checkout could not be
// created as FAILED, so the next click starts a new attempt.
func (s *Service) closeUnstarted(ctx context.Context, a Attempt, cause error) error {
	return s.inTx(ctx, func(tx *Repository) error {
		if _, err := tx.Order(ctx, a.OrderID); err != nil {
			return err
		}
		ok, err := tx.SetStatus(ctx, a.ID, StatusFailed, nil)
		if err != nil || !ok {
			return err
		}
		if err := tx.RecordProvider(ctx, a.ID, ProviderStatus{Raw: "create_failed"}); err != nil {
			return err
		}
		return tx.Event(ctx, event{OrderID: a.OrderID, PaymentID: a.ID, Source: sourceUser, Kind: "create_failed",
			From: StatusPending, To: StatusFailed, Detail: errorCategory(cause)})
	})
}

// CancelOrder cancels the user's unpaid order and gives its stock back. An
// open provider transaction is cancelled first; if the provider says it is
// paid, or the cancellation cannot be confirmed, the order is kept.
// Cancelling an already cancelled order succeeds without changes.
func (s *Service) CancelOrder(ctx context.Context, userID int64, number string) error {
	orderID, err := s.repo.OrderID(ctx, userID, number)
	if err != nil {
		return err
	}
	return s.cancelUnpaid(ctx, orderID, canceller{reason: reasonUserCancelled, source: sourceUser})
}

// CancelByAdmin cancels an unpaid order on an admin's decision, by the same
// rules as the buyer's cancellation (provider transaction first, stock back
// once), and records it in the admin audit log. A paid order is
// ErrAlreadyPaid: the fulfillment service cancels those with a refund
// record instead.
func (s *Service) CancelByAdmin(ctx context.Context, adminID, orderID int64, note string) error {
	return s.cancelUnpaid(ctx, orderID, canceller{reason: reasonAdminCancelled, source: sourceAdmin, adminID: adminID, note: note})
}

// canceller says who cancels an unpaid order and why.
type canceller struct {
	reason  string
	source  string
	adminID int64 // 0 unless an admin cancels
	note    string
}

func (s *Service) cancelUnpaid(ctx context.Context, orderID int64, c canceller) error {
	unlock := s.locks.lock(orderID)
	defer unlock()

	o, err := s.repo.Order(ctx, orderID)
	if err != nil {
		return err
	}
	if err := cancellable(o); err != nil {
		return err
	}
	p, err := s.repo.Pending(ctx, orderID)
	if err != nil {
		return err
	}
	if p != nil && p.RedirectURL != "" {
		if s.provider == nil {
			return ErrCancelUnavailable
		}
		st, err := s.provider.Cancel(ctx, p.ProviderOrderID)
		if err != nil {
			s.opts.Log.Warn("payment: provider cancel failed", "order", o.Number, "provider_order_id", p.ProviderOrderID, "error", err)
			return ErrCancelUnavailable
		}
		if st.Found {
			if _, err := s.apply(ctx, p.ID, orderID, st, c.source); err != nil {
				return err
			}
			switch st.Status {
			case StatusPaid:
				return ErrAlreadyPaid
			case StatusPending, "":
				return ErrCancelUnavailable
			}
		}
	}

	return s.inTx(ctx, func(tx *Repository) error {
		o, err := tx.Order(ctx, orderID)
		if err != nil {
			return err
		}
		if o.Status == orderCancelled {
			return nil
		}
		if err := cancellable(o); err != nil {
			return err
		}
		return s.cancelLocked(ctx, tx, o, c, StatusCancelled)
	})
}

func cancellable(o orderRow) error {
	switch o.Status {
	case orderWaitingPayment, orderCancelled:
		return nil
	case orderWaitingConfirmation:
		return ErrAlreadyPaid
	}
	return ErrNotCancellable
}

// cancelLocked cancels a locked unpaid order and restores its stock.
func (s *Service) cancelLocked(ctx context.Context, tx *Repository, o orderRow, c canceller, closeAs Status) error {
	reason, source := c.reason, c.source
	paid, err := tx.HasPaid(ctx, o.ID)
	if err != nil {
		return err
	}
	if paid {
		return ErrAlreadyPaid
	}
	res, err := tx.CancelAndRestore(ctx, o.ID, reason, closeAs, c.adminID)
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("%s; stock restored for %d variant(s)", reason, res.Variants)
	if err := tx.Event(ctx, event{OrderID: o.ID, Source: source, Kind: "order_cancelled", Detail: detail}); err != nil {
		return err
	}
	if res.Missing > 0 {
		// The variant row is gone, so there is no stock to give back. This
		// cannot happen for orders created after migration 0007 (deleting a
		// reserved variant is refused); record it for reconciliation.
		msg := fmt.Sprintf("%d line(s) without a catalog variant: stock not restored", res.Missing)
		if err := tx.Event(ctx, event{OrderID: o.ID, Source: source, Kind: "restock_missing", Detail: msg}); err != nil {
			return err
		}
		s.opts.Log.Warn("payment: restock skipped lines", "order", o.Number, "missing", res.Missing)
	}
	s.opts.Log.Info("payment: order cancelled", "order", o.Number, "reason", reason, "variants_restored", res.Variants)
	if c.adminID == 0 {
		return nil
	}
	return audit.Record(ctx, tx.q, audit.Entry{AdminID: c.adminID, Action: "order.cancel", EntityType: audit.EntityOrder,
		EntityID: o.ID, Label: o.Number, Meta: map[string]any{"from": o.Status, "paid": false, "note": c.note, "variants_restored": res.Variants}})
}

// HandleNotification processes a provider notification: it is accepted only
// with a valid signature, and then treated as a hint. The status applied is
// the one the provider returns from GetStatus, so a replayed or reordered
// notification cannot move an attempt to a stale status.
func (s *Service) HandleNotification(ctx context.Context, body []byte) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	n, err := s.provider.ParseNotification(body)
	if err != nil {
		return err
	}
	paymentID, orderID, err := s.repo.Ref(ctx, n.ProviderOrderID)
	if err != nil {
		return err
	}
	st, err := s.provider.GetStatus(ctx, n.ProviderOrderID)
	if err != nil {
		return err
	}
	if !st.Found {
		return fmt.Errorf("%w: notified transaction %s not found", ErrProviderUnavailable, n.ProviderOrderID)
	}
	_, err = s.apply(ctx, paymentID, orderID, st, sourceWebhook)
	return err
}

// Sync asks the provider about the order's pending attempt (the buyer just
// came back from the provider checkout) and expires the order when its
// deadline has passed. Provider errors are logged: the page shows the last
// known status and notifications will catch up.
func (s *Service) Sync(ctx context.Context, orderID int64) error {
	if s.provider != nil {
		p, err := s.repo.Pending(ctx, orderID)
		if err != nil {
			return err
		}
		if p != nil && p.RedirectURL != "" {
			st, err := s.provider.GetStatus(ctx, p.ProviderOrderID)
			switch {
			case err != nil:
				s.opts.Log.Warn("payment: status check failed", "provider_order_id", p.ProviderOrderID, "error", err)
			case st.Found:
				if _, err := s.apply(ctx, p.ID, orderID, st, sourceStatusCheck); err != nil {
					return err
				}
			}
		}
	}
	_, err := s.ExpireOverdue(ctx, orderID)
	return err
}

// ExpireOverdue cancels an unpaid order whose payment deadline has passed
// and gives its stock back (lazy expiry from order pages, and reconcile).
// It reports whether the order is cancelled now. An attempt that may still
// be open at the provider is checked (and cancelled there) first; when the
// provider cannot be reached the order is left for a later try.
func (s *Service) ExpireOverdue(ctx context.Context, orderID int64) (bool, error) {
	unlock := s.locks.lock(orderID)
	defer unlock()
	return s.expireOverdue(ctx, orderID, sourceSystem)
}

func (s *Service) expireOverdue(ctx context.Context, orderID int64, source string) (bool, error) {
	o, err := s.repo.Order(ctx, orderID)
	if err != nil {
		return false, err
	}
	now := s.opts.Now()
	if o.Status != orderWaitingPayment || now.Before(o.DueAt) {
		return o.Status == orderCancelled, nil
	}
	p, err := s.repo.Pending(ctx, orderID)
	if err != nil {
		return false, err
	}
	if p != nil && p.RedirectURL != "" {
		if now.Before(o.DueAt.Add(expiryGrace)) || s.provider == nil {
			return false, nil
		}
		st, err := s.provider.GetStatus(ctx, p.ProviderOrderID)
		if err == nil && st.Found && st.Status == StatusPending {
			st, err = s.provider.Cancel(ctx, p.ProviderOrderID)
		}
		if err != nil {
			s.opts.Log.Warn("payment: expiry check failed", "order", o.Number, "provider_order_id", p.ProviderOrderID, "error", err)
			return false, nil
		}
		if st.Found {
			if _, err := s.apply(ctx, p.ID, orderID, st, source); err != nil {
				return false, err
			}
			if st.Status == StatusPending || st.Status == StatusPaid || st.Status == "" {
				return false, nil
			}
		}
		// Not found at the provider: the checkout was never used and has
		// expired with the deadline, so the order can be cancelled.
	}

	cancelled := false
	err = s.inTx(ctx, func(tx *Repository) error {
		o, err := tx.Order(ctx, orderID)
		if err != nil {
			return err
		}
		if o.Status != orderWaitingPayment || now.Before(o.DueAt) {
			cancelled = o.Status == orderCancelled
			return nil
		}
		if err := s.cancelLocked(ctx, tx, o, canceller{reason: reasonPaymentExpired, source: source}, StatusExpired); err != nil {
			return err
		}
		cancelled = true
		return nil
	})
	if errors.Is(err, ErrAlreadyPaid) {
		return false, nil
	}
	return cancelled, err
}

// apply records a provider status for one attempt and moves the attempt
// and its order according to decide, in one transaction that locks the
// order row first, then the attempt. Applying the same status again changes
// nothing, so notifications and checks can be replayed safely.
func (s *Service) apply(ctx context.Context, paymentID, orderID int64, st ProviderStatus, source string) (decision, error) {
	var (
		d         decision
		straggler string
	)
	err := s.inTx(ctx, func(tx *Repository) error {
		straggler = ""
		o, err := tx.Order(ctx, orderID)
		if err != nil {
			return err
		}
		a, err := tx.LockAttempt(ctx, paymentID)
		if err != nil {
			return err
		}
		if err := tx.RecordProvider(ctx, a.ID, st); err != nil {
			return err
		}
		ev := event{OrderID: o.ID, PaymentID: a.ID, Source: source, From: a.Status, To: st.Status, Raw: st.Raw}

		d = decide(a.Status, st.Status)
		if d == move && st.Status == StatusPaid {
			otherPaid, err := tx.HasPaid(ctx, o.ID)
			if err != nil {
				return err
			}
			if reason := paidMismatch(o, a, st, otherPaid); reason != "" {
				d, ev.Detail = review, reason
			}
		}

		switch d {
		case keep:
			ev.Kind = "duplicate"
		case ignore:
			ev.Kind = "ignored"
			if st.Status == StatusPending && (a.Status == StatusCancelled || a.Status == StatusExpired) {
				// The buyer opened a transaction on a checkout we already
				// closed: stop it at the provider before it can be paid.
				straggler = a.ProviderOrderID
			}
		case review:
			ev.Kind = "review"
			switch {
			case ev.Detail != "":
			case st.Status == StatusPaid:
				ev.Detail = fmt.Sprintf("paid after the attempt was %s: refund manually", strings.ToLower(string(a.Status)))
			default:
				ev.Detail = fmt.Sprintf("provider reports %q for a %s attempt: handle manually", st.Raw, a.Status)
			}
			if err := tx.Flag(ctx, a.ID, ev.Detail); err != nil {
				return err
			}
		case move:
			ev.Kind = "status"
			var paidAt *time.Time
			if st.Status == StatusPaid {
				t := s.opts.Now()
				if st.PaidAt != nil {
					t = *st.PaidAt
				}
				paidAt = &t
			}
			if ok, err := tx.SetStatus(ctx, a.ID, st.Status, paidAt); err != nil || !ok {
				return errors.Join(err, errors.New("payment: attempt changed concurrently"))
			}
			switch st.Status {
			case StatusPaid:
				if ok, err := tx.MarkOrderPaid(ctx, o.ID, *paidAt); err != nil || !ok {
					return errors.Join(err, errors.New("payment: order changed concurrently"))
				}
				ev.Kind = "paid"
			case StatusExpired:
				if !s.opts.Now().Before(o.DueAt) && o.Status == orderWaitingPayment {
					if err := tx.Event(ctx, ev); err != nil {
						return err
					}
					return s.cancelLocked(ctx, tx, o, canceller{reason: reasonPaymentExpired, source: source}, StatusExpired)
				}
			}
		}
		return tx.Event(ctx, ev)
	})
	if err != nil {
		return d, err
	}

	switch d {
	case move:
		s.opts.Log.Info("payment: status changed", "payment_id", paymentID, "transaction_id", st.TransactionID,
			"to", st.Status, "provider_status", st.Raw, "source", source)
	case review:
		s.opts.Log.Warn("payment: needs review", "payment_id", paymentID, "transaction_id", st.TransactionID,
			"provider_status", st.Raw, "source", source)
	}
	if straggler != "" {
		if _, err := s.provider.Cancel(ctx, straggler); err != nil {
			s.opts.Log.Warn("payment: cancel straggler failed", "provider_order_id", straggler, "error", err)
		}
	}
	return d, nil
}

// Reconcile syncs payments without relying on notifications: it asks the
// provider about pending attempts older than a few minutes (missed
// notifications), expires overdue unpaid orders, and lists attempts that
// need manual review. Each part handles at most limit rows. It is meant
// for "ecommerce reconcile-payments", run by hand or from a timer.
func (s *Service) Reconcile(ctx context.Context, limit int) (Report, error) {
	var rep Report
	now := s.opts.Now()
	if s.provider != nil {
		stale, err := s.repo.StalePending(ctx, now.Add(-stalePending), limit)
		if err != nil {
			return rep, err
		}
		for _, p := range stale {
			st, err := s.provider.GetStatus(ctx, p.ProviderOrderID)
			if err != nil {
				rep.Errors++
				s.opts.Log.Warn("reconcile: status check failed", "provider_order_id", p.ProviderOrderID, "error", err)
				continue
			}
			rep.Checked++
			if !st.Found {
				continue
			}
			d, err := s.apply(ctx, p.ID, p.OrderID, st, sourceReconcile)
			if err != nil {
				rep.Errors++
				s.opts.Log.Error("reconcile: apply status", "provider_order_id", p.ProviderOrderID, "error", err)
				continue
			}
			if d == move {
				rep.Updated++
			}
		}
	}

	overdue, err := s.repo.Overdue(ctx, now, limit)
	if err != nil {
		return rep, err
	}
	for _, id := range overdue {
		unlock := s.locks.lock(id)
		cancelled, err := s.expireOverdue(ctx, id, sourceReconcile)
		unlock()
		if err != nil {
			rep.Errors++
			s.opts.Log.Error("reconcile: expire order", "order_id", id, "error", err)
			continue
		}
		if cancelled {
			rep.Expired++
		}
	}

	rep.Review, err = s.repo.NeedingReview(ctx, limit)
	return rep, err
}

// inTx runs fn in a transaction, once more after a deadlock or
// serialization failure (fn must be safe to re-run).
func (s *Service) inTx(ctx context.Context, fn func(tx *Repository) error) error {
	err := s.repo.InTx(ctx, fn)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001") {
		err = s.repo.InTx(ctx, fn)
	}
	return err
}

func (s *Service) finishURL(number string, app bool) string {
	if app && s.opts.AppFinishURL != "" {
		return strings.ReplaceAll(s.opts.AppFinishURL, "{order_number}", url.QueryEscape(number))
	}
	if s.opts.BaseURL == "" {
		return ""
	}
	return s.opts.BaseURL + "/account/orders/" + url.PathEscape(number) + "/payment/finish"
}

// lineItems builds the provider checkout lines from the order snapshot and
// checks they add up to orders.total, the only amount ever charged.
func lineItems(ctx context.Context, tx *Repository, o orderRow) ([]LineItem, error) {
	rows, err := tx.Items(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	var sum int64
	items := make([]LineItem, 0, len(rows)+2)
	for _, it := range rows {
		name := it.ProductName
		if it.VariantLabel != "" {
			name += " - " + it.VariantLabel
		}
		items = append(items, LineItem{ID: it.Sku, Name: name, Price: it.UnitPrice, Quantity: int(it.Quantity)})
		sum += it.Subtotal
	}
	if o.ShippingCost > 0 {
		items = append(items, LineItem{ID: "SHIPPING", Name: "Ongkos Kirim", Price: o.ShippingCost, Quantity: 1})
	}
	if o.Discount > 0 {
		items = append(items, LineItem{ID: "DISCOUNT", Name: "Diskon", Price: -o.Discount, Quantity: 1})
	}
	if len(rows) == 0 || o.Total <= 0 || sum != o.Subtotal || o.Subtotal+o.ShippingCost-o.Discount != o.Total {
		return nil, ErrInvalidAmount
	}
	return items, nil
}

// errorCategory names a provider error for the audit trail without its
// raw message.
func errorCategory(err error) string {
	switch {
	case errors.Is(err, ErrProviderRejected):
		return "provider rejected the request"
	case errors.Is(err, context.DeadlineExceeded):
		return "provider timeout"
	}
	return "provider unavailable"
}
