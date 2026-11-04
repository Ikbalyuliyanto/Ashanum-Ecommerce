package returns

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/inventory"
	"ecommerce/internal/media"
	"ecommerce/internal/validate"
)

const orderCompleted = "COMPLETED"

type Options struct {
	// WindowDays is the return policy: days after completion during which a
	// return may be requested (RETURN_WINDOW_DAYS).
	WindowDays int
	// Now is the clock (tests); time.Now when nil.
	Now func() time.Time
}

type Service struct {
	repo   *Repository
	window time.Duration
	now    func() time.Time
}

func NewService(repo *Repository, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.WindowDays < 1 {
		opts.WindowDays = 7
	}
	return &Service{repo: repo, window: time.Duration(opts.WindowDays) * 24 * time.Hour, now: opts.Now}
}

// WindowDays is the return window in days (the configured policy).
func (s *Service) WindowDays() int { return int(s.window / (24 * time.Hour)) }

// eligible checks the order-level rules; the server clock decides the
// window, never a date from the browser.
func (s *Service) eligible(o orderRow) error {
	switch {
	case o.Status != orderCompleted:
		return ErrOrderNotCompleted
	case !o.Paid:
		return ErrOrderNotPaid
	case o.CompletedAt == nil || !s.now().Before(o.CompletedAt.Add(s.window)):
		return ErrWindowClosed
	}
	return nil
}

// ForOrder tells whether a return may be requested for the user's order and
// lists the order's returns (loads the order itself; see Info).
func (s *Service) ForOrder(ctx context.Context, userID int64, number string) (OrderInfo, error) {
	o, err := s.repo.Order(ctx, userID, number)
	if err != nil {
		return OrderInfo{}, err
	}
	items, err := s.repo.Items(ctx, o.ID)
	if err != nil {
		return OrderInfo{}, err
	}
	returnable := false
	for _, it := range items {
		returnable = returnable || it.Remaining() > 0
	}
	return s.Info(ctx, userID, OrderFacts{ID: o.ID, Status: o.Status, Paid: o.Paid, CompletedAt: o.CompletedAt, Returnable: returnable})
}

// Info decides return eligibility from facts the order page already loaded
// and lists the order's returns: one query.
func (s *Service) Info(ctx context.Context, userID int64, f OrderFacts) (OrderInfo, error) {
	var info OrderInfo
	var err error
	if info.Returns, err = s.repo.OrderReturns(ctx, userID, f.ID); err != nil {
		return info, err
	}
	if f.CompletedAt != nil {
		d := f.CompletedAt.Add(s.window)
		info.Deadline = &d
	}
	info.Eligible = f.Returnable && s.eligible(orderRow{Status: f.Status, Paid: f.Paid, CompletedAt: f.CompletedAt}) == nil
	return info, nil
}

// Draft returns the return form of the user's order, or why no return can
// be requested.
func (s *Service) Draft(ctx context.Context, userID int64, number string) (Draft, error) {
	o, err := s.repo.Order(ctx, userID, number)
	if err != nil {
		return Draft{}, err
	}
	if err := s.eligible(o); err != nil {
		return Draft{}, err
	}
	items, err := s.repo.Items(ctx, o.ID)
	if err != nil {
		return Draft{}, err
	}
	d := Draft{OrderNumber: o.Number, Deadline: o.CompletedAt.Add(s.window)}
	for _, it := range items {
		if it.Remaining() > 0 {
			d.Items = append(d.Items, it)
		}
	}
	if len(d.Items) == 0 {
		return Draft{}, ErrNothingToReturn
	}
	return d, nil
}

