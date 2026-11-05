package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/address"
	"ecommerce/internal/auth"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/media"
	"ecommerce/internal/order"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/returns"
	"ecommerce/internal/review"
	"ecommerce/internal/shipping"
	"ecommerce/internal/user"
	"ecommerce/internal/validate"
)

// ---------------------------------------------------------------- envelope

type envelope struct {
	Data any `json:"data"`
	Meta any `json:"meta,omitempty"`
}

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// pageMeta is the pagination contract of every list: page-based, has_more
// always; total only where it is computed anyway (products).
type pageMeta struct {
	Page    int  `json:"page"`
	PerPage int  `json:"per_page"`
	Total   *int `json:"total,omitempty"`
	HasMore bool `json:"has_more"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, status int, data any) { writeJSON(w, status, envelope{Data: data}) }

func okPage(w http.ResponseWriter, data any, meta any) {
	writeJSON(w, http.StatusOK, envelope{Data: data, Meta: meta})
}

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func failWith(w http.ResponseWriter, status int, code, msg string, fields map[string]string) {
	writeJSON(w, status, errorBody{Error: apiError{Code: code, Message: msg, Fields: fields}})
}

// ---------------------------------------------------------------- errors

// problem is a request error found by this package (decoding, params).
type problem struct {
	status int
	code   string
	msg    string
	fields map[string]string
}

func (p *problem) Error() string { return p.code + ": " + p.msg }

func badRequest(msg string, fields map[string]string) error {
	return &problem{status: http.StatusBadRequest, code: "INVALID_REQUEST", msg: msg, fields: fields}
}

func invalid(fields map[string]string) error {
	return &problem{status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR", msg: "Periksa kembali data yang dikirim.", fields: fields}
}

func isAuthError(err error) bool {
	return errors.Is(err, auth.ErrInvalidToken) || errors.Is(err, auth.ErrTokenExpired) || errors.Is(err, auth.ErrRefreshReused)
}

// fail maps a service error to the error contract. Messages are the
// customer-facing texts of the domain packages; unknown errors are logged
// and answered as a generic 500 — never a database, provider or Go error.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		p    *problem
		verr validate.Errors
		qe   *cart.QuantityError
	)
	switch {
	case errors.As(err, &p):
		failWith(w, p.status, p.code, p.msg, p.fields)
	case errors.As(err, &verr):
		failWith(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Periksa kembali data yang dikirim.", map[string]string(verr))
	case errors.As(err, &qe):
		failWith(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", cart.Message(err), map[string]string{"quantity": cart.Message(err)})

	case errors.Is(err, auth.ErrInvalidCredentials):
		failWith(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email atau password salah.", nil)
	case errors.Is(err, auth.ErrEmailTaken):
		failWith(w, http.StatusConflict, "EMAIL_TAKEN", "Email sudah terdaftar.", map[string]string{"email": "Email sudah terdaftar. Silakan masuk."})
	case errors.Is(err, auth.ErrTokenExpired):
		failWith(w, http.StatusUnauthorized, "TOKEN_EXPIRED", "Token akses kedaluwarsa. Perbarui dengan refresh token.", nil)
	case errors.Is(err, auth.ErrRefreshReused):
		failWith(w, http.StatusUnauthorized, "REFRESH_TOKEN_REUSED", "Refresh token sudah pernah dipakai. Demi keamanan semua token perangkat ini dicabut; silakan masuk lagi.", nil)
	case errors.Is(err, auth.ErrInvalidToken):
		failWith(w, http.StatusUnauthorized, "INVALID_TOKEN", "Token tidak valid atau sudah dicabut. Silakan masuk lagi.", nil)

	case errors.Is(err, address.ErrNotFound), errors.Is(err, cart.ErrItemNotFound), errors.Is(err, order.ErrNotFound),
		errors.Is(err, payment.ErrNotFound), errors.Is(err, product.ErrNotFound), errors.Is(err, review.ErrNotFound),
		errors.Is(err, returns.ErrNotFound), errors.Is(err, category.ErrNotFound), errors.Is(err, user.ErrNotFound):
		failWith(w, http.StatusNotFound, "NOT_FOUND", "Data tidak ditemukan.", nil)
	case errors.Is(err, address.ErrLimitReached):
		failWith(w, http.StatusUnprocessableEntity, "ADDRESS_LIMIT", "Jumlah alamat sudah mencapai batas. Hapus alamat lain terlebih dahulu.", nil)

	case errors.Is(err, cart.ErrUnavailable):
		failWith(w, http.StatusUnprocessableEntity, "PRODUCT_UNAVAILABLE", cart.Message(err), map[string]string{"variant_id": cart.Message(err)})
	case errors.Is(err, cart.ErrCartFull):
		failWith(w, http.StatusUnprocessableEntity, "CART_FULL", cart.Message(err), nil)

	case errors.Is(err, checkout.ErrStockChanged):
		failWith(w, http.StatusConflict, "STOCK_CHANGED", checkout.Message(err), nil)
	case errors.Is(err, checkout.ErrUnavailable):
		failWith(w, http.StatusConflict, "PRODUCT_UNAVAILABLE", checkout.Message(err), nil)
	case errors.Is(err, checkout.ErrPriceChanged):
		failWith(w, http.StatusConflict, "PRICE_CHANGED", checkout.Message(err), nil)
	case errors.Is(err, checkout.ErrAddressRequired):
		failWith(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", checkout.Message(err), map[string]string{"address_id": checkout.Message(err)})
	case errors.Is(err, checkout.ErrNothingToCheckout):
		failWith(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", checkout.Message(err), map[string]string{"cart_item_ids": checkout.Message(err)})
	case errors.Is(err, checkout.ErrNotShippable):
		failWith(w, http.StatusConflict, "ITEMS_NOT_SHIPPABLE", checkout.Message(err), nil)
	case errors.Is(err, checkout.ErrShippingRequired):
		failWith(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", checkout.Message(err), map[string]string{"shipping_quote_id": checkout.Message(err)})
	case errors.Is(err, checkout.ErrAreaRequired):
		failWith(w, http.StatusUnprocessableEntity, "SHIPPING_AREA_REQUIRED", checkout.Message(err), map[string]string{"address_id": checkout.Message(err)})

	case errors.Is(err, shipping.ErrQuoteChanged):
		failWith(w, http.StatusConflict, "SHIPPING_CHANGED", shipping.Message(err), map[string]string{"shipping_quote_id": shipping.Message(err)})
	case errors.Is(err, shipping.ErrInvalidQuery):
		failWith(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", shipping.Message(err), map[string]string{"q": shipping.Message(err)})
	case errors.Is(err, shipping.ErrNoCourier):
		failWith(w, http.StatusUnprocessableEntity, "NO_COURIER", shipping.Message(err), nil)
	case errors.Is(err, shipping.ErrPackageInvalid):
		failWith(w, http.StatusConflict, "ITEMS_NOT_SHIPPABLE", shipping.Message(err), nil)
	case errors.Is(err, shipping.ErrNotConfigured), errors.Is(err, shipping.ErrUnavailable), errors.Is(err, shipping.ErrRejected):
		failWith(w, http.StatusServiceUnavailable, "SHIPPING_PROVIDER_UNAVAILABLE", shipping.Message(shipping.ErrUnavailable), nil)

	case errors.Is(err, payment.ErrNotConfigured):
		failWith(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", payment.Message(err), nil)
	case errors.Is(err, payment.ErrProviderUnavailable), errors.Is(err, payment.ErrProviderRejected), errors.Is(err, payment.ErrCancelUnavailable):
		failWith(w, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", payment.Message(err), nil)
	case errors.Is(err, payment.ErrAlreadyPaid):
		failWith(w, http.StatusConflict, "ALREADY_PAID", payment.Message(err), nil)
	case errors.Is(err, payment.ErrNotPayable), errors.Is(err, payment.ErrOrderExpired), errors.Is(err, payment.ErrTooManyAttempts),
		errors.Is(err, payment.ErrNotCancellable), errors.Is(err, payment.ErrInvalidAmount):
		failWith(w, http.StatusConflict, "INVALID_STATE", payment.Message(err), nil)

	case errors.Is(err, review.ErrNotEligible):
		failWith(w, http.StatusConflict, "REVIEW_NOT_ALLOWED", review.Message(err), nil)
	case errors.Is(err, review.ErrAlreadyReviewed):
		failWith(w, http.StatusConflict, "ALREADY_REVIEWED", review.Message(err), nil)

	case errors.Is(err, returns.ErrOrderNotCompleted), errors.Is(err, returns.ErrOrderNotPaid),
		errors.Is(err, returns.ErrWindowClosed), errors.Is(err, returns.ErrNothingToReturn):
		failWith(w, http.StatusConflict, "RETURN_NOT_ALLOWED", returns.Message(err), nil)
	case errors.Is(err, returns.ErrInvalidTransition):
		failWith(w, http.StatusConflict, "INVALID_STATE", returns.Message(err), nil)

	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the answer.
	default:
		h.Log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		failWith(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan pada server.", nil)
	}
}

// ---------------------------------------------------------------- decoding

// maxBody bounds every JSON request (the largest, a return with five
// evidence links and a description, is a few KiB).
const maxBody = 64 << 10

// decode reads a JSON request body strictly: Content-Type must be JSON, the
// body at most maxBody, exactly one value, and no unknown field (a typo in
// a mobile app fails loudly instead of being ignored).
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &problem{status: http.StatusUnsupportedMediaType, code: "UNSUPPORTED_MEDIA_TYPE", msg: "Kirim body sebagai application/json."}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return jsonProblem(err)
	}
	if dec.More() {
		return badRequest("Body harus berisi satu objek JSON.", nil)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return jsonProblem(err)
		}
		return badRequest("Body harus berisi satu objek JSON.", nil)
	}
	return nil
}

func jsonProblem(err error) error {
	var (
		mbe *http.MaxBytesError
		ute *json.UnmarshalTypeError
		se  *json.SyntaxError
	)
	switch {
	case errors.As(err, &mbe):
		return &problem{status: http.StatusRequestEntityTooLarge, code: "PAYLOAD_TOO_LARGE",
			msg: fmt.Sprintf("Body terlalu besar (maksimal %d KiB).", maxBody>>10)}
	case errors.As(err, &ute):
		field := ute.Field
		if field == "" {
			return badRequest("Body harus berupa objek JSON.", nil)
		}
		return badRequest("Tipe data tidak sesuai.", map[string]string{field: "Tipe data harus " + jsonKind(ute.Type.Kind().String()) + "."})
	case errors.As(err, &se), errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("JSON tidak valid.", nil)
	case errors.Is(err, io.EOF):
		return badRequest("Body JSON wajib diisi.", nil)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return &problem{status: http.StatusBadRequest, code: "UNKNOWN_FIELD", msg: "Ada field yang tidak dikenal.",
			fields: map[string]string{field: "Field tidak dikenal."}}
	}
	return badRequest("JSON tidak valid.", nil)
}

func jsonKind(k string) string {
	switch k {
	case "int", "int32", "int64", "uint", "float64":
		return "angka"
	case "string":
		return "teks"
	case "bool":
		return "boolean"
	case "slice":
		return "array"
	}
	return "objek"
}

// ---------------------------------------------------------------- params

func pathID(r *http.Request, key string) (int64, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || n <= 0 {
		return 0, &problem{status: http.StatusNotFound, code: "NOT_FOUND", msg: "Data tidak ditemukan."}
	}
	return n, nil
}

// queryInt reads an optional integer query parameter (0 when absent).
func queryInt(r *http.Request, key string) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, badRequest("Parameter tidak valid.", map[string]string{key: "Harus bilangan bulat ≥ 0."})
	}
	return n, nil
}

// ---------------------------------------------------------------- values

// wib is the shop's zone: API times are RFC 3339 with +07:00.
var wib = time.FixedZone("WIB", 7*60*60)

func ts(t time.Time) string { return t.In(wib).Format(time.RFC3339) }

func tsp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

// Image widths for mobile (Cloudinary transformation via media.Resize;
// other URLs are returned as stored).
const (
	thumbWidth  = 200
	cardWidth   = 400
	detailWidth = 900
)

func img(url string, width int) string {
	if url == "" {
		return ""
	}
	return media.Resize(url, width)
}

func nonZero(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
