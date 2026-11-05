// Package fulfillment is the shop's side of an order: the admin order list
// and detail, and the normal order flow after payment
//
//	WAITING_CONFIRMATION → PROCESSING → SHIPPED → COMPLETED
//
// plus admin cancellation. It never decides that an order is paid: only a
// payment the provider confirmed (package payment) moves an order out of
// WAITING_PAYMENT, and the database refuses PROCESSING/SHIPPED/COMPLETED
// without paid_at. Completing an order sets completed_at, which makes its
// items reviewable and starts the return window (packages review, returns).
//
// Shipping: the buyer chose a courier service at checkout (the order's
// shipping snapshot). The shop books it with the provider (package
// shipping: idempotent, never for an unpaid order) or, as a fallback, ships
// manually by typing courier, service and tracking number here. At most one
// shipment per order is active. The Service has no HTTP dependency.
package fulfillment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
	"ecommerce/internal/order"
	"ecommerce/internal/payment"
	"ecommerce/internal/returns"
	"ecommerce/internal/shipping"
	"ecommerce/internal/validate"
)

var (
	ErrNotFound          = errors.New("fulfillment: order not found")
	ErrNotPaid           = errors.New("fulfillment: order is not paid")
	ErrInvalidTransition = errors.New("fulfillment: status change not allowed")
	ErrNotCancellable    = errors.New("fulfillment: order can no longer be cancelled")
	// ErrShipmentActive: the order already has an active shipment (booked
	// with the provider); cancel that first.
	ErrShipmentActive = errors.New("fulfillment: order has an active shipment")
)

// Message is the admin-facing text for a known error, or "".
func Message(err error) string {
	switch {
	case errors.Is(err, ErrNotPaid):
		return "Pesanan belum dibayar: hanya pembayaran yang dikonfirmasi Midtrans yang bisa memproses pesanan."
	case errors.Is(err, ErrInvalidTransition):
		return "Status pesanan sudah berubah; langkah ini tidak bisa dilakukan lagi."
	case errors.Is(err, ErrNotCancellable):
		return "Pesanan yang sudah dikirim atau selesai tidak bisa dibatalkan. Gunakan pengembalian barang."
	case errors.Is(err, payment.ErrCancelUnavailable):
		return "Transaksi di Midtrans belum bisa dibatalkan. Coba lagi beberapa saat."
	case errors.Is(err, ErrShipmentActive):
		return "Pesanan sudah punya pengiriman aktif di penyedia. Batalkan pengiriman itu terlebih dahulu."
	}
	return ""
}

// Payments is the part of payment.Service the admin order pages use.
type Payments interface {
	Attempts(ctx context.Context, orderID int64) ([]payment.Attempt, error)
	CancelByAdmin(ctx context.Context, adminID, orderID int64, note string) error
}

// Tab is one filter of the admin order list.
type Tab struct {
	Key      string
	Label    string
	Statuses []order.Status
}

// Tabs are the admin order list filters, in display order.
var Tabs = []Tab{
	{Key: "", Label: "Semua"},
	{Key: "confirm", Label: "Perlu Dikonfirmasi", Statuses: []order.Status{order.StatusWaitingConfirmation}},
	{Key: "processing", Label: "Diproses", Statuses: []order.Status{order.StatusProcessing}},
	{Key: "shipped", Label: "Dikirim", Statuses: []order.Status{order.StatusShipped}},
	{Key: "completed", Label: "Selesai", Statuses: []order.Status{order.StatusCompleted}},
	{Key: "unpaid", Label: "Belum Bayar", Statuses: []order.Status{order.StatusWaitingPayment}},
	{Key: "cancelled", Label: "Dibatalkan", Statuses: []order.Status{order.StatusCancelled}},
}

// TabByKey returns the tab for key, or "Semua".
func TabByKey(key string) Tab {
	for _, t := range Tabs {
		if t.Key == key {
			return t
		}
	}
	return Tabs[0]
}

const (
	PageSize    = 20
	MaxPageSize = 100
)