// Create submits a return request in one transaction:
//
//	lock order → owner, COMPLETED, PAID, within window
//	→ lock chosen order items (id order) → quantities against earlier
//	returns → create return + items + evidence → refund amount from the
//	order_items snapshot prices
//
// The same idempotency key returns the request created first.
func (s *Service) Create(ctx context.Context, userID int64, req Request) (string, error) {
	evidence, err := checkRequest(&req)
	if err != nil {
		return "", err
	}
	if req.IdempotencyKey != "" {
		if n, err := s.repo.NumberByKey(ctx, userID, req.IdempotencyKey); err == nil {
			return n, nil
		}
	}

	var number string
	err = s.repo.InTx(ctx, func(tx *Repository) error {
		o, err := tx.Order(ctx, userID, req.OrderNumber)
		if err != nil {
			return err
		}
		if err := s.eligible(o); err != nil {
			return err
		}
		ids := make([]int64, len(req.Items))
		for i, it := range req.Items {
			ids[i] = it.OrderItemID
		}
		if err := tx.LockItems(ctx, o.ID, ids); err != nil {
			return err
		}
		// Read after the locks: earlier returns of this order are committed.
		items, err := tx.Items(ctx, o.ID)
		if err != nil {
			return err
		}
		lines, amount, err := quantities(req, items)
		if err != nil {
			return err
		}
		id, n, err := tx.Create(ctx, newReturn{OrderID: o.ID, UserID: userID, Type: req.Type, Reason: req.Reason,
			Description: req.Description, Amount: amount, Key: req.IdempotencyKey, Items: lines, Evidence: evidence})
		if err != nil {
			return err
		}
		number = n
		return tx.Event(ctx, id, "", StatusRequested, ActorUser, "")
	})
	if errors.Is(err, errDuplicateKey) {
		return s.repo.NumberByKey(ctx, userID, req.IdempotencyKey)
	}
	return number, err
}

// checkRequest validates what does not need the database and parses the
// evidence links.
func checkRequest(req *Request) ([]media.Attachment, error) {
	e := validate.Errors{}
	if len(req.Items) == 0 {
		e.Add("items", "Pilih minimal satu barang yang ingin dikembalikan.")
	}
	seen := map[int64]bool{}
	for _, it := range req.Items {
		if it.OrderItemID <= 0 || seen[it.OrderItemID] {
			e.Add("items", "Pilihan barang tidak valid.")
		}
		seen[it.OrderItemID] = true
	}
	if !req.Type.Valid() {
		e.Add("type", "Pilih Pengembalian Dana atau Tukar Barang.")
	}
	if !req.Reason.Valid() {
		e.Add("reason", "Pilih alasan pengembalian.")
	}
	req.Description = strings.TrimSpace(strings.ReplaceAll(req.Description, "\r\n", "\n"))
	min := 0
	if req.Reason == ReasonOther {
		min = 1
	}
	validate.Length(e, "description", req.Description, "Deskripsi", min, MaxDescription)
	if len(req.IdempotencyKey) > maxKeyLength {
		req.IdempotencyKey = ""
	}

	var evidence []media.Attachment
	links := 0
	for _, raw := range req.Evidence {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if links++; links > MaxEvidence {
			e.Add("evidence", "Maksimal "+strconv.Itoa(MaxEvidence)+" tautan bukti.")
			break
		}
		a, ok := media.ParseAttachment(raw)
		if !ok {
			e.Add("evidence", "Tautan bukti ke-"+strconv.Itoa(links)+" harus berupa alamat https yang valid.")
			continue
		}
		evidence = append(evidence, a)
	}
	return evidence, e.Err()
}

// quantities checks each requested line against what is still returnable
// and computes the refund amount from the prices paid (order_items).
func quantities(req Request, items []Item) ([]ReturnItem, int64, error) {
	byID := make(map[int64]Item, len(items))
	for _, it := range items {
		byID[it.OrderItemID] = it
	}
	e := validate.Errors{}
	var lines []ReturnItem
	var amount int64
	for _, r := range req.Items {
		it, ok := byID[r.OrderItemID]
		switch {
		case !ok:
			e.Add("items", "Barang yang dipilih bukan bagian dari pesanan ini.")
			continue
		case it.Remaining() == 0:
			e.Add("items", fmt.Sprintf("%s sudah diajukan pengembalian seluruhnya.", it.ProductName))
			continue
		case r.Quantity < 1 || r.Quantity > it.Remaining():
			e.Add("items", fmt.Sprintf("Jumlah %s yang bisa dikembalikan 1–%d.", it.ProductName, it.Remaining()))
			continue
		}
		lines = append(lines, ReturnItem{OrderItemID: it.OrderItemID, ProductName: it.ProductName, Quantity: r.Quantity, UnitPrice: it.UnitPrice})
		amount += it.UnitPrice * int64(r.Quantity)
	}
	if err := e.Err(); err != nil {
		return nil, 0, err
	}
	if req.Type == TypeExchange {
		amount = 0 // an exchange sends goods back, not money
	}
	return lines, amount, nil
}

