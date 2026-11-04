// Package returns handles return requests for completed, paid orders:
// refund ("Pengembalian Dana") or exchange ("Tukar Barang"), their items,
// evidence links and status lifecycle, plus the refund domain foundation.
//
// Buyers only submit (and may withdraw a request still waiting for review).
// Every other step — approve, reject, receive the goods, prepare and record
// a refund, complete — is a Service method for the admin phase; no route
// exposes them yet. No payment provider is called: a refund is recorded as
// PENDING and money is returned outside this system for now.
//
// The package is named "returns" because return is a Go keyword.
package returns

import (
	"errors"
	"time"

	"ecommerce/internal/media"
)

type Type string

const (
	TypeRefund   Type = "REFUND"
	TypeExchange Type = "EXCHANGE"
)

func (t Type) Valid() bool { return t == TypeRefund || t == TypeExchange }

func (t Type) Label() string {
	switch t {
	case TypeRefund:
		return "Pengembalian Dana"
	case TypeExchange:
		return "Tukar Barang"
	}
	return string(t)
}

type Reason string

const (
	ReasonDamaged        Reason = "DAMAGED"
	ReasonWrongItem      Reason = "WRONG_ITEM"
	ReasonMissingItem    Reason = "MISSING_ITEM"
	ReasonNotAsDescribed Reason = "NOT_AS_DESCRIBED"
	ReasonVariantIssue   Reason = "VARIANT_ISSUE"
	ReasonOther          Reason = "OTHER"
)

// Reasons in display order.
var Reasons = []Reason{ReasonDamaged, ReasonWrongItem, ReasonMissingItem, ReasonNotAsDescribed, ReasonVariantIssue, ReasonOther}

func (r Reason) Valid() bool {
	for _, x := range Reasons {
		if r == x {
			return true
		}
	}
	return false
}

func (r Reason) Label() string {
	switch r {
	case ReasonDamaged:
		return "Barang rusak"
	case ReasonWrongItem:
		return "Barang tidak sesuai"
	case ReasonMissingItem:
		return "Barang kurang"
	case ReasonNotAsDescribed:
		return "Tidak sesuai deskripsi"
	case ReasonVariantIssue:
		return "Masalah ukuran/varian"
	case ReasonOther:
		return "Lainnya"
	}
	return string(r)
}

type Status string

const (
	StatusRequested     Status = "REQUESTED"
	StatusApproved      Status = "APPROVED"
	StatusRejected      Status = "REJECTED"
	StatusWaitingReturn Status = "WAITING_RETURN"
	StatusItemReceived  Status = "ITEM_RECEIVED"
	StatusRefundPending Status = "REFUND_PENDING"
	StatusRefunded      Status = "REFUNDED"
	StatusExchanged     Status = "EXCHANGED"
	StatusCompleted     Status = "COMPLETED"
	StatusCancelled     Status = "CANCELLED"
)

func (s Status) Label() string {
	switch s {
	case StatusRequested:
		return "Menunggu Persetujuan"
	case StatusApproved:
		return "Disetujui"
	case StatusRejected:
		return "Ditolak"
	case StatusWaitingReturn:
		return "Menunggu Barang Dikirim"
	case StatusItemReceived:
		return "Barang Diterima Penjual"
	case StatusRefundPending:
		return "Menunggu Pengembalian Dana"
	case StatusRefunded:
		return "Dana Dikembalikan"
	case StatusExchanged:
		return "Barang Pengganti Dikirim"
	case StatusCompleted:
		return "Selesai"
	case StatusCancelled:
		return "Dibatalkan"
	}
	return string(s)
}

// Closed reports a return that no longer holds quantity (rejected or
// cancelled): its items may be requested again.
func (s Status) Closed() bool { return s == StatusRejected || s == StatusCancelled }

type RefundStatus string

const (
	RefundPending    RefundStatus = "PENDING"
	RefundApproved   RefundStatus = "APPROVED"
	RefundProcessing RefundStatus = "PROCESSING"
	RefundSucceeded  RefundStatus = "SUCCEEDED"
	RefundFailed     RefundStatus = "FAILED"
	RefundCancelled  RefundStatus = "CANCELLED"
)

func (s RefundStatus) Label() string {
	switch s {
	case RefundPending:
		return "Menunggu Diproses"
	case RefundApproved:
		return "Disetujui"
	case RefundProcessing:
		return "Sedang Diproses"
	case RefundSucceeded:
		return "Berhasil"
	case RefundFailed:
		return "Gagal"
	case RefundCancelled:
		return "Dibatalkan"
	}
	return string(s)
}

// Actor is who changed a return (timeline).
type Actor string

const (
	ActorUser   Actor = "USER"
	ActorAdmin  Actor = "ADMIN"
	ActorSystem Actor = "SYSTEM"
)

const (
	PageSize       = 10
	MaxEvidence    = 5
	MaxDescription = 1000
	maxKeyLength   = 64
)