// Filter selects the admin order list; Query searches the order number.
type Filter struct {
	Tab      string
	Query    string
	Page     int
	PageSize int
}

// Summary is one order in the admin list.
type Summary struct {
	ID          int64
	Number      string
	Status      order.Status
	Total       int64
	CreatedAt   time.Time
	PaidAt      *time.Time
	Recipient   string
	City        string
	Quantity    int
	ProductName string
	ImageURL    string
}

type Page struct {
	Orders  []Summary
	Filter  Filter
	Tab     Tab
	HasNext bool
}

// Buyer is the account that placed the order.
type Buyer struct {
	Email string
	Name  string
}

// ReturnRef is a return of the order (admin detail).
type ReturnRef struct {
	Number    string
	Type      returns.Type
	Status    returns.Status
	Amount    int64
	CreatedAt time.Time
}

// RefundRef is a refund of the order (admin detail).
type RefundRef struct {
	ID           int64
	Source       returns.RefundSource
	Status       returns.RefundStatus
	Amount       int64
	RequestedAt  time.Time
	ProcessedAt  *time.Time
	ReturnNumber string
}

// Detail is one order with everything the admin detail page shows.
type Detail struct {
	ID            int64
	Number        string
	Status        order.Status
	Subtotal      int64
	ShippingCost  int64
	Discount      int64
	Total         int64
	CreatedAt     time.Time
	PaymentDueAt  time.Time
	PaidAt        *time.Time
	ConfirmedAt   *time.Time
	CancelledAt   *time.Time
	CompletedAt   *time.Time
	CancelReason  string
	StockRestored bool
	Buyer         Buyer
	Address       order.Address
	Items         []order.Item
	Shipping      *order.ShippingChoice // chosen at checkout (nil for older orders)
	Shipment      *order.Shipment       // the active shipment, with events
	Shipments     []shipping.Shipment   // every shipment, newest first (failed/cancelled bookings too)
	Payments      []payment.Attempt
	Returns       []ReturnRef
	Refunds       []RefundRef
}

func (d Detail) Quantity() int {
	n := 0
	for _, it := range d.Items {
		n += it.Quantity
	}
	return n
}

// Next steps the admin may take (the service checks again under lock).
func (d Detail) CanConfirm() bool {
	return d.Status == order.StatusWaitingConfirmation && d.PaidAt != nil
}

// CanShip: the manual shipment form (fallback) — only without an active
// shipment.
func (d Detail) CanShip() bool { return d.Status == order.StatusProcessing && d.Shipment == nil }

// CanBook: booking the buyer's courier choice with the provider.
func (d Detail) CanBook() bool {
	return d.Status == order.StatusProcessing && d.PaidAt != nil && d.Shipping != nil && d.Shipment == nil
}

// PastShipments are the inactive (failed or cancelled) shipments.
func (d Detail) PastShipments() []shipping.Shipment {
	var out []shipping.Shipment
	for _, s := range d.Shipments {
		if !s.Status.Active() {
			out = append(out, s)
		}
	}
	return out
}
func (d Detail) CanComplete() bool { return d.Status == order.StatusShipped }
func (d Detail) CanCancel() bool {
	if d.Shipment != nil {
		return false // a booked pickup is cancelled first (CancelBooking)
	}
	switch d.Status {
	case order.StatusWaitingPayment, order.StatusWaitingConfirmation, order.StatusProcessing:
		return true
	}
	return false
}

// CancelCreatesRefund reports whether cancelling now records a refund.
func (d Detail) CancelCreatesRefund() bool { return d.CanCancel() && d.PaidAt != nil }

// CancelReasonText explains a cancellation to the admin.
func (d Detail) CancelReasonText() string {
	switch d.CancelReason {
	case "USER_CANCELLED":
		return "Dibatalkan pembeli"
	case "PAYMENT_EXPIRED":
		return "Batas waktu pembayaran habis"
	case "ADMIN_CANCELLED":
		return "Dibatalkan admin"
	}
	return d.CancelReason
}

// ShipInput is the manual shipment form. Only these fields are read.
type ShipInput struct {
	Courier  string
	Service  string
	Tracking string
	Note     string
}