// List returns one page (1-based) of the user's returns, newest first.
func (s *Service) List(ctx context.Context, userID int64, page int) (Page, error) {
	page = min(max(page, 1), 500)
	rows, err := s.repo.List(ctx, userID, PageSize+1, (page-1)*PageSize)
	if err != nil {
		return Page{}, err
	}
	p := Page{Page: page}
	if len(rows) > PageSize {
		rows, p.HasNext = rows[:PageSize], true
	}
	p.Returns = rows
	return p, nil
}

// Get returns one of the user's returns; another user's is ErrNotFound.
func (s *Service) Get(ctx context.Context, userID int64, number string) (Return, error) {
	if number == "" || len(number) > 40 {
		return Return{}, ErrNotFound
	}
	return s.repo.Get(ctx, userID, number)
}

// CancelByUser withdraws the user's own request while it still waits for
// review (REQUESTED); its quantity becomes returnable again.
func (s *Service) CancelByUser(ctx context.Context, userID int64, number string) error {
	return s.repo.InTx(ctx, func(tx *Repository) error {
		r, err := tx.LockOwn(ctx, userID, number)
		if err != nil {
			return err
		}
		if r.Status == StatusCancelled {
			return nil
		}
		if r.Status != StatusRequested {
			return ErrInvalidTransition
		}
		ok, err := tx.SetStatus(ctx, r.ID, r.Status, StatusCancelled, "")
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidTransition
		}
		return tx.Event(ctx, r.ID, r.Status, StatusCancelled, ActorUser, "Dibatalkan oleh pembeli")
	})
}

// ---------------------------------------------------------------- admin
//
// Every admin step runs in one transaction that locks the return row first
// (then its lines, then payments or variants), checks the state machine in
// policy.go, records the timeline event and the admin audit entry. Handlers
// never decide a status themselves.

// IDByNumber resolves a return number (admin routes).
func (s *Service) IDByNumber(ctx context.Context, number string) (int64, error) {
	if number == "" || len(number) > 40 {
		return 0, ErrNotFound
	}
	return s.repo.IDByNumber(ctx, number)
}

// AdminList returns one page of every return, newest first.
func (s *Service) AdminList(ctx context.Context, f AdminFilter) (AdminPage, error) {
	f = f.normalized()
	tab := AdminTabByKey(f.Tab)
	statuses := make([]string, len(tab.Statuses))
	for i, st := range tab.Statuses {
		statuses[i] = string(st)
	}
	rows, err := s.repo.AdminList(ctx, statuses, database.Like(f.Query), f.PageSize+1, (f.Page-1)*f.PageSize)
	if err != nil {
		return AdminPage{}, err
	}
	p := AdminPage{Filter: f}
	if len(rows) > f.PageSize {
		rows, p.HasNext = rows[:f.PageSize], true
	}
	p.Returns = rows
	return p, nil
}

// AdminRefunds returns one page of every refund, newest first.
func (s *Service) AdminRefunds(ctx context.Context, f AdminFilter) (AdminPage, error) {
	f = f.normalized()
	tab := RefundTabByKey(f.Tab)
	statuses := make([]string, len(tab.Statuses))
	for i, st := range tab.Statuses {
		statuses[i] = string(st)
	}
	rows, err := s.repo.AdminRefunds(ctx, statuses, database.Like(f.Query), f.PageSize+1, (f.Page-1)*f.PageSize)
	if err != nil {
		return AdminPage{}, err
	}
	p := AdminPage{Filter: f}
	if len(rows) > f.PageSize {
		rows, p.HasNext = rows[:f.PageSize], true
	}
	p.Refunds = rows
	return p, nil
}

