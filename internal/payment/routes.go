package payment

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/middleware"
)

// Routes are the buyer actions; mount them behind auth.RequireUser. Both
// may call the provider, so they are rate limited per client IP.
func (h *Handler) Routes(r chi.Router) {
	limit := middleware.RateLimit(30, 5*time.Minute, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.view.Error(w, req, http.StatusTooManyRequests)
	}))
	r.With(limit).Post("/account/orders/{number}/pay", h.Pay)
	r.With(limit).Post("/account/orders/{number}/cancel", h.Cancel)
}

// WebhookRoutes are public: notifications authenticate with their signature.
func (h *Handler) WebhookRoutes(r chi.Router) {
	limit := middleware.RateLimit(300, time.Minute, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	r.With(limit).Post("/webhooks/midtrans", h.Webhook)
}