// CancelResult says what an admin cancellation did.
type CancelResult struct {
	Paid         bool  // the order had been paid
	RefundID     int64 // the refund recorded for it (0 when unpaid)
	RefundAmount int64
}

type Service struct {
	pool     *pgxpool.Pool
	q        *dbgen.Queries
	orders   *order.Repository
	payments Payments
}

func NewService(pool *pgxpool.Pool, payments Payments) *Service {
	return &Service{pool: pool, q: dbgen.New(pool), orders: order.NewRepository(pool), payments: payments}
}

// List returns one page of orders, newest first.
func (s *Service) List(ctx context.Context, f Filter) (Page, error) {
	f.Page = min(max(f.Page, 1), 1000)
	if f.PageSize <= 0 {
		f.PageSize = PageSize
	}
	f.PageSize = min(f.PageSize, MaxPageSize)
	f.Query = validate.SearchText(f.Query)
	tab := TabByKey(f.Tab)
	f.Tab = tab.Key
	statuses := make([]string, len(tab.Statuses))
	for i, st := range tab.Statuses {
		statuses[i] = string(st)
	}
	rows, err := s.q.AdminOrders(ctx, dbgen.AdminOrdersParams{Statuses: statuses, Q: database.Like(f.Query),
		RowLimit: int32(f.PageSize + 1), RowOffset: int32((f.Page - 1) * f.PageSize)})
	if err != nil {
		return Page{}, err
	}
	p := Page{Filter: f, Tab: tab}
	if len(rows) > f.PageSize {
		rows, p.HasNext = rows[:f.PageSize], true
	}
	for _, r := range rows {
		p.Orders = append(p.Orders, Summary{ID: r.ID, Number: r.OrderNumber, Status: order.Status(r.Status), Total: r.Total,
			CreatedAt: r.CreatedAt, PaidAt: r.PaidAt, Recipient: r.RecipientName, City: r.City, Quantity: int(r.Quantity),
			ProductName: r.ProductName, ImageURL: r.ImageUrl})
	}
	return p, nil
}