func (f AdminFilter) normalized() AdminFilter {
	f.Page = min(max(f.Page, 1), 1000)
	if f.PageSize <= 0 {
		f.PageSize = AdminPageSize
	}
	f.PageSize = min(f.PageSize, MaxAdminPageSize)
	f.Query = validate.SearchText(f.Query)
	return f
}

// AdminGet loads any return with the buyer's contact, every refund and —
// while an exchange's goods are with the shop — the replacement choices.
func (s *Service) AdminGet(ctx context.Context, number string) (Return, error) {
	if number == "" || len(number) > 40 {
		return Return{}, ErrNotFound
	}
	r, err := s.repo.AdminGet(ctx, number)
	if err != nil || r.Type != TypeExchange || r.Status != StatusItemReceived {
		return r, err
	}
	var ids []int64
	for _, it := range r.Items {
		if it.ProductID != 0 && !slices.Contains(ids, it.ProductID) {
			ids = append(ids, it.ProductID)
		}
	}
	if len(ids) > 0 {
		r.Replacements, err = s.repo.Replacements(ctx, ids)
	}
	return r, err
}

// AdminRefund loads one refund with its payment reference.
func (s *Service) AdminRefund(ctx context.Context, id int64) (Refund, error) {
	return s.repo.AdminRefund(ctx, id)
}

// Approve accepts a REQUESTED return, all or in part: each line gets an
// approved quantity 0..requested (stored explicitly) and the refund amount
// becomes the approved units × the price paid (0 for an exchange). At least
// one unit must be approved; to accept nothing, Reject the request (the
// buyer then sees the reason). Units not approved can be requested again.
func (s *Service) Approve(ctx context.Context, adminID, returnID int64, in ApproveInput) error {
	note := strings.TrimSpace(in.Note)
	if utf8Len(note) > 500 {
		return validate.Errors{"note": "Catatan maksimal 500 karakter."}
	}
	return s.repo.InTx(ctx, func(tx *Repository) error {
		r, err := tx.Lock(ctx, returnID)
		if err != nil {
			return err
		}
		if !allowed(r.Type, r.Status, StatusApproved) {
			return fmt.Errorf("%w: %s → APPROVED", ErrInvalidTransition, r.Status)
		}
		lines, err := tx.Lines(ctx, r.ID)
		if err != nil {
			return err
		}
		ids := make([]int64, len(lines))
		qty := make([]int32, len(lines))
		var amount int64
		approved, requested := 0, 0
		e := validate.Errors{}
		for i, l := range lines {
			n, ok := in.Quantities[l.ID]
			if !ok || n < 0 || n > l.Quantity {
				e.Add(fmt.Sprintf("qty_%d", l.ID), fmt.Sprintf("Jumlah disetujui untuk %s harus 0–%d.", l.ProductName, l.Quantity))
				continue
			}
			ids[i], qty[i] = l.ID, int32(n)
			amount += l.UnitPrice * int64(n)
			approved += n
			requested += l.Quantity
		}
		if err := e.Err(); err != nil {
			return err
		}
		if approved == 0 {
			return validate.Errors{"items": "Setujui minimal satu barang. Untuk menolak seluruh pengajuan, gunakan Tolak beserta alasannya."}
		}
		if r.Type == TypeExchange {
			amount = 0
		}
		if err := tx.SetApproved(ctx, r.ID, ids, qty); err != nil {
			return err
		}
		if err := tx.SetAmount(ctx, r.ID, amount); err != nil {
			return err
		}
		event := note
		if approved < requested {
			event = strings.TrimSpace(fmt.Sprintf("Disetujui sebagian: %d dari %d barang. %s", approved, requested, note))
		}
		if err := move(ctx, tx, r, StatusApproved, ActorAdmin, event); err != nil {
			return err
		}
		return s.audit(ctx, tx, adminID, "return.approve", r, map[string]any{
			"approved": approved, "requested": requested, "refund_amount": amount, "note": note})
	})
}

// Reject refuses a REQUESTED return; the note (shown to the buyer) is
// required. The quantity becomes returnable again.
func (s *Service) Reject(ctx context.Context, adminID, returnID int64, note string) error {
	if err := requireNote(note, "Alasan penolakan"); err != nil {
		return err
	}
	return s.transition(ctx, adminID, returnID, StatusRejected, note, nil)
}

