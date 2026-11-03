package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	tx   bool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: dbgen.New(pool)}
}

// InTx runs fn with a repository bound to one transaction; it commits when
// fn returns nil and rolls back otherwise. Inside it, Order row-locks.
func (r *Repository) InTx(ctx context.Context, fn func(tx *Repository) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(&Repository{q: r.q.WithTx(tx), tx: true})
	})
}

// orderRow is the part of an order payments need.
type orderRow struct {
	ID            int64
	Number        string
	Status        string
	Subtotal      int64
	ShippingCost  int64
	Discount      int64
	Total         int64
	DueAt         time.Time
	StockRestored bool
	Recipient     string
	Phone         string
	Email         string
}

// OrderID returns the id of the user's order; other users' orders are
// ErrNotFound.
func (r *Repository) OrderID(ctx context.Context, userID int64, number string) (int64, error) {
	id, err := r.q.PaymentOrderID(ctx, dbgen.PaymentOrderIDParams{OrderNumber: number, UserID: userID})
	if database.IsNoRows(err) {
		return 0, ErrNotFound
	}
	return id, err
}

// Order loads an order; inside a transaction the order row is locked.
func (r *Repository) Order(ctx context.Context, id int64) (orderRow, error) {
	var row dbgen.PaymentOrderRow
	var err error
	if r.tx {
		var locked dbgen.LockPaymentOrderRow
		locked, err = r.q.LockPaymentOrder(ctx, id)
		row = dbgen.PaymentOrderRow(locked)
	} else {
		row, err = r.q.PaymentOrder(ctx, id)
	}
	if database.IsNoRows(err) {
		return orderRow{}, ErrNotFound
	}
	if err != nil {
		return orderRow{}, err
	}
	return orderRow{
		ID: row.ID, Number: row.OrderNumber, Status: row.Status,
		Subtotal: row.Subtotal, ShippingCost: row.ShippingCost, Discount: row.Discount, Total: row.Total,
		DueAt: row.PaymentDueAt, StockRestored: row.StockRestoredAt != nil,
		Recipient: row.RecipientName, Phone: row.RecipientPhone, Email: row.Email,
	}, nil
}

func (r *Repository) Items(ctx context.Context, orderID int64) ([]dbgen.PaymentOrderItemsRow, error) {
	return r.q.PaymentOrderItems(ctx, orderID)
}

func (r *Repository) Attempts(ctx context.Context, orderID int64) ([]Attempt, error) {
	rows, err := r.q.ListOrderPayments(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Attempt, len(rows))
	for i, row := range rows {
		out[i] = attemptFrom(row)
	}
	return out, nil
}

// Pending returns the order's pending attempt, or nil.
func (r *Repository) Pending(ctx context.Context, orderID int64) (*Attempt, error) {
	row, err := r.q.PendingPayment(ctx, orderID)
	if database.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := attemptFrom(row)
	return &a, nil
}

// LockAttempt row-locks an attempt (lock its order first).
func (r *Repository) LockAttempt(ctx context.Context, id int64) (Attempt, error) {
	row, err := r.q.LockPayment(ctx, id)
	if err != nil {
		return Attempt{}, err
	}
	return attemptFrom(row), nil
}

// Ref resolves a provider order ID to its attempt and order.
func (r *Repository) Ref(ctx context.Context, providerOrderID string) (paymentID, orderID int64, err error) {
	row, err := r.q.PaymentRef(ctx, providerOrderID)
	if database.IsNoRows(err) {
		return 0, 0, ErrUnknownPayment
	}
	return row.ID, row.OrderID, err
}

func (r *Repository) HasPaid(ctx context.Context, orderID int64) (bool, error) {
	return r.q.HasPaidPayment(ctx, orderID)
}

func (r *Repository) NextAttemptNo(ctx context.Context, orderID int64) (int, error) {
	n, err := r.q.NextAttemptNo(ctx, orderID)
	return int(n), err
}

func (r *Repository) CreateAttempt(ctx context.Context, orderID int64, n int, provider, providerOrderID string, amount int64, expires time.Time) (Attempt, error) {
	row, err := r.q.CreatePayment(ctx, dbgen.CreatePaymentParams{
		OrderID: orderID, AttemptNo: int32(n), Provider: provider,
		ProviderOrderID: providerOrderID, Amount: amount, ExpiresAt: expires,
	})
	if err != nil {
		return Attempt{}, err
	}
	return attemptFrom(row), nil
}

// SetCheckout stores the provider checkout of a pending attempt, once.
func (r *Repository) SetCheckout(ctx context.Context, id int64, c Checkout) (bool, error) {
	n, err := r.q.SetPaymentCheckout(ctx, dbgen.SetPaymentCheckoutParams{
		ID: id, SnapToken: &c.Token, SnapRedirectUrl: &c.RedirectURL,
	})
	return n == 1, err
}

// RecordProvider keeps the latest provider details of an attempt.
func (r *Repository) RecordProvider(ctx context.Context, id int64, st ProviderStatus) error {
	return r.q.RecordProviderStatus(ctx, dbgen.RecordProviderStatusParams{
		ID:                    id,
		ProviderStatus:        &st.Raw,
		FraudStatus:           nonEmpty(st.Fraud),
		PaymentType:           nonEmpty(st.PaymentType),
		ProviderTransactionID: nonEmpty(st.TransactionID),
	})
}