// Get returns one order with its buyer, items, payments, shipment, returns
// and refunds.
func (s *Service) Get(ctx context.Context, number string) (Detail, error) {
	if number == "" || len(number) > 40 {
		return Detail{}, ErrNotFound
	}
	row, err := s.q.AdminOrder(ctx, number)
	if database.IsNoRows(err) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	d := Detail{ID: row.ID, Number: row.OrderNumber, Status: order.Status(row.Status), Subtotal: row.Subtotal,
		ShippingCost: row.ShippingCost, Discount: row.Discount, Total: row.Total, CreatedAt: row.CreatedAt,
		PaymentDueAt: row.PaymentDueAt, PaidAt: row.PaidAt, ConfirmedAt: row.ConfirmedAt, CancelledAt: row.CancelledAt,
		CompletedAt: row.CompletedAt, CancelReason: deref(row.CancelReason), StockRestored: row.StockRestoredAt != nil,
		Buyer: Buyer{Email: row.Email, Name: strings.TrimSpace(row.FirstName + " " + row.LastName)},
		Address: order.Address{Label: row.AddressLabel, RecipientName: row.RecipientName, Phone: row.RecipientPhone,
			Province: row.Province, City: row.City, District: row.District, Village: row.Village, PostalCode: row.PostalCode,
			Street: row.Address, Latitude: row.Latitude, Longitude: row.Longitude}}
	d.Shipping = order.ShippingColumns{Provider: row.ShippingProvider, CourierCode: row.ShippingCourierCode,
		CourierName: row.ShippingCourierName, ServiceCode: row.ShippingServiceCode, ServiceName: row.ShippingServiceName,
		EstimatedDays: row.ShippingEstimatedDays, WeightGrams: row.ShippingWeightGrams, AreaID: row.ShippingAreaID,
		AreaLabel: row.ShippingAreaLabel}.Choice()
	d.Shipment = order.ShipmentColumns{ID: row.ShipmentID, Method: row.ShipmentMethod, Provider: row.ShipmentProvider,
		Courier: row.CourierName, CourierCode: row.ShipmentCourierCode, Service: row.ServiceName,
		ServiceCode: row.ShipmentServiceCode, Tracking: row.TrackingNumber, TrackingURL: row.ShipmentTrackingUrl,
		Status: row.ShipmentStatus, EstimatedDays: row.ShipmentEstimatedDays, Note: row.ShipmentNote,
		FailureReason: row.ShipmentFailureReason, ShippedAt: row.ShippedAt, BookedAt: row.ShipmentBookedAt,
		DeliveredAt: row.ShipmentDeliveredAt, CancelledAt: row.ShipmentCancelledAt, SyncedAt: row.ShipmentSyncedAt,
		CreatedAt: row.ShipmentCreatedAt}.Shipment()
	if d.Items, err = s.orders.Items(ctx, d.ID); err != nil {
		return Detail{}, err
	}
	if err = s.orders.Timeline(ctx, d.Shipment); err != nil {
		return Detail{}, err
	}
	if d.Shipment != nil || d.Status == order.StatusProcessing {
		rows, err := s.q.OrderShipments(ctx, d.ID)
		if err != nil {
			return Detail{}, err
		}
		for _, r := range rows {
			d.Shipments = append(d.Shipments, shipping.FromRow(r))
		}
	}
	if s.payments != nil {
		if d.Payments, err = s.payments.Attempts(ctx, d.ID); err != nil {
			return Detail{}, err
		}
	}
	rets, err := s.q.AdminOrderReturns(ctx, d.ID)
	if err != nil {
		return Detail{}, err
	}
	for _, r := range rets {
		d.Returns = append(d.Returns, ReturnRef{Number: r.ReturnNumber, Type: returns.Type(r.Type), Status: returns.Status(r.Status),
			Amount: r.RefundAmount, CreatedAt: r.CreatedAt})
	}
	refunds, err := s.q.OrderRefunds(ctx, d.ID)
	if err != nil {
		return Detail{}, err
	}
	for _, f := range refunds {
		d.Refunds = append(d.Refunds, RefundRef{ID: f.ID, Source: returns.RefundSource(f.Source), Status: returns.RefundStatus(f.Status),
			Amount: f.Amount, RequestedAt: f.RequestedAt, ProcessedAt: f.ProcessedAt, ReturnNumber: deref(f.ReturnNumber)})
	}
	return d, nil
}

// locked is an order row held for a fulfillment change.
type locked struct {
	ID            int64
	Number        string
	Status        order.Status
	Total         int64
	Paid          bool
	StockRestored bool
}

// change locks the order, runs fn and records the audit entry fn names, in
// one transaction.
func (s *Service) change(ctx context.Context, adminID int64, number string, fn func(q *dbgen.Queries, o locked) (string, map[string]any, error)) error {
	if number == "" || len(number) > 40 {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		row, err := q.LockFulfillmentOrder(ctx, number)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		o := locked{ID: row.ID, Number: row.OrderNumber, Status: order.Status(row.Status), Total: row.Total,
			Paid: row.PaidAt != nil, StockRestored: row.StockRestoredAt != nil}
		action, meta, err := fn(q, o)
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{AdminID: adminID, Action: action, EntityType: audit.EntityOrder,
			EntityID: o.ID, Label: o.Number, Meta: meta})
	})
}

// Confirm accepts a paid order: WAITING_CONFIRMATION → PROCESSING. An
// unpaid order is ErrNotPaid; the order must also hold a PAID payment.
func (s *Service) Confirm(ctx context.Context, adminID int64, number string) error {
	return s.change(ctx, adminID, number, func(q *dbgen.Queries, o locked) (string, map[string]any, error) {
		switch {
		case o.Status == order.StatusWaitingPayment:
			return "", nil, ErrNotPaid
		case o.Status != order.StatusWaitingConfirmation:
			return "", nil, ErrInvalidTransition
		case !o.Paid:
			return "", nil, ErrNotPaid
		}
		if _, err := q.LockPaidPayment(ctx, o.ID); database.IsNoRows(err) {
			return "", nil, ErrNotPaid
		} else if err != nil {
			return "", nil, err
		}
		if err := one(q.ConfirmOrder(ctx, o.ID)); err != nil {
			return "", nil, err
		}
		return "order.confirm", map[string]any{"from": string(o.Status), "to": string(order.StatusProcessing)}, nil
	})
}

