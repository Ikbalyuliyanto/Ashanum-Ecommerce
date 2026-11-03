package payment

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/auth"
	"ecommerce/internal/middleware"
	"ecommerce/internal/view"
)

// maxNotificationBytes bounds a provider notification body.
const maxNotificationBytes = 64 << 10

type Handler struct {
	svc  *Service
	view *view.Renderer
	log  *slog.Logger
}

func NewHandler(svc *Service, v *view.Renderer, log *slog.Logger) *Handler {
	return &Handler{svc: svc, view: v, log: log}
}

// Pay handles "Bayar Sekarang": it sends the buyer to the provider
// checkout (303), or back to the order with a message.
func (h *Handler) Pay(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	number := chi.URLParam(r, "number")
	target, err := h.svc.Pay(r.Context(), p.UserID, number)
	if err != nil {
		h.fail(w, r, number, err)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// Cancel handles "Batalkan Pesanan" for an unpaid order.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	number := chi.URLParam(r, "number")
	if err := h.svc.CancelOrder(r.Context(), p.UserID, number); err != nil {
		h.fail(w, r, number, err)
		return
	}
	h.view.SetFlash(w, "success", "Pesanan berhasil dibatalkan.")
	http.Redirect(w, r, orderPath(number), http.StatusSeeOther)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, number string, err error) {
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	msg := Message(err)
	if msg == "" {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.SetFlash(w, "error", msg)
	http.Redirect(w, r, orderPath(number), http.StatusSeeOther)
}

// Webhook receives provider notifications. It uses no session and no
// browser CSRF check: a notification is authenticated by its signature,
// then verified with the provider. Non-2xx answers make the provider retry,
// so only permanent problems get 4xx.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNotificationBytes))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	err = h.svc.HandleNotification(r.Context(), body)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "OK")
	case errors.Is(err, ErrInvalidSignature):
		h.log.Warn("payment webhook: invalid signature", "ip", middleware.ClientIP(r))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
	case errors.Is(err, ErrInvalidNotification):
		http.Error(w, "malformed notification", http.StatusBadRequest)
	case errors.Is(err, ErrUnknownPayment), errors.Is(err, ErrNotConfigured):
		http.Error(w, "unknown transaction", http.StatusNotFound)
	case errors.Is(err, ErrProviderUnavailable), errors.Is(err, ErrProviderRejected):
		h.log.Warn("payment webhook: provider status unavailable, provider will retry", "error", err)
		http.Error(w, "try again later", http.StatusServiceUnavailable)
	default:
		h.log.Error("payment webhook", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func orderPath(number string) string { return "/account/orders/" + url.PathEscape(number) }
