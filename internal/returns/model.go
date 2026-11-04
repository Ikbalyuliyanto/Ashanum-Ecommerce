// Package returns handles return requests for completed, paid orders:
// refund ("Pengembalian Dana") or exchange ("Tukar Barang"), their items,
// evidence links and status lifecycle, plus the refund domain foundation.
//
// Buyers only submit (and may withdraw a request still waiting for review).
// The shop (admin) reviews it — approving all or part of each line, or
// rejecting it — then receives and inspects the goods, may put restockable
// goods back on sale, and pays a refund or sends a replacement. No payment
// provider is called: a refund is carried out outside this system and only
// its outcome is recorded here.
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

// Condition is the shop's inspection verdict on a returned line. Only
// RESTOCKABLE goods may go back on sale (and only by an explicit restock).
type Condition string

const (
	ConditionRestockable    Condition = "RESTOCKABLE"
	ConditionDamaged        Condition = "DAMAGED"
	ConditionNonRestockable Condition = "NON_RESTOCKABLE"
)

// Conditions in display order.
var Conditions = []Condition{ConditionRestockable, ConditionDamaged, ConditionNonRestockable}

func (c Condition) Valid() bool {
	return c == ConditionRestockable || c == ConditionDamaged || c == ConditionNonRestockable
}

func (c Condition) Label() string {
	switch c {
	case ConditionRestockable:
		return "Layak jual kembali"
	case ConditionDamaged:
		return "Rusak"
	case ConditionNonRestockable:
		return "Tidak layak jual"
	}
	return string(c)
}

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

// Open reports a refund still to be carried out.
func (s RefundStatus) Open() bool {
	return s == RefundPending || s == RefundApproved || s == RefundProcessing
}

// RefundSource says why money is owed back.
type RefundSource string

const (
	SourceReturn            RefundSource = "RETURN"
	SourceOrderCancellation RefundSource = "ORDER_CANCELLATION"
)