// Ship records a manual shipment (the fallback when the provider booking is
// not used or failed) and moves PROCESSING → SHIPPED. An order with an
// active provider shipment is refused (ErrShipmentActive) by the
// one-active-shipment index.
func (s *Service) Ship(ctx context.Context, adminID int64, number string, in ShipInput) error {
	in.Courier, in.Service, in.Tracking = validate.Text(in.Courier), validate.Text(in.Service), validate.Text(in.Tracking)
	in.Note = strings.TrimSpace(in.Note)
	e := validate.Errors{}
	validate.Length(e, "courier", in.Courier, "Kurir", 2, 60)
	validate.Length(e, "service", in.Service, "Layanan", 0, 60)
	validate.Length(e, "tracking", in.Tracking, "Nomor resi", 0, 60)
	validate.Length(e, "note", in.Note, "Catatan", 0, 500)
	if err := e.Err(); err != nil {
		return err
	}
	return s.change(ctx, adminID, number, func(q *dbgen.Queries, o locked) (string, map[string]any, error) {
		if o.Status != order.StatusProcessing {
			if o.Status == order.StatusWaitingPayment {
				return "", nil, ErrNotPaid
			}
			return "", nil, ErrInvalidTransition
		}
		if err := one(q.ShipOrder(ctx, o.ID)); err != nil {
			return "", nil, err
		}
		now := time.Now()
		err := q.CreateShipment(ctx, dbgen.CreateShipmentParams{OrderID: o.ID, CourierName: in.Courier, ServiceName: in.Service,
			TrackingNumber: in.Tracking, Note: in.Note, ShippedAt: &now, CreatedBy: &adminID})
		if database.IsUniqueViolation(err, "shipments_active_order_idx") {
			return "", nil, ErrShipmentActive
		}
		if err != nil {
			return "", nil, err
		}
		return "order.ship", map[string]any{"from": string(o.Status), "to": string(order.StatusShipped),
			"courier": in.Courier, "service": in.Service, "tracking": in.Tracking}, nil
	})
}

// Complete closes a shipped order: SHIPPED → COMPLETED with completed_at
// in the same transaction. From then on its items may be reviewed and the
// return window runs.
func (s *Service) Complete(ctx context.Context, adminID int64, number string) error {
	return s.change(ctx, adminID, number, func(q *dbgen.Queries, o locked) (string, map[string]any, error) {
		if o.Status != order.StatusShipped {
			return "", nil, ErrInvalidTransition
		}
		if err := one(q.CompleteOrder(ctx, o.ID)); err != nil {
			return "", nil, err
		}
		return "order.complete", map[string]any{"from": string(o.Status), "to": string(order.StatusCompleted)}, nil
	})
}

