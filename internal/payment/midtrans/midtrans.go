// Package midtrans implements payment.Provider for Midtrans: Snap in
// redirect mode (the buyer is sent to the Midtrans hosted payment page, so
// no Midtrans JavaScript runs on our pages) plus the Core API status,
// cancel and expire endpoints. It uses net/http only; the official SDK
// would add a dependency for a few JSON calls.
//
// Only the sandbox is supported: real-money payments stay disabled until
// the real sandbox and the shipping provider have been tested end to end
// and a production payment review is done. The amount is always
// orders.total (items + shipping), with shipping as its own line item.
package midtrans

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ecommerce/internal/payment"
)

const (
	EnvSandbox = "sandbox"

	sandboxSnapURL = "https://app.sandbox.midtrans.com"
	sandboxAPIURL  = "https://api.sandbox.midtrans.com"

	// Midtrans field limits.
	maxItemName = 50
	maxItemID   = 50
)

// wib is the time zone of Midtrans timestamps (transaction_time etc.).
var wib = time.FixedZone("WIB", 7*60*60)

type Config struct {
	Env       string // only EnvSandbox
	ServerKey string // backend only: never rendered, logged or stored
	// SnapURL and APIURL replace the Midtrans endpoints (tests only).
	SnapURL    string
	APIURL     string
	HTTPClient *http.Client
}

type Client struct {
	serverKey string
	snapURL   string
	apiURL    string
	http      *http.Client
}

var _ payment.Provider = (*Client)(nil)

