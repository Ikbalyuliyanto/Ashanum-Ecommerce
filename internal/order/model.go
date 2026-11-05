// Package order reads orders: history ("Pesanan Saya") and order detail.
// Orders are created by package checkout; payment, cancellation and expiry
// of unpaid orders belong to package payment. Every order is a snapshot, so
// history stays correct when addresses or the catalog change later. The
// Service has no HTTP dependency, so a future API can reuse it.
package order

import (
	"errors"
	"strings"
	"time"

	"ecommerce/internal/payment"
	"ecommerce/internal/returns"
	"ecommerce/internal/shipping"
)

var ErrNotFound = errors.New("order: not found")

type Status string

const (
	StatusWaitingPayment      Status = "WAITING_PAYMENT"
	StatusWaitingConfirmation Status = "WAITING_CONFIRMATION"
	StatusProcessing          Status = "PROCESSING"
	StatusShipped             Status = "SHIPPED"
	StatusCompleted           Status = "COMPLETED"
	StatusCancelled           Status = "CANCELLED"
)

// Label is the short customer-facing status (Indonesian).
func (s Status) Label() string {
	switch s {
	case StatusWaitingPayment:
		return "Belum Bayar"
	case StatusWaitingConfirmation:
		return "Menunggu Konfirmasi"
	case StatusProcessing:
		return "Diproses"
	case StatusShipped:
		return "Dikirim"
	case StatusCompleted:
		return "Selesai"
	case StatusCancelled:
		return "Dibatalkan"
	}
	return string(s)
}

// Tab is one filter of the order history.
type Tab struct {
	Key      string
	Label    string
	Statuses []Status
}

// Tabs are the order history filters, in display order.
var Tabs = []Tab{
	{Key: "", Label: "Semua"},
	{Key: "unpaid", Label: "Belum Bayar", Statuses: []Status{StatusWaitingPayment}},
	{Key: "processing", Label: "Diproses", Statuses: []Status{StatusWaitingConfirmation, StatusProcessing}},
	{Key: "shipped", Label: "Dikirim", Statuses: []Status{StatusShipped}},
	{Key: "completed", Label: "Selesai", Statuses: []Status{StatusCompleted}},
	{Key: "cancelled", Label: "Dibatalkan", Statuses: []Status{StatusCancelled}},
}

// TabByKey returns the tab for key, or "Semua" for unknown keys.
func TabByKey(key string) Tab {
	for _, t := range Tabs {
		if t.Key == key {
			return t
		}
	}
	return Tabs[0]
}

// Address is the shipping address copied into the order.
type Address struct {
	Label         string
	RecipientName string
	Phone         string
	Province      string
	City          string
	District      string
	Village       string
	PostalCode    string
	Street        string
	Latitude      *float64
	Longitude     *float64
}

// Item is an order line snapshot.
type Item struct {
	ID           int64
	ProductID    int64 // 0 once the product was deleted from the catalog
	VariantID    int64 // 0 once the variant was deleted
	ProductName  string
	ProductSlug  string
	SKU          string
	VariantLabel string
	ImageURL     string
	UnitPrice    int64
	Quantity     int
	Subtotal     int64

	ReviewID int64 // the buyer's review of this line, 0 when none
	Returned int   // units held by returns that still count
}

// Returnable is how many units of the line may still be returned.
func (i Item) Returnable() int { return max(i.Quantity-i.Returned, 0) }

// Shipment is the order's active shipment: booked through the shipping
// provider (status and tracking events) or entered manually by the shop.
type Shipment = shipping.Shipment

// ShippingChoice is the order's shipping snapshot: the courier service and
// cost the buyer chose at checkout. It never changes after the order is
// placed and stays readable when the provider is down. nil for orders
// placed before shipping rates existed.
type ShippingChoice struct {
	Provider      string
	CourierCode   string
	CourierName   string
	ServiceCode   string
	ServiceName   string
	EstimatedDays string
	WeightGrams   int
	AreaID        string
	AreaLabel     string
}

// Name is "JNE Reguler".
func (c ShippingChoice) Name() string { return strings.TrimSpace(c.CourierName + " " + c.ServiceName) }

// EstimateLabel is the delivery estimate at checkout ("2–3 hari").
func (c ShippingChoice) EstimateLabel() string { return shipping.EstimateLabel(c.EstimatedDays) }

// ShipmentColumns are the shipment columns read with an order.
type ShipmentColumns struct {
	ID            *int64
	Method        *string
	Provider      *string
	Courier       *string
	CourierCode   *string
	Service       *string
	ServiceCode   *string
	Tracking      *string
	TrackingURL   *string
	Status        *string
	EstimatedDays *string
	Note          *string
	FailureReason *string
	ShippedAt     *time.Time
	BookedAt      *time.Time
	DeliveredAt   *time.Time
	CancelledAt   *time.Time
	SyncedAt      *time.Time
	CreatedAt     *time.Time
}

// Shipment converts the columns (nil when the order has no shipment).
func (c ShipmentColumns) Shipment() *Shipment {
	if c.ID == nil {
		return nil
	}
	return &Shipment{ID: *c.ID, Method: deref(c.Method), Provider: deref(c.Provider), Courier: deref(c.Courier),
		CourierCode: deref(c.CourierCode), Service: deref(c.Service), ServiceCode: deref(c.ServiceCode),
		Tracking: deref(c.Tracking), TrackingURL: deref(c.TrackingURL), Status: shipping.Status(deref(c.Status)),
		EstimatedDays: deref(c.EstimatedDays), Note: deref(c.Note), FailureReason: deref(c.FailureReason),
		ShippedAt: c.ShippedAt, BookedAt: c.BookedAt, DeliveredAt: c.DeliveredAt, CancelledAt: c.CancelledAt,
		LastSyncedAt: c.SyncedAt, CreatedAt: deref(c.CreatedAt)}
}