var (
	ErrNotFound          = errors.New("returns: not found")
	ErrOrderNotCompleted = errors.New("returns: order is not completed")
	ErrOrderNotPaid      = errors.New("returns: order was not paid")
	ErrWindowClosed      = errors.New("returns: return window has closed")
	ErrNothingToReturn   = errors.New("returns: every item is already in a return")
	ErrInvalidTransition = errors.New("returns: status change not allowed")
	ErrNoPayment         = errors.New("returns: order has no paid payment to refund")
	ErrRefundTooLarge    = errors.New("returns: refund exceeds the amount paid")

	errDuplicateKey = errors.New("returns: idempotency key already used")
)

// Message is the customer-facing text for a known error, or "".
func Message(err error) string {
	switch {
	case errors.Is(err, ErrOrderNotCompleted):
		return "Pengembalian hanya bisa diajukan untuk pesanan yang sudah selesai."
	case errors.Is(err, ErrOrderNotPaid):
		return "Pengembalian hanya bisa diajukan untuk pesanan yang sudah dibayar."
	case errors.Is(err, ErrWindowClosed):
		return "Batas waktu pengajuan pengembalian untuk pesanan ini sudah lewat."
	case errors.Is(err, ErrNothingToReturn):
		return "Semua barang di pesanan ini sudah diajukan pengembalian."
	case errors.Is(err, ErrInvalidTransition):
		return "Status pengajuan ini sudah berubah dan tidak dapat diproses."
	}
	return ""
}

// Item is an order item in the return form, with what is still returnable.
type Item struct {
	OrderItemID  int64
	ProductName  string
	VariantLabel string
	ImageURL     string
	UnitPrice    int64
	Purchased    int
	Returned     int // already in returns that are not rejected or cancelled
}

func (i Item) Remaining() int { return max(i.Purchased-i.Returned, 0) }

// QuantityOptions are the choices of the quantity picker (1..Remaining).
func (i Item) QuantityOptions() []int {
	opts := make([]int, i.Remaining())
	for n := range opts {
		opts[n] = n + 1
	}
	return opts
}

// Draft is the return form for one order.
type Draft struct {
	OrderNumber string
	Deadline    time.Time
	Items       []Item
}

// ItemRequest is one line the buyer wants to return.
type ItemRequest struct {
	OrderItemID int64
	Quantity    int
}

// Request is a buyer's return request. Amounts are never taken from it.
type Request struct {
	OrderNumber    string
	Items          []ItemRequest
	Type           Type
	Reason         Reason
	Description    string
	Evidence       []string // https URLs (uploads later produce URLs too)
	IdempotencyKey string
}

type ReturnItem struct {
	OrderItemID  int64
	ProductName  string
	VariantLabel string
	ImageURL     string
	Quantity     int
	UnitPrice    int64
}

func (i ReturnItem) Subtotal() int64 { return i.UnitPrice * int64(i.Quantity) }

// Event is one step of the return timeline.
type Event struct {
	From  Status
	To    Status
	Actor Actor
	Note  string
	At    time.Time
}

type Refund struct {
	ID          int64
	Amount      int64
	Status      RefundStatus
	Provider    string
	RequestedAt time.Time
	ProcessedAt *time.Time
}

// RefundResult records a refund carried out outside this system (manual
// transfer now; a provider integration later). Recording it calls nothing.
type RefundResult struct {
	Provider  string // e.g. "manual"
	Reference string // transfer or provider reference, optional
}

type Return struct {
	ID             int64
	Number         string
	OrderNumber    string
	Type           Type
	Status         Status
	Reason         Reason
	Description    string
	RefundAmount   int64
	ResolutionNote string
	RequestedAt    time.Time
	ApprovedAt     *time.Time
	RejectedAt     *time.Time
	CompletedAt    *time.Time
	Items          []ReturnItem
	Evidence       []media.Attachment
	Events         []Event
	Refund         *Refund // latest refund, if any
}

// Quantity is the number of units in the return.
func (r Return) Quantity() int {
	n := 0
	for _, it := range r.Items {
		n += it.Quantity
	}
	return n
}

// ItemsValue is the value of the returned items at the price paid.
func (r Return) ItemsValue() int64 {
	var v int64
	for _, it := range r.Items {
		v += it.Subtotal()
	}
	return v
}

// CanCancel reports whether the buyer may still withdraw the request.
func (r Return) CanCancel() bool { return r.Status == StatusRequested }

// Summary is one card of the return history.
type Summary struct {
	Number       string
	OrderNumber  string
	Type         Type
	Status       Status
	RefundAmount int64
	CreatedAt    time.Time
	ItemCount    int
	ProductName  string
	ImageURL     string
}

type Page struct {
	Returns []Summary
	Page    int
	HasNext bool
}

// OrderReturn is a return listed on its order's detail page.
type OrderReturn struct {
	Number    string
	Type      Type
	Status    Status
	CreatedAt time.Time
}

// OrderInfo is what the order detail page needs about returns.
type OrderInfo struct {
	Eligible bool       // "Ajukan Pengembalian" may be shown
	Deadline *time.Time // end of the return window
	Returns  []OrderReturn
}

// ActorLabel names who made a timeline step, for the buyer.
func (e Event) ActorLabel() string {
	switch e.Actor {
	case ActorUser:
		return "Anda"
	case ActorAdmin:
		return "Ashanum"
	}
	return "Sistem"
}
