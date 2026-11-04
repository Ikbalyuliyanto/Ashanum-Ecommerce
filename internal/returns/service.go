package returns

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// ForOrder tells the order detail page whether a return may be requested
// and lists the order's returns.
func (s *Service) ForOrder(ctx context.Context, userID int64, number string) (OrderInfo, error) {
	o, err := s.repo.Order(ctx, userID, number)
	if err != nil {
		return OrderInfo{}, err
	}
	var info OrderInfo
	if info.Returns, err = s.repo.OrderReturns(ctx, userID, o.ID); err != nil {
		return info, err
	}
	if o.CompletedAt != nil {
		d := o.CompletedAt.Add(s.window)
		info.Deadline = &d
	}
	if s.eligible(o) != nil {
		return info, nil
	}
	items, err := s.repo.Items(ctx, o.ID)
	if err != nil {
		return info, err
	}
	for _, it := range items {
		if it.Remaining() > 0 {
			info.Eligible = true
			break
		}
	}
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

// --- Admin-ready transitions (Phase 8 admin calls these; no route yet). ---

// Approve accepts a REQUESTED return.
func (s *Service) Approve(ctx context.Context, returnID int64, note string) error {
	return s.transition(ctx, returnID, StatusApproved, note, nil)
}

// Reject refuses a REQUESTED return; the note (shown to the buyer) is required.
func (s *Service) Reject(ctx context.Context, returnID int64, note string) error {
	if strings.TrimSpace(note) == "" {
		return validate.Errors{"note": "Alasan penolakan wajib diisi."}
	}
	return s.transition(ctx, returnID, StatusRejected, note, nil)
}

// MarkWaitingReturn: approved, waiting for the buyer to send the goods back.
func (s *Service) MarkWaitingReturn(ctx context.Context, returnID int64, note string) error {
	return s.transition(ctx, returnID, StatusWaitingReturn, note, nil)
}

// MarkItemReceived: the shop received and checked the returned goods.
// Restocking is a separate admin decision (goods may be damaged).
func (s *Service) MarkItemReceived(ctx context.Context, returnID int64, note string) error {
	return s.transition(ctx, returnID, StatusItemReceived, note, nil)
}

// PrepareRefund creates the PENDING refund of a refund return whose goods
// were received, for the order's PAID payment. The amount is the return's
// server-computed refund amount and, with earlier refunds, never more than
// what was paid. No provider is called.
func (s *Service) PrepareRefund(ctx context.Context, returnID int64) (Refund, error) {
	var refund Refund
	err := s.transition(ctx, returnID, StatusRefundPending, "", func(tx *Repository, r locked) error {
		if r.PaymentID == nil {
			return ErrNoPayment
		}
		done, err := tx.RefundedForPayment(ctx, *r.PaymentID)
		if err != nil {
			return err
		}
		if r.Amount <= 0 || done+r.Amount > r.PaidAmount {
			return ErrRefundTooLarge
		}
		refund, err = tx.CreateRefund(ctx, r.ID, r.PaymentID, r.Amount)
		return err
	})
	return refund, err
}

// CompleteRefund records that the refund was paid out (outside this
// system) and marks the return REFUNDED. It calls no payment provider.
func (s *Service) CompleteRefund(ctx context.Context, returnID int64, res RefundResult) error {
	res.Provider = strings.TrimSpace(res.Provider)
	if res.Provider == "" || len(res.Provider) > 30 || len(res.Reference) > 100 {
		return validate.Errors{"provider": "Cara pengembalian dana wajib diisi."}
	}
	return s.transition(ctx, returnID, StatusRefunded, "", func(tx *Repository, r locked) error {
		id, err := tx.OpenRefund(ctx, r.ID)
		if err != nil {
			return err
		}
		ok, err := tx.RecordRefund(ctx, id, res)
		if err == nil && !ok {
			err = ErrInvalidTransition
		}
		return err
	})
}

// CompleteExchange: the replacement goods were sent (exchange returns).
func (s *Service) CompleteExchange(ctx context.Context, returnID int64, note string) error {
	return s.transition(ctx, returnID, StatusExchanged, note, nil)
}

// Complete closes a refunded or exchanged return.
func (s *Service) Complete(ctx context.Context, returnID int64, note string) error {
	return s.transition(ctx, returnID, StatusCompleted, note, nil)
}

// Cancel stops a return before the goods are back with the shop.
func (s *Service) Cancel(ctx context.Context, returnID int64, note string) error {
	return s.transition(ctx, returnID, StatusCancelled, note, nil)
}

// transition locks the return, checks the state machine, runs extra (in the
// same transaction) and records the change in the timeline.
func (s *Service) transition(ctx context.Context, returnID int64, to Status, note string, extra func(*Repository, locked) error) error {
	note = strings.TrimSpace(note)
	return s.repo.InTx(ctx, func(tx *Repository) error {
		r, err := tx.Lock(ctx, returnID)
		if err != nil {
			return err
		}
		if !allowed(r.Type, r.Status, to) {
			return fmt.Errorf("%w: %s %s → %s", ErrInvalidTransition, r.Type, r.Status, to)
		}
		if extra != nil {
			if err := extra(tx, r); err != nil {
				return err
			}
		}
		return move(ctx, tx, r, to, ActorAdmin, note)
	})
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