// SetStatus moves a PENDING attempt to status (paidAt only for PAID). It
// reports false when the attempt was no longer pending.
func (r *Repository) SetStatus(ctx context.Context, id int64, status Status, paidAt *time.Time) (bool, error) {
	n, err := r.q.SetPaymentStatus(ctx, dbgen.SetPaymentStatusParams{ID: id, Status: string(status), PaidAt: paidAt})
	return n == 1, err
}

func (r *Repository) Flag(ctx context.Context, id int64, reason string) error {
	return r.q.FlagPayment(ctx, dbgen.FlagPaymentParams{ID: id, ReviewReason: &reason})
}

func (r *Repository) MarkOrderPaid(ctx context.Context, orderID int64, paidAt time.Time) (bool, error) {
	n, err := r.q.MarkOrderPaid(ctx, dbgen.MarkOrderPaidParams{ID: orderID, PaidAt: &paidAt})
	return n == 1, err
}

type restock struct {
	Variants int // variants that got stock back
	Missing  int // order lines whose variant no longer exists
}

// CancelAndRestore cancels an unpaid order, gives its stock back and closes
// its pending attempt as closeAs. The order row must already be locked;
// variants are locked here in id order (as checkout does). The order
// update is guarded by stock_restored_at, so stock is returned once only.
func (r *Repository) CancelAndRestore(ctx context.Context, orderID int64, reason string, closeAs Status) (restock, error) {
	if !r.tx {
		return restock{}, errors.New("payment: CancelAndRestore needs a transaction")
	}
	if _, err := r.q.LockOrderVariants(ctx, orderID); err != nil {
		return restock{}, err
	}
	res, err := r.q.RestoreOrderStock(ctx, orderID)
	if err != nil {
		return restock{}, err
	}
	if res.Restored != res.Variants {
		return restock{}, fmt.Errorf("payment: restored %d of %d variants", res.Restored, res.Variants)
	}
	n, err := r.q.CancelUnpaidOrder(ctx, dbgen.CancelUnpaidOrderParams{ID: orderID, CancelReason: &reason})
	if err != nil {
		return restock{}, err
	}
	if n != 1 {
		return restock{}, errors.New("payment: order is no longer cancellable")
	}
	if _, err := r.q.ClosePendingPayments(ctx, dbgen.ClosePendingPaymentsParams{OrderID: orderID, Status: string(closeAs)}); err != nil {
		return restock{}, err
	}
	return restock{Variants: int(res.Restored), Missing: int(res.Missing)}, nil
}

// event is one payment_events row.
type event struct {
	OrderID   int64
	PaymentID int64 // 0 for order-level events
	Source    string
	Kind      string
	From, To  Status
	Raw       string
	Detail    string
}

func (r *Repository) Event(ctx context.Context, e event) error {
	p := dbgen.InsertPaymentEventParams{
		OrderID: e.OrderID, Source: e.Source, Kind: e.Kind, Detail: e.Detail,
		FromStatus: nonEmpty(string(e.From)), ToStatus: nonEmpty(string(e.To)), ProviderStatus: nonEmpty(e.Raw),
	}
	if e.PaymentID != 0 {
		p.PaymentID = &e.PaymentID
	}
	return r.q.InsertPaymentEvent(ctx, p)
}

func (r *Repository) StalePending(ctx context.Context, before time.Time, limit int) ([]dbgen.StalePendingPaymentsRow, error) {
	return r.q.StalePendingPayments(ctx, dbgen.StalePendingPaymentsParams{CreatedBefore: before, RowLimit: int32(limit)})
}

func (r *Repository) Overdue(ctx context.Context, now time.Time, limit int) ([]int64, error) {
	return r.q.OverdueOrders(ctx, dbgen.OverdueOrdersParams{DueBefore: now, RowLimit: int32(limit)})
}

func (r *Repository) NeedingReview(ctx context.Context, limit int) ([]ReviewItem, error) {
	rows, err := r.q.PaymentsNeedingReview(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]ReviewItem, len(rows))
	for i, row := range rows {
		out[i] = ReviewItem{
			OrderNumber: row.OrderNumber, ProviderOrderID: row.ProviderOrderID, Status: Status(row.Status),
			ProviderStatus: str(row.ProviderStatus), Reason: str(row.ReviewReason), UpdatedAt: row.UpdatedAt,
		}
	}
	return out, nil
}

func attemptFrom(p dbgen.Payment) Attempt {
	return Attempt{
		ID: p.ID, OrderID: p.OrderID, No: int(p.AttemptNo), ProviderOrderID: p.ProviderOrderID,
		Amount: p.Amount, Status: Status(p.Status), PaymentType: str(p.PaymentType),
		RedirectURL: str(p.SnapRedirectUrl), NeedsReview: p.ReviewReason != nil,
		ExpiresAt: p.ExpiresAt, PaidAt: p.PaidAt, CreatedAt: p.CreatedAt,
	}
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