func New(cfg Config) (*Client, error) {
	if cfg.Env != EnvSandbox {
		return nil, fmt.Errorf("midtrans: environment %q is not supported (sandbox only)", cfg.Env)
	}
	if cfg.ServerKey == "" {
		return nil, errors.New("midtrans: server key is required")
	}
	c := &Client{serverKey: cfg.ServerKey, snapURL: sandboxSnapURL, apiURL: sandboxAPIURL, http: cfg.HTTPClient}
	if cfg.SnapURL != "" {
		c.snapURL = strings.TrimRight(cfg.SnapURL, "/")
	}
	if cfg.APIURL != "" {
		c.apiURL = strings.TrimRight(cfg.APIURL, "/")
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	return c, nil
}

func (c *Client) Name() string { return "midtrans" }

// CheckoutOrigin is the origin of the hosted payment page, which the
// Content-Security-Policy form-action must allow (the pay form redirects
// there).
func (c *Client) CheckoutOrigin() string {
	u, err := url.Parse(c.snapURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

type snapRequest struct {
	TransactionDetails snapTransaction `json:"transaction_details"`
	ItemDetails        []snapItem      `json:"item_details,omitempty"`
	CustomerDetails    *snapCustomer   `json:"customer_details,omitempty"`
	Expiry             snapExpiry      `json:"expiry"`
	Callbacks          *snapCallbacks  `json:"callbacks,omitempty"`
	CreditCard         snapCreditCard  `json:"credit_card"`
}

type snapTransaction struct {
	OrderID     string `json:"order_id"`
	GrossAmount int64  `json:"gross_amount"`
}

type snapItem struct {
	ID       string `json:"id"`
	Price    int64  `json:"price"`
	Quantity int    `json:"quantity"`
	Name     string `json:"name"`
}

type snapCustomer struct {
	FirstName string `json:"first_name,omitempty"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
}

type snapExpiry struct {
	StartTime string `json:"start_time"` // "2006-01-02 15:04:05 -0700"
	Unit      string `json:"unit"`
	Duration  int64  `json:"duration"`
}

type snapCallbacks struct {
	Finish string `json:"finish"`
}

type snapCreditCard struct {
	Secure bool `json:"secure"` // 3-D Secure
}

type snapResponse struct {
	Token         string   `json:"token"`
	RedirectURL   string   `json:"redirect_url"`
	ErrorMessages []string `json:"error_messages"`
}

// snapBody builds the Snap request. The item lines must add up to the
// gross amount (Midtrans rejects the request otherwise).
func snapBody(req payment.TransactionRequest) (snapRequest, error) {
	body := snapRequest{
		TransactionDetails: snapTransaction{OrderID: req.ProviderOrderID, GrossAmount: req.Amount},
		Expiry: snapExpiry{
			StartTime: req.Start.In(wib).Format("2006-01-02 15:04:05 -0700"),
			Unit:      "minute",
			Duration:  int64(req.ExpiresAt.Sub(req.Start) / time.Minute),
		},
		CreditCard: snapCreditCard{Secure: true},
	}
	if body.Expiry.Duration < 1 {
		return body, errors.New("midtrans: expiry must be at least one minute")
	}
	var sum int64
	for _, it := range req.Items {
		body.ItemDetails = append(body.ItemDetails, snapItem{
			ID: truncate(it.ID, maxItemID), Price: it.Price, Quantity: it.Quantity, Name: truncate(it.Name, maxItemName),
		})
		sum += it.Price * int64(it.Quantity)
	}
	if len(req.Items) > 0 && sum != req.Amount {
		return body, fmt.Errorf("midtrans: item total %d does not match gross amount %d", sum, req.Amount)
	}
	if c := req.Customer; c != (payment.Customer{}) {
		body.CustomerDetails = &snapCustomer{FirstName: truncate(c.Name, 255), Email: c.Email, Phone: c.Phone}
	}
	if req.FinishURL != "" {
		body.Callbacks = &snapCallbacks{Finish: req.FinishURL}
	}
	return body, nil
}

// CreateTransaction creates a Snap transaction and returns its token and
// hosted payment page URL.
func (c *Client) CreateTransaction(ctx context.Context, req payment.TransactionRequest) (payment.Checkout, error) {
	body, err := snapBody(req)
	if err != nil {
		return payment.Checkout{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return payment.Checkout{}, err
	}
	status, resp, err := c.do(ctx, http.MethodPost, c.snapURL+"/snap/v1/transactions", raw)
	if err != nil {
		return payment.Checkout{}, err
	}
	var out snapResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return payment.Checkout{}, fmt.Errorf("%w: snap response: %v", payment.ErrProviderUnavailable, err)
	}
	if status >= 400 || out.Token == "" || out.RedirectURL == "" {
		return payment.Checkout{}, fmt.Errorf("%w: snap HTTP %d: %s", payment.ErrProviderRejected, status, strings.Join(out.ErrorMessages, "; "))
	}
	return payment.Checkout{Token: out.Token, RedirectURL: out.RedirectURL}, nil
}

// statusResponse is the Core API transaction status (also the shape of
// cancel/expire responses and of notifications).
type statusResponse struct {
	StatusCode        string `json:"status_code"`
	StatusMessage     string `json:"status_message"`
	OrderID           string `json:"order_id"`
	TransactionID     string `json:"transaction_id"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	PaymentType       string `json:"payment_type"`
	GrossAmount       string `json:"gross_amount"`
	TransactionTime   string `json:"transaction_time"`
	SettlementTime    string `json:"settlement_time"`
	SignatureKey      string `json:"signature_key"`
}

// GetStatus returns the current status of a transaction. A transaction
// that does not exist (yet) is Found=false.
func (c *Client) GetStatus(ctx context.Context, providerOrderID string) (payment.ProviderStatus, error) {
	status, raw, err := c.do(ctx, http.MethodGet, c.apiURL+"/v2/"+url.PathEscape(providerOrderID)+"/status", nil)
	if err != nil {
		return payment.ProviderStatus{}, err
	}
	var r statusResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return payment.ProviderStatus{}, fmt.Errorf("%w: status response: %v", payment.ErrProviderUnavailable, err)
	}
	switch {
	case r.StatusCode == "404" || (status == http.StatusNotFound && r.TransactionStatus == ""):
		return payment.ProviderStatus{Found: false}, nil
	case status == http.StatusUnauthorized || r.StatusCode == "401":
		return payment.ProviderStatus{}, fmt.Errorf("%w: unauthorized (check MIDTRANS_SERVER_KEY)", payment.ErrProviderRejected)
	case r.TransactionStatus == "":
		return payment.ProviderStatus{}, fmt.Errorf("%w: status %s: %s", payment.ErrProviderUnavailable, r.StatusCode, r.StatusMessage)
	case r.OrderID != "" && r.OrderID != providerOrderID:
		return payment.ProviderStatus{}, fmt.Errorf("%w: status for %q, asked %q", payment.ErrProviderUnavailable, r.OrderID, providerOrderID)
	}
	return r.translate()
}

// Cancel stops an unpaid transaction: Midtrans "cancel" first, then
// "expire" for methods that cannot be cancelled while pending (bank
// transfer, e-wallets). The returned status always comes from GetStatus.
func (c *Client) Cancel(ctx context.Context, providerOrderID string) (payment.ProviderStatus, error) {
	base := c.apiURL + "/v2/" + url.PathEscape(providerOrderID)
	if _, _, err := c.do(ctx, http.MethodPost, base+"/cancel", nil); err != nil {
		return payment.ProviderStatus{}, err
	}
	st, err := c.GetStatus(ctx, providerOrderID)
	if err != nil || !st.Found || st.Status != payment.StatusPending {
		return st, err
	}
	if _, _, err := c.do(ctx, http.MethodPost, base+"/expire", nil); err != nil {
		return payment.ProviderStatus{}, err
	}
	return c.GetStatus(ctx, providerOrderID)
}

// ParseNotification checks the notification signature:
//
//	signature_key = SHA512(order_id + status_code + gross_amount + server key)
//
// compared in constant time. The status in the notification is not used:
// the caller asks GetStatus.
func (c *Client) ParseNotification(body []byte) (payment.Notification, error) {
	var n statusResponse
	if err := json.Unmarshal(body, &n); err != nil {
		return payment.Notification{}, payment.ErrInvalidNotification
	}
	if n.OrderID == "" || n.StatusCode == "" || n.GrossAmount == "" || n.SignatureKey == "" {
		return payment.Notification{}, payment.ErrInvalidNotification
	}
	if !c.validSignature(n.OrderID, n.StatusCode, n.GrossAmount, n.SignatureKey) {
		return payment.Notification{}, payment.ErrInvalidSignature
	}
	return payment.Notification{ProviderOrderID: n.OrderID, Raw: n.TransactionStatus}, nil
}

func (c *Client) validSignature(orderID, statusCode, grossAmount, signature string) bool {
	want := c.signature(orderID, statusCode, grossAmount)
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(signature))) == 1
}

