package shipping

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/middleware"
)

// maxWebhookBytes bounds a provider notification body (they are well under
// 2 KiB).
const maxWebhookBytes = 64 << 10

// WebhookHandler receives provider notifications at
// /webhooks/shipping/<provider>.
type WebhookHandler struct {
	svc *Service
	log *slog.Logger
}

func NewWebhookHandler(svc *Service, log *slog.Logger) *WebhookHandler {
	return &WebhookHandler{svc: svc, log: log}
}

// Routes are public: notifications carry no session or CSRF token and
// authenticate with the provider's shared secret. Rate limited per IP.
func (h *WebhookHandler) Routes(r chi.Router) {
	limit := middleware.RateLimit(300, time.Minute, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	r.With(limit).Post("/webhooks/shipping/biteship", h.biteship)
}

// biteship handles Biteship order webhooks. Non-2xx answers make the
// provider retry, so only permanent problems get 4xx; an unknown shipment
// is acknowledged (200) and logged, it will never become known.
func (h *WebhookHandler) biteship(w http.ResponseWriter, r *http.Request) {
	if h.svc.ProviderName() != "biteship" {
		http.Error(w, "not configured", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	res, err := h.svc.HandleWebhook(r.Context(), r.Header, body)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"result":"`+string(res)+`"}`)
	case errors.Is(err, ErrInvalidSignature):
		h.log.Warn("shipping webhook: invalid signature", "ip", middleware.ClientIP(r))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
	case errors.Is(err, ErrMalformed):
		http.Error(w, "malformed notification", http.StatusBadRequest)
	case errors.Is(err, ErrNotConfigured):
		http.Error(w, "not configured", http.StatusNotFound)
	default:
		h.log.Error("shipping webhook", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
