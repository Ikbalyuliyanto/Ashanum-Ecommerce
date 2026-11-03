// Package payment takes payments for unpaid orders and owns the rest of
// the unpaid-order lifecycle: payment attempts, provider notifications,
// cancellation, payment expiry and stock restoration.
//
// The payment provider (Midtrans in this phase, sandbox only) sits behind
// the Provider interface; the Service never sees provider HTTP details and
// has no dependency on HTTP handlers, so a future API can reuse it.
//
// The provider is the source of truth for whether money was paid: browser
// redirects are only UX. Status changes come from signed notifications or
// from asking the provider (GetStatus), and every change runs in a database
// transaction guarded by the transition policy (see decide).
package payment

import (
	"context"
	"errors"
	"time"
)

// Status is the internal payment attempt status. Provider statuses are
// translated into it by the Provider implementation.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusPaid      Status = "PAID"
	StatusFailed    Status = "FAILED"
	StatusDenied    Status = "DENIED"
	StatusCancelled Status = "CANCELLED"
	StatusExpired   Status = "EXPIRED"
)

// Label is the customer-facing status (Indonesian).
func (s Status) Label() string {
	switch s {
	case StatusPending:
		return "Menunggu Pembayaran"
	case StatusPaid:
		return "Berhasil"
	case StatusFailed:
		return "Gagal"
	case StatusDenied:
		return "Ditolak"
	case StatusCancelled:
		return "Dibatalkan"
	case StatusExpired:
		return "Kedaluwarsa"
	}
	return string(s)
}

// Unsuccessful reports a closed attempt that did not take money.
func (s Status) Unsuccessful() bool {
	return s == StatusFailed || s == StatusDenied || s == StatusCancelled || s == StatusExpired
}

// Attempt is one payment attempt of an order. Every attempt has its own
// provider order ID; a closed attempt is never reopened.
type Attempt struct {
	ID              int64
	OrderID         int64
	No              int
	ProviderOrderID string
	Amount          int64
	Status          Status
	PaymentType     string // provider payment type, "" until the buyer picks a method
	Method          string // customer-facing name of PaymentType
	RedirectURL     string // provider checkout page of a pending attempt
	NeedsReview     bool
	ExpiresAt       time.Time
	PaidAt          *time.Time
	CreatedAt       time.Time
}

// Provider is a payment provider with a hosted checkout page.
type Provider interface {
	// Name identifies the provider in payments.provider, e.g. "midtrans".
	Name() string
	// CreateTransaction starts the provider checkout for one attempt.
	CreateTransaction(ctx context.Context, req TransactionRequest) (Checkout, error)
	// GetStatus asks the provider for the current status of a transaction.
	GetStatus(ctx context.Context, providerOrderID string) (ProviderStatus, error)
	// Cancel stops a transaction that is not paid, where the provider allows
	// it, and returns the provider's status afterwards.
	Cancel(ctx context.Context, providerOrderID string) (ProviderStatus, error)
	// ParseNotification authenticates a provider notification (signature)
	// and returns the transaction it is about.
	ParseNotification(body []byte) (Notification, error)
	// MethodLabel is the customer-facing name of a provider payment type.
	MethodLabel(paymentType string) string
}

// TransactionRequest is what a provider needs to open a checkout.
type TransactionRequest struct {
	ProviderOrderID string
	Amount          int64 // always orders.total
	Items           []LineItem
	Customer        Customer
	Start           time.Time
	ExpiresAt       time.Time
	FinishURL       string // where the provider sends the buyer back; "" for none
}

// LineItem is one line shown on the provider checkout; lines add up to the
// amount (products, shipping cost, and discount as a negative price).
type LineItem struct {
	ID       string
	Name     string
	Price    int64
	Quantity int
}

type Customer struct {
	Name  string
	Email string
	Phone string
}

// Checkout is the provider checkout of an attempt.
type Checkout struct {
	Token       string
	RedirectURL string
}

// ProviderStatus is a provider's view of one transaction, already
// translated to an internal Status.
type ProviderStatus struct {
	// Found is false when the provider has no transaction for the ID, e.g.
	// the buyer opened the checkout page but did not choose a method.
	Found bool
	// Status is "" when the provider status has no automatic meaning here
	// (refund, chargeback, unknown): such reports are flagged for review.
	Status        Status
	Raw           string // provider's own status, e.g. "settlement"
	Fraud         string
	PaymentType   string
	TransactionID string
	Amount        int64
	PaidAt        *time.Time
}

// Notification is an authenticated provider notification. It only says
// which transaction changed; the status itself is fetched with GetStatus.
type Notification struct {
	ProviderOrderID string
	Raw             string
}

// ReviewItem is a payment the system would not settle automatically.
type ReviewItem struct {
	OrderNumber     string
	ProviderOrderID string
	Status          Status
	ProviderStatus  string
	Reason          string
	UpdatedAt       time.Time
}

// Report is the result of one Reconcile run.
type Report struct {
	Checked int // pending attempts verified with the provider
	Updated int // attempts whose status changed
	Expired int // overdue unpaid orders cancelled, stock restored
	Errors  int // provider or database errors (left for the next run)
	Review  []ReviewItem
}

var (
	ErrNotFound          = errors.New("payment: order not found")
	ErrNotConfigured     = errors.New("payment: provider not configured")
	ErrNotPayable        = errors.New("payment: order is not waiting for payment")
	ErrOrderExpired      = errors.New("payment: payment deadline has passed")
	ErrTooManyAttempts   = errors.New("payment: too many payment attempts")
	ErrInvalidAmount     = errors.New("payment: order amounts are inconsistent")
	ErrAlreadyPaid       = errors.New("payment: order is already paid")
	ErrNotCancellable    = errors.New("payment: order can no longer be cancelled")
	ErrCancelUnavailable = errors.New("payment: provider cancellation could not be confirmed")

	// Provider errors, returned by Provider implementations.
	ErrProviderUnavailable = errors.New("payment: provider unavailable")
	ErrProviderRejected    = errors.New("payment: provider rejected the request")
	ErrInvalidNotification = errors.New("payment: malformed notification")
	ErrInvalidSignature    = errors.New("payment: invalid notification signature")
	ErrUnknownPayment      = errors.New("payment: unknown provider order id")
)

// Message is the customer-facing text for a known error, or "" for
// unexpected errors (which become a 500 page). Raw provider errors are
// logged, never shown.
func Message(err error) string {
	switch {
	case errors.Is(err, ErrNotConfigured):
		return "Pembayaran online belum tersedia."
	case errors.Is(err, ErrNotPayable):
		return "Pesanan ini tidak lagi menunggu pembayaran."
	case errors.Is(err, ErrOrderExpired):
		return "Batas waktu pembayaran pesanan ini sudah habis."
	case errors.Is(err, ErrTooManyAttempts):
		return "Batas percobaan pembayaran tercapai. Silakan buat pesanan baru."
	case errors.Is(err, ErrInvalidAmount):
		return "Total pesanan tidak valid. Silakan hubungi kami."
	case errors.Is(err, ErrAlreadyPaid):
		return "Pesanan sudah dibayar dan tidak dapat dibatalkan."
	case errors.Is(err, ErrNotCancellable):
		return "Pesanan ini tidak dapat dibatalkan."
	case errors.Is(err, ErrCancelUnavailable):
		return "Pesanan belum dapat dibatalkan saat ini. Silakan coba kembali sebentar lagi."
	case errors.Is(err, ErrProviderUnavailable), errors.Is(err, ErrProviderRejected):
		return "Pembayaran belum berhasil. Silakan coba kembali."
	}
	return ""
}