// MarkWaitingReturn: approved, waiting for the buyer to send the goods back.
func (s *Service) MarkWaitingReturn(ctx context.Context, adminID, returnID int64, note string) error {
	return s.transition(ctx, adminID, returnID, StatusWaitingReturn, note, nil)
}

// MarkItemReceived records that the shop received the goods and its
// inspection: a condition for every approved line. Stock does not change
// here — restocking restockable goods is a separate, explicit step.
func (s *Service) MarkItemReceived(ctx context.Context, adminID, returnID int64, in ReceiveInput) error {
	return s.transition(ctx, adminID, returnID, StatusItemReceived, in.Note, func(tx *Repository, r locked) (map[string]any, error) {
		lines, err := tx.Lines(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		var ids []int64
		var conds []string
		counts := map[string]int{}
		e := validate.Errors{}
		for _, l := range lines {
			if l.approved() == 0 {
				continue // nothing of this line comes back
			}
			c := in.Conditions[l.ID]
			if !c.Valid() {
				e.Add(fmt.Sprintf("condition_%d", l.ID), "Pilih kondisi barang "+l.ProductName+".")
				continue
			}
			ids, conds = append(ids, l.ID), append(conds, string(c))
			counts[string(c)]++
		}
		if err := e.Err(); err != nil {
			return nil, err
		}
		return map[string]any{"conditions": counts}, tx.Inspect(ctx, r.ID, ids, conds)
	})
}

// Restock puts the approved units of inspected RESTOCKABLE lines back on
// sale, once per line (restocked_at): a repeated request finds nothing left
// and returns ErrNothingToRestock. Damaged and non-restockable goods never
// go back. Each line writes a RETURN_RESTOCK inventory adjustment. Returns
// the number of units restocked.
func (s *Service) Restock(ctx context.Context, adminID, returnID int64) (int, error) {
	units := 0
	err := s.repo.InTx(ctx, func(tx *Repository) error {
		r, err := tx.Lock(ctx, returnID)
		if err != nil {
			return err
		}
		switch r.Status {
		case StatusItemReceived, StatusRefundPending, StatusRefunded, StatusExchanged, StatusCompleted:
		default:
			return fmt.Errorf("%w: restock in %s", ErrInvalidTransition, r.Status)
		}
		lines, err := tx.Lines(ctx, r.ID)
		if err != nil {
			return err
		}
		// Variants in id order, as checkout and cancellation lock them.
		slices.SortFunc(lines, func(a, b line) int { return cmp.Compare(a.VariantID, b.VariantID) })
		var skus []string
		for _, l := range lines {
			if l.Condition != ConditionRestockable || l.Restocked || l.approved() == 0 || l.VariantID == 0 {
				continue
			}
			ok, err := tx.MarkRestocked(ctx, l.ID, l.approved())
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if _, err := inventory.Give(ctx, tx.q, inventory.Change{VariantID: l.VariantID, Type: inventory.TypeReturnRestock,
				Reason: "Restock " + r.Number, RefType: inventory.RefReturn, RefID: r.ID, AdminID: adminID}, l.approved()); err != nil {
				return err
			}
			units += l.approved()
			skus = append(skus, l.SKU)
		}
		if units == 0 {
			return ErrNothingToRestock
		}
		return s.audit(ctx, tx, adminID, "return.restock", r, map[string]any{"units": units, "skus": skus})
	})
	if err != nil {
		return 0, err
	}
	return units, nil
}

// PrepareRefund creates the PENDING refund of a refund return whose goods
// were received (or whose earlier refund failed), for the order's PAID
// payment. The amount is the approved refund amount; together with every
// other refund of the payment it never exceeds what was paid (the payment
// row is locked to serialize this). No provider is called.
func (s *Service) PrepareRefund(ctx context.Context, adminID, returnID int64) (Refund, error) {
	var refund Refund
	err := s.repo.InTx(ctx, func(tx *Repository) error {
		r, err := tx.Lock(ctx, returnID)
		if err != nil {
			return err
		}
		retry := r.Type == TypeRefund && r.Status == StatusRefundPending
		if !retry && !allowed(r.Type, r.Status, StatusRefundPending) {
			return fmt.Errorf("%w: %s → REFUND_PENDING", ErrInvalidTransition, r.Status)
		}
		if retry {
			active, err := tx.HasActiveRefund(ctx, r.ID)
			if err != nil {
				return err
			}
			if active {
				return ErrRefundOpen
			}
		}
		if r.PaymentID == nil {
			return ErrNoPayment
		}
		paid, err := tx.LockPayment(ctx, *r.PaymentID)
		if err != nil {
			return err
		}
		done, err := tx.RefundedForPayment(ctx, *r.PaymentID)
		if err != nil {
			return err
		}
		if r.Amount <= 0 || done+r.Amount > paid {
			return ErrRefundTooLarge
		}
		if refund, err = tx.CreateRefund(ctx, r.ID, r.OrderID, r.PaymentID, r.Amount); err != nil {
			return err
		}
		if !retry {
			if err := move(ctx, tx, r, StatusRefundPending, ActorAdmin, ""); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, adminID, "return.prepare_refund", r, map[string]any{
			"refund_id": refund.ID, "amount": r.Amount, "retry": retry})
	})
	return refund, err
}

// CompleteRefund records that the return's open refund was paid out
// outside this system (a reference is required) and marks the return
// REFUNDED. It calls no payment provider.
func (s *Service) CompleteRefund(ctx context.Context, adminID, returnID int64, res RefundResult) error {
	res, err := checkResult(res)
	if err != nil {
		return err
	}
	return s.transition(ctx, adminID, returnID, StatusRefunded, res.Note, func(tx *Repository, r locked) (map[string]any, error) {
		id, err := tx.OpenRefund(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		ok, err := tx.RecordRefund(ctx, id, res)
		if err == nil && !ok {
			err = ErrInvalidTransition
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"refund_id": id, "method": res.Provider, "reference": res.Reference},
			audit.Record(ctx, tx.q, audit.Entry{AdminID: adminID, Action: "refund.succeeded", EntityType: audit.EntityRefund,
				EntityID: id, Label: r.Number, Meta: map[string]any{"method": res.Provider, "reference": res.Reference, "note": res.Note}})
	})
}

// MarkRefundProcessing: the shop started paying the refund out.
func (s *Service) MarkRefundProcessing(ctx context.Context, adminID, refundID int64, note string) error {
	note = strings.TrimSpace(note)
	if utf8Len(note) > 500 {
		return validate.Errors{"note": "Catatan maksimal 500 karakter."}
	}
	return s.refundStep(ctx, adminID, refundID, func(tx *Repository, f lockedRefund) (string, map[string]any, error) {
		ok, err := tx.MarkRefundProcessing(ctx, f.ID, note)
		if err == nil && !ok {
			err = ErrInvalidTransition
		}
		return "refund.processing", map[string]any{"note": note}, err
	})
}

// FailRefund records that paying the refund out failed (reason required).
// A return's refund can then be prepared again.
func (s *Service) FailRefund(ctx context.Context, adminID, refundID int64, reason string) error {
	if err := requireNote(reason, "Alasan kegagalan"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	return s.refundStep(ctx, adminID, refundID, func(tx *Repository, f lockedRefund) (string, map[string]any, error) {
		ok, err := tx.FailRefund(ctx, f.ID, reason)
		if err == nil && !ok {
			err = ErrInvalidTransition
		}
		return "refund.failed", map[string]any{"reason": reason}, err
	})
}

// CompleteRefundByID records a refund (from a return or an order
// cancellation) as paid out manually. For a return refund the return moves
// to REFUNDED in the same transaction.
func (s *Service) CompleteRefundByID(ctx context.Context, adminID, refundID int64, res RefundResult) error {
	returnID, err := s.repo.RefundReturnID(ctx, refundID)
	if err != nil {
		return err
	}
	if returnID != 0 {
		return s.CompleteRefund(ctx, adminID, returnID, res)
	}
	if res, err = checkResult(res); err != nil {
		return err
	}
	return s.refundStep(ctx, adminID, refundID, func(tx *Repository, f lockedRefund) (string, map[string]any, error) {
		ok, err := tx.RecordRefund(ctx, f.ID, res)
		if err == nil && !ok {
			err = ErrInvalidTransition
		}
		return "refund.succeeded", map[string]any{"method": res.Provider, "reference": res.Reference, "note": res.Note}, err
	})
}

// refundStep locks a refund (after its return, when it has one, keeping the
// lock order return → refund) and runs fn with the audit entry it names.
func (s *Service) refundStep(ctx context.Context, adminID, refundID int64,
	fn func(tx *Repository, f lockedRefund) (string, map[string]any, error)) error {
	returnID, err := s.repo.RefundReturnID(ctx, refundID)
	if err != nil {
		return err
	}
	return s.repo.InTx(ctx, func(tx *Repository) error {
		label := ""
		if returnID != 0 {
			r, err := tx.Lock(ctx, returnID)
			if err != nil {
				return err
			}
			label = r.Number
		}
		f, err := tx.LockRefund(ctx, refundID)
		if err != nil {
			return err
		}
		action, meta, err := fn(tx, f)
		if err != nil {
			return err
		}
		meta["amount"] = f.Amount
		meta["source"] = string(f.Source)
		return audit.Record(ctx, tx.q, audit.Entry{AdminID: adminID, Action: action, EntityType: audit.EntityRefund,
			EntityID: f.ID, Label: label, Meta: meta})
	})
}

func checkResult(res RefundResult) (RefundResult, error) {
	res.Provider = strings.TrimSpace(res.Provider)
	res.Reference = validate.Text(res.Reference)
	res.Note = strings.TrimSpace(res.Note)
	e := validate.Errors{}
	if res.Provider == "" {
		res.Provider = "manual"
	}
	if len(res.Provider) > 30 {
		e.Add("provider", "Cara pengembalian dana terlalu panjang.")
	}
	validate.Length(e, "reference", res.Reference, "Referensi pengembalian dana", 3, 100)
	if utf8Len(res.Note) > 500 {
		e.Add("note", "Catatan maksimal 500 karakter.")
	}
	return res, e.Err()
}

// CompleteExchange records that the replacement goods are being sent: each
// approved line gets a replacement variant of the same product, whose stock
// is taken atomically (UPDATE … WHERE stock >= n) with an EXCHANGE_OUT
// adjustment. Too little stock refuses the whole step and changes nothing.
// The status guard makes a second completion fail without touching stock.
func (s *Service) CompleteExchange(ctx context.Context, adminID, returnID int64, in ExchangeInput) error {
	return s.transition(ctx, adminID, returnID, StatusExchanged, in.Note, func(tx *Repository, r locked) (map[string]any, error) {
		lines, err := tx.Lines(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		var products []int64
		for _, l := range lines {
			if l.ProductID != 0 && !slices.Contains(products, l.ProductID) {
				products = append(products, l.ProductID)
			}
		}
		options, err := tx.Replacements(ctx, products)
		if err != nil {
			return nil, err
		}
		type pick struct {
			l line
			v Replacement
		}
		var picks []pick
		e := validate.Errors{}
		for _, l := range lines {
			if l.approved() == 0 {
				continue
			}
			key := fmt.Sprintf("replacement_%d", l.ID)
			if l.ProductID == 0 {
				e.Add(key, l.ProductName+" sudah dihapus dari katalog; tidak ada varian pengganti.")
				continue
			}
			want := in.Replacements[l.ID]
			i := slices.IndexFunc(options, func(v Replacement) bool { return v.VariantID == want && v.ProductID == l.ProductID })
			if i < 0 {
				e.Add(key, "Pilih varian pengganti yang aktif untuk "+l.ProductName+".")
				continue
			}
			picks = append(picks, pick{l, options[i]})
		}
		if err := e.Err(); err != nil {
			return nil, err
		}
		slices.SortFunc(picks, func(a, b pick) int { return cmp.Compare(a.v.VariantID, b.v.VariantID) })
		var out []string
		for _, p := range picks {
			ok, err := tx.SetReplacement(ctx, p.l.ID, p.v, p.l.approved())
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, ErrInvalidTransition
			}
			_, err = inventory.Take(ctx, tx.q, inventory.Change{VariantID: p.v.VariantID, Type: inventory.TypeExchangeOut,
				Reason: "Pengganti " + r.Number, RefType: inventory.RefReturn, RefID: r.ID, AdminID: adminID}, p.l.approved())
			if errors.Is(err, inventory.ErrInsufficientStock) {
				return nil, validate.Errors{fmt.Sprintf("replacement_%d", p.l.ID): fmt.Sprintf(
					"Stok %s tidak cukup untuk %d barang pengganti.", p.v.SKU, p.l.approved())}
			}
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("%s×%d", p.v.SKU, p.l.approved()))
		}
		return map[string]any{"replacements": out}, nil
	})
}

// Complete closes a refunded or exchanged return.
func (s *Service) Complete(ctx context.Context, adminID, returnID int64, note string) error {
	return s.transition(ctx, adminID, returnID, StatusCompleted, note, nil)
}

// Cancel stops a return before the goods are back with the shop (reason
// required, shown to the buyer).
func (s *Service) Cancel(ctx context.Context, adminID, returnID int64, note string) error {
	if err := requireNote(note, "Alasan pembatalan"); err != nil {
		return err
	}
	return s.transition(ctx, adminID, returnID, StatusCancelled, note, nil)
}

var transitionAction = map[Status]string{
	StatusApproved:      "return.approve",
	StatusRejected:      "return.reject",
	StatusWaitingReturn: "return.waiting_return",
	StatusItemReceived:  "return.receive",
	StatusRefundPending: "return.prepare_refund",
	StatusRefunded:      "return.refunded",
	StatusExchanged:     "return.complete_exchange",
	StatusCompleted:     "return.complete",
	StatusCancelled:     "return.cancel",
}

// transition locks the return, checks the state machine, runs extra (in the
// same transaction), records the change in the timeline and audits it.
func (s *Service) transition(ctx context.Context, adminID, returnID int64, to Status, note string,
	extra func(*Repository, locked) (map[string]any, error)) error {
	note = strings.TrimSpace(note)
	if utf8Len(note) > 500 {
		return validate.Errors{"note": "Catatan maksimal 500 karakter."}
	}
	return s.repo.InTx(ctx, func(tx *Repository) error {
		r, err := tx.Lock(ctx, returnID)
		if err != nil {
			return err
		}
		if !allowed(r.Type, r.Status, to) {
			return fmt.Errorf("%w: %s %s → %s", ErrInvalidTransition, r.Type, r.Status, to)
		}
		meta := map[string]any{}
		if extra != nil {
			m, err := extra(tx, r)
			if err != nil {
				return err
			}
			for k, v := range m {
				meta[k] = v
			}
		}
		if err := move(ctx, tx, r, to, ActorAdmin, note); err != nil {
			return err
		}
		meta["from"], meta["to"] = string(r.Status), string(to)
		if note != "" {
			meta["note"] = note
		}
		return s.audit(ctx, tx, adminID, transitionAction[to], r, meta)
	})
}

func (s *Service) audit(ctx context.Context, tx *Repository, adminID int64, action string, r locked, meta map[string]any) error {
	return audit.Record(ctx, tx.q, audit.Entry{AdminID: adminID, Action: action, EntityType: audit.EntityReturn,
		EntityID: r.ID, Label: r.Number, Meta: meta})
}

func move(ctx context.Context, tx *Repository, r locked, to Status, actor Actor, note string) error {
	ok, err := tx.SetStatus(ctx, r.ID, r.Status, to, note)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidTransition
	}
	return tx.Event(ctx, r.ID, r.Status, to, actor, note)
}

func requireNote(note, label string) error {
	e := validate.Errors{}
	validate.Length(e, "note", strings.TrimSpace(note), label, 3, 500)
	if len(e) > 0 && strings.TrimSpace(note) == "" {
		return validate.Errors{"note": label + " wajib diisi."}
	}
	return e.Err()
}

func utf8Len(s string) int { return len([]rune(s)) }