// ShippingColumns are the order's shipping snapshot columns.
type ShippingColumns struct {
	Provider      *string
	CourierCode   *string
	CourierName   *string
	ServiceCode   *string
	ServiceName   *string
	EstimatedDays *string
	WeightGrams   *int32
	AreaID        *string
	AreaLabel     *string
}

// Choice converts the snapshot (nil for orders without one).
func (c ShippingColumns) Choice() *ShippingChoice {
	if c.Provider == nil {
		return nil
	}
	return &ShippingChoice{Provider: *c.Provider, CourierCode: deref(c.CourierCode), CourierName: deref(c.CourierName),
		ServiceCode: deref(c.ServiceCode), ServiceName: deref(c.ServiceName), EstimatedDays: deref(c.EstimatedDays),
		WeightGrams: int(deref(c.WeightGrams)), AreaID: deref(c.AreaID), AreaLabel: deref(c.AreaLabel)}
}

type Order struct {
	ID           int64
	Number       string
	Status       Status
	Subtotal     int64
	ShippingCost int64
	Discount     int64
	Total        int64
	Address      Address
	Items        []Item
	CreatedAt    time.Time

	PaymentDueAt time.Time  // pay before this, or the order is cancelled
	PaidAt       *time.Time // payment confirmed by the provider
	Paid         bool       // has a PAID payment (and paid_at)
	ConfirmedAt  *time.Time // accepted by the shop (→ PROCESSING)
	CancelledAt  *time.Time
	CancelReason string // USER_CANCELLED | PAYMENT_EXPIRED | ADMIN_CANCELLED
	CompletedAt  *time.Time
	Shipping     *ShippingChoice // chosen at checkout; nil for older orders
	Shipment     *Shipment       // the active shipment, with tracking events

	Payments       []payment.Attempt // oldest first
	PaymentEnabled bool              // online payment is configured
	Sandbox        bool              // test payments only

	// Completed orders only: returns of the order and return eligibility.
	Returns returns.OrderInfo
}

// ReviewID is the review of an order item, or 0.
func (o Order) ReviewID(itemID int64) int64 {
	for _, it := range o.Items {
		if it.ID == itemID {
			return it.ReviewID
		}
	}
	return 0
}

// CanReview reports whether an item of a completed order still has no review.
func (o Order) CanReview() bool { return o.FirstUnreviewed() != 0 }

// FirstUnreviewed is the first item without a review (0 when none or when
// the order is not completed).
func (o Order) FirstUnreviewed() int64 {
	if o.Status != StatusCompleted {
		return 0
	}
	for _, it := range o.Items {
		if it.ReviewID == 0 {
			return it.ID
		}
	}
	return 0
}

// Returnable reports whether some line still has units that may be
// returned (the window and payment are checked by package returns).
func (o Order) Returnable() bool {
	for _, it := range o.Items {
		if it.Returnable() > 0 {
			return true
		}
	}
	return false
}

// LastPayment is the latest payment attempt, or nil.
func (o Order) LastPayment() *payment.Attempt {
	if len(o.Payments) == 0 {
		return nil
	}
	return &o.Payments[len(o.Payments)-1]
}

// CanPay reports whether the buyer can pay now ("Bayar Sekarang").
func (o Order) CanPay() bool {
	return o.Status == StatusWaitingPayment && o.PaymentEnabled && time.Now().Before(o.PaymentDueAt)
}

// CanCancel reports whether the buyer can still cancel (unpaid orders only).
func (o Order) CanCancel() bool { return o.Status == StatusWaitingPayment }

// PaymentFailed reports an unpaid order whose latest attempt did not take
// money (declined, failed, expired): the buyer may pay again.
func (o Order) PaymentFailed() bool {
	last := o.LastPayment()
	return o.Status == StatusWaitingPayment && last != nil && last.Status.Unsuccessful()
}

// PayLabel is the pay button text.
func (o Order) PayLabel() string {
	if o.PaymentFailed() {
		return "Bayar Lagi"
	}
	return "Bayar Sekarang"
}

// CancelReasonText explains a cancellation to the buyer.
func (o Order) CancelReasonText() string {
	switch o.CancelReason {
	case "USER_CANCELLED":
		return "Pesanan dibatalkan oleh Anda."
	case "PAYMENT_EXPIRED":
		return "Pesanan dibatalkan otomatis karena batas waktu pembayaran habis."
	case "ADMIN_CANCELLED":
		if o.Paid {
			return "Pesanan dibatalkan oleh penjual. Dana yang sudah dibayar akan dikembalikan oleh toko."
		}
		return "Pesanan dibatalkan oleh penjual."
	}
	return "Pesanan dibatalkan."
}

// Quantity is the total number of units in the order.
func (o Order) Quantity() int {
	n := 0
	for _, it := range o.Items {
		n += it.Quantity
	}
	return n
}

// Summary is one order card in the history.
type Summary struct {
	ID        int64
	Number    string
	Status    Status
	Total     int64
	CreatedAt time.Time
	DueAt     time.Time // payment deadline (unpaid orders)
	ItemCount int
	First     Item // first line, shown on the card
}

// Page is one page of the order history.
type Page struct {
	Orders  []Summary
	Tab     Tab
	Page    int
	HasNext bool
}

// PageSize is the number of orders per history page.
const PageSize = 10