func (c *Client) signature(orderID, statusCode, grossAmount string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + c.serverKey))
	return hex.EncodeToString(sum[:])
}

// translate maps a Midtrans transaction to the internal status. This is the
// only place that knows Midtrans status names.
func (r statusResponse) translate() (payment.ProviderStatus, error) {
	amount, err := parseAmount(r.GrossAmount)
	if err != nil {
		return payment.ProviderStatus{}, fmt.Errorf("%w: %v", payment.ErrProviderUnavailable, err)
	}
	st := payment.ProviderStatus{
		Found: true, Raw: r.TransactionStatus, Fraud: r.FraudStatus,
		PaymentType: r.PaymentType, TransactionID: r.TransactionID, Amount: amount,
		Status: Status(r.TransactionStatus, r.FraudStatus),
	}
	if st.Status == payment.StatusPaid {
		when := r.SettlementTime
		if when == "" {
			when = r.TransactionTime
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", when, wib); err == nil {
			st.PaidAt = &t
		}
	}
	return st, nil
}

// Status maps a Midtrans transaction_status and fraud_status to the
// internal status, or "" for statuses with no automatic meaning here
// (refund, partial_refund, chargeback, partial_chargeback, unknown).
func Status(transactionStatus, fraudStatus string) payment.Status {
	switch transactionStatus {
	case "settlement":
		return payment.StatusPaid
	case "capture": // card payments
		switch fraudStatus {
		case "", "accept":
			return payment.StatusPaid
		case "challenge": // waits for the merchant's decision in the dashboard
			return payment.StatusPending
		}
		return payment.StatusDenied
	case "pending", "authorize":
		return payment.StatusPending
	case "deny":
		return payment.StatusDenied
	case "cancel":
		return payment.StatusCancelled
	case "expire":
		return payment.StatusExpired
	case "failure":
		return payment.StatusFailed
	}
	return ""
}

// parseAmount reads a Midtrans amount such as "649000.00" (Rupiah has no
// fractional part).
func parseAmount(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if strings.Trim(frac, "0") != "" {
		return 0, fmt.Errorf("unexpected fractional amount %q", s)
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad amount %q", s)
	}
	return n, nil
}

var methodLabels = map[string]string{
	"credit_card":    "Kartu Kredit/Debit",
	"bank_transfer":  "Transfer Bank (Virtual Account)",
	"echannel":       "Mandiri Bill Payment",
	"permata":        "Permata Virtual Account",
	"gopay":          "GoPay",
	"shopeepay":      "ShopeePay",
	"qris":           "QRIS",
	"other_qris":     "QRIS",
	"dana":           "DANA",
	"cstore":         "Gerai Retail",
	"akulaku":        "Akulaku",
	"kredivo":        "Kredivo",
	"bca_klikpay":    "BCA KlikPay",
	"bca_klikbca":    "KlikBCA",
	"cimb_clicks":    "OCTO Clicks",
	"danamon_online": "Danamon Online Banking",
	"bri_epay":       "BRI e-Pay",
	"uob_ezpay":      "UOB EZ Pay",
}

func (c *Client) MethodLabel(paymentType string) string {
	if l, ok := methodLabels[paymentType]; ok {
		return l
	}
	return strings.ReplaceAll(paymentType, "_", " ")
}

// do sends one authenticated request. Network errors and 5xx answers are
// ErrProviderUnavailable; other answers are returned for the caller to read.
// The Authorization header and server key are never logged.
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(c.serverKey, "")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, keep the cause (timeout, refused...)
		}
		return 0, nil, fmt.Errorf("%w: %s %s: %w", payment.ErrProviderUnavailable, method, redact(endpoint), err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read response: %v", payment.ErrProviderUnavailable, err)
	}
	if res.StatusCode >= 500 {
		return res.StatusCode, raw, fmt.Errorf("%w: HTTP %d", payment.ErrProviderUnavailable, res.StatusCode)
	}
	return res.StatusCode, raw, nil
}

// redact keeps only the path of an endpoint for error messages.
func redact(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Path
	}
	return ""
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