// Cancel cancels an order on an admin's decision (reason required):
//
//   - unpaid (WAITING_PAYMENT): by the payment rules — the provider
//     transaction is cancelled first, stock is restored once;
//   - paid, not shipped (WAITING_CONFIRMATION, PROCESSING): in one
//     transaction lock order → PAID payment → variants (id order), restore
//     the stock once (stock_restored_at, ORDER_RESTORE adjustments), cancel,
//     and record a PENDING refund of what was paid. No provider refund is
//     made: the shop pays it back and records the outcome;
//   - shipped or completed: ErrNotCancellable (use a return).
//
// Cancelling an already cancelled order changes nothing.
func (s *Service) Cancel(ctx context.Context, adminID int64, number, note string) (CancelResult, error) {
	note = strings.TrimSpace(note)
	e := validate.Errors{}
	validate.Length(e, "note", note, "Alasan pembatalan", 3, 500)
	if note == "" {
		e = validate.Errors{"note": "Alasan pembatalan wajib diisi."}
	}
	if err := e.Err(); err != nil {
		return CancelResult{}, err
	}
	d, err := s.q.OrderStatusByNumber(ctx, number)
	if database.IsNoRows(err) {
		return CancelResult{}, ErrNotFound
	}
	if err != nil {
		return CancelResult{}, err
	}
	if order.Status(d.Status) == order.StatusWaitingPayment && s.payments != nil {
		err := s.payments.CancelByAdmin(ctx, adminID, d.ID, note)
		if !errors.Is(err, payment.ErrAlreadyPaid) {
			return CancelResult{}, err
		}
		// Paid in the meantime: cancel it as a paid order below.
	}

	var res CancelResult
	err = s.change(ctx, adminID, number, func(q *dbgen.Queries, o locked) (string, map[string]any, error) {
		switch o.Status {
		case order.StatusCancelled:
			return "", nil, errAlreadyCancelled
		case order.StatusWaitingConfirmation, order.StatusProcessing:
		case order.StatusWaitingPayment:
			return "", nil, ErrInvalidTransition // the payment service decides unpaid orders
		default:
			return "", nil, ErrNotCancellable
		}
		if !o.Paid || o.StockRestored {
			return "", nil, ErrInvalidTransition
		}
		if sh, err := q.ActiveShipment(ctx, o.ID); err == nil && sh.Method == "PROVIDER" {
			return "", nil, ErrShipmentActive // a booked pickup must be cancelled with the provider first
		} else if err != nil && !database.IsNoRows(err) {
			return "", nil, err
		}
		pay, err := q.LockPaidPayment(ctx, o.ID)
		if database.IsNoRows(err) {
			return "", nil, ErrNotPaid
		}
		if err != nil {
			return "", nil, err
		}
		if _, err := q.LockOrderVariants(ctx, o.ID); err != nil {
			return "", nil, err
		}
		reason := "ADMIN_CANCELLED"
		st, err := q.RestoreOrderStock(ctx, dbgen.RestoreOrderStockParams{OrderID: o.ID, Reason: reason, AdminUserID: &adminID})
		if err != nil {
			return "", nil, err
		}
		if st.Restored != st.Variants {
			return "", nil, fmt.Errorf("fulfillment: restored %d of %d variants", st.Restored, st.Variants)
		}
		if err := one(q.CancelPaidOrder(ctx, o.ID)); err != nil {
			return "", nil, err
		}
		done, err := q.RefundedForPayment(ctx, &pay.ID)
		if err != nil {
			return "", nil, err
		}
		res = CancelResult{Paid: true}
		if amount := pay.Amount - done; amount > 0 {
			if res.RefundID, err = q.CreateCancellationRefund(ctx, dbgen.CreateCancellationRefundParams{
				OrderID: o.ID, PaymentID: &pay.ID, Amount: amount}); err != nil {
				return "", nil, err
			}
			res.RefundAmount = amount
		}
		if err := q.InsertPaymentEvent(ctx, dbgen.InsertPaymentEventParams{OrderID: o.ID, PaymentID: &pay.ID, Source: "admin",
			Kind: "order_cancelled", Detail: fmt.Sprintf("%s after payment; stock restored for %d variant(s); refund %d recorded, not sent",
				reason, st.Restored, res.RefundAmount)}); err != nil {
			return "", nil, err
		}
		return "order.cancel", map[string]any{"from": string(o.Status), "paid": true, "note": note,
			"refund_id": res.RefundID, "refund_amount": res.RefundAmount, "variants_restored": st.Restored}, nil
	})
	if errors.Is(err, errAlreadyCancelled) {
		return CancelResult{}, nil
	}
	return res, err
}

var errAlreadyCancelled = errors.New("fulfillment: already cancelled")

// one turns an :execrows result into ErrInvalidTransition when no row
// changed (the order moved meanwhile).
func one(n int64, err error) error {
	if err == nil && n != 1 {
		return ErrInvalidTransition
	}
	return err
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