func (s RefundSource) Label() string {
	if s == SourceOrderCancellation {
		return "Pembatalan pesanan"
	}
	return "Pengembalian barang"
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
	ErrRefundNotFound    = errors.New("returns: refund not found")
	ErrNothingToRestock  = errors.New("returns: nothing left to restock")
	ErrRefundOpen        = errors.New("returns: the return already has an open or paid refund")

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

// AdminMessage is the admin-facing text for a known error, or "".
func AdminMessage(err error) string {
	switch {
	case errors.Is(err, ErrInvalidTransition):
		return "Langkah ini tidak sesuai status pengembalian saat ini (mungkin sudah diproses)."
	case errors.Is(err, ErrNoPayment):
		return "Pesanan ini tidak punya pembayaran berhasil untuk dikembalikan."
	case errors.Is(err, ErrRefundTooLarge):
		return "Total refund akan melebihi jumlah yang dibayar."
	case errors.Is(err, ErrNothingToRestock):
		return "Tidak ada barang layak jual yang belum dikembalikan ke stok."
	case errors.Is(err, ErrRefundOpen):
		return "Pengembalian ini sudah punya refund yang berjalan atau selesai."
	case errors.Is(err, ErrRefundNotFound), errors.Is(err, ErrNotFound):
		return "Data tidak ditemukan."
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
	ID           int64 // return line
	OrderItemID  int64
	ProductID    int64 // 0 once the product was deleted
	VariantID    int64 // 0 once the variant was deleted
	ProductName  string
	SKU          string
	VariantLabel string
	ImageURL     string
	Quantity     int // requested
	UnitPrice    int64

	Approved    *int // set when the shop reviewed the request
	Condition   Condition
	InspectedAt *time.Time
	Restocked   int // units put back on sale (0 = not restocked)
	RestockedAt *time.Time

	ReplacementSKU   string
	ReplacementLabel string
	ReplacementQty   int
}

func (i ReturnItem) Subtotal() int64 { return i.UnitPrice * int64(i.Quantity) }

// ApprovedQuantity is what the shop accepted, or the requested quantity
// before the review.
func (i ReturnItem) ApprovedQuantity() int {
	if i.Approved != nil {
		return *i.Approved
	}
	return i.Quantity
}

// Partial reports a line approved for fewer units than requested.
func (i ReturnItem) Partial() bool { return i.Approved != nil && *i.Approved < i.Quantity }

// ApprovedSubtotal is the value of the approved units at the price paid.
func (i ReturnItem) ApprovedSubtotal() int64 { return i.UnitPrice * int64(i.ApprovedQuantity()) }

// CanRestock reports an inspected restockable line not restocked yet whose
// catalog variant still exists.
func (i ReturnItem) CanRestock() bool {
	return i.Condition == ConditionRestockable && i.RestockedAt == nil && i.ApprovedQuantity() > 0 && i.VariantID != 0
}

// ApprovedOptions are the choices of the approval picker (0..requested).
func (i ReturnItem) ApprovedOptions() []int {
	opts := make([]int, i.Quantity+1)
	for n := range opts {
		opts[n] = n
	}
	return opts
}

// Event is one step of the return timeline.
type Event struct {
	From  Status
	To    Status
	Actor Actor
	Note  string
	At    time.Time
}

type Refund struct {
	ID            int64
	Source        RefundSource
	Amount        int64
	Status        RefundStatus
	Provider      string // how it was paid out, e.g. "manual"
	Reference     string // transfer or provider reference
	Note          string
	FailureReason string
	RequestedAt   time.Time
	ProcessedAt   *time.Time

	// Admin views.
	OrderNumber     string
	ReturnNumber    string
	ReturnStatus    Status
	PaymentProvider string
	PaymentOrderID  string // the provider transaction reference of the payment
	PaymentTxID     string
	PaymentType     string
	PaidAmount      int64
}

// RefundResult records a refund carried out outside this system (manual
// transfer now; a provider integration later). Recording it calls nothing.
type RefundResult struct {
	Provider  string // how it was paid out, e.g. "manual" (default)
	Reference string // transfer or provider reference (required)
	Note      string
}

type Return struct {
	ID             int64
	Number         string
	OrderNumber    string
	OrderTotal     int64  // admin view
	BuyerEmail     string // admin view
	BuyerName      string // admin view
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
	Refund         *Refund  // latest refund, if any
	Refunds        []Refund // every refund (admin view)

	// Replacements are the variants an exchange may send (admin view, only
	// while the goods are with the shop).
	Replacements []Replacement
}

// Replacement is a variant offered as an exchange replacement.
type Replacement struct {
	VariantID    int64
	ProductID    int64
	SKU          string
	VariantLabel string
	Stock        int
}

// ReplacementsFor lists the replacement choices for a line (same product).
func (r Return) ReplacementsFor(it ReturnItem) []Replacement {
	var out []Replacement
	for _, v := range r.Replacements {
		if v.ProductID == it.ProductID {
			out = append(out, v)
		}
	}
	return out
}

// ApprovedQuantity is the number of units the shop accepted.
func (r Return) ApprovedQuantity() int {
	n := 0
	for _, it := range r.Items {
		n += it.ApprovedQuantity()
	}
	return n
}

// CanRestock reports whether some line can still be put back on sale.
func (r Return) CanRestock() bool {
	if r.Status != StatusItemReceived && r.Status != StatusRefundPending && r.Status != StatusRefunded &&
		r.Status != StatusExchanged && r.Status != StatusCompleted {
		return false
	}
	for _, it := range r.Items {
		if it.CanRestock() {
			return true
		}
	}
	return false
}

// OpenRefund is the refund still to be carried out, or nil.
func (r Return) OpenRefund() *Refund {
	for i := range r.Refunds {
		if r.Refunds[i].Status.Open() {
			return &r.Refunds[i]
		}
	}
	return nil
}

// CanPrepareRefund: goods received (or an earlier refund failed) and no
// refund is open or paid.
func (r Return) CanPrepareRefund() bool {
	if r.Type != TypeRefund {
		return false
	}
	if r.Status == StatusItemReceived {
		return true
	}
	if r.Status != StatusRefundPending {
		return false
	}
	for _, f := range r.Refunds {
		if f.Status.Open() || f.Status == RefundSucceeded {
			return false
		}
	}
	return true
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

// OrderFacts are what the order detail page already loaded about an
// order, so return eligibility needs no extra query.
type OrderFacts struct {
	ID          int64
	Status      string
	Paid        bool
	CompletedAt *time.Time
	Returnable  bool // some line still has units that may be returned
}

// OrderInfo is what the order detail page needs about returns.
type OrderInfo struct {
	Eligible bool       // "Ajukan Pengembalian" may be shown
	Deadline *time.Time // end of the return window
	Returns  []OrderReturn
}

// ApproveInput is the shop's review: the approved units per return line
// (0..requested) and a note for the buyer. Nothing else is read from the
// form; the refund amount is computed from the prices paid.
type ApproveInput struct {
	Quantities map[int64]int // return line ID → approved units
	Note       string
}

// ReceiveInput records the inspection of the received goods per line.
type ReceiveInput struct {
	Conditions map[int64]Condition // return line ID → condition
	Note       string
}

// ExchangeInput names the replacement variant per approved line.
type ExchangeInput struct {
	Replacements map[int64]int64 // return line ID → variant ID
	Note         string
}

// AdminTab is one filter of the admin return list.
type AdminTab struct {
	Key      string
	Label    string
	Statuses []Status
}

// AdminTabs are the admin return list filters, in display order.
var AdminTabs = []AdminTab{
	{Key: "", Label: "Semua"},
	{Key: "review", Label: "Perlu Ditinjau", Statuses: []Status{StatusRequested}},
	{Key: "progress", Label: "Menunggu Barang", Statuses: []Status{StatusApproved, StatusWaitingReturn}},
	{Key: "received", Label: "Barang Diterima", Statuses: []Status{StatusItemReceived}},
	{Key: "refund", Label: "Refund", Statuses: []Status{StatusRefundPending, StatusRefunded}},
	{Key: "exchange", Label: "Ditukar", Statuses: []Status{StatusExchanged}},
	{Key: "done", Label: "Selesai", Statuses: []Status{StatusCompleted}},
	{Key: "closed", Label: "Ditolak/Batal", Statuses: []Status{StatusRejected, StatusCancelled}},
}

// AdminTabByKey returns the tab for key, or "Semua".
func AdminTabByKey(key string) AdminTab {
	for _, t := range AdminTabs {
		if t.Key == key {
			return t
		}
	}
	return AdminTabs[0]
}

// RefundTab is one filter of the admin refund list.
type RefundTab struct {
	Key      string
	Label    string
	Statuses []RefundStatus
}

var RefundTabs = []RefundTab{
	{Key: "", Label: "Semua"},
	{Key: "open", Label: "Perlu Diproses", Statuses: []RefundStatus{RefundPending, RefundApproved, RefundProcessing}},
	{Key: "succeeded", Label: "Berhasil", Statuses: []RefundStatus{RefundSucceeded}},
	{Key: "failed", Label: "Gagal", Statuses: []RefundStatus{RefundFailed, RefundCancelled}},
}

func RefundTabByKey(key string) RefundTab {
	for _, t := range RefundTabs {
		if t.Key == key {
			return t
		}
	}
	return RefundTabs[0]
}

const (
	AdminPageSize    = 20
	MaxAdminPageSize = 100
)

// AdminFilter selects the admin list; Query searches return/order numbers.
type AdminFilter struct {
	Tab      string
	Query    string
	Page     int
	PageSize int
}

type AdminPage struct {
	Returns []Summary
	Refunds []Refund
	Filter  AdminFilter
	HasNext bool
}

// ShopActorLabel names who made a timeline step, for the admin.
func (e Event) ShopActorLabel() string {
	switch e.Actor {
	case ActorUser:
		return "Pembeli"
	case ActorAdmin:
		return "Admin"
	}
	return "Sistem"
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
