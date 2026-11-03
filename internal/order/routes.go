package order

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/middleware"
)

// Routes must be mounted behind auth.RequireUser.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/account/orders", h.History)
	r.Get("/account/orders/{number}", h.Detail)
	// The finish page asks the payment provider for the status.
	finishLimit := middleware.RateLimit(60, 5*time.Minute, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.view.Error(w, req, http.StatusTooManyRequests)
	}))
	r.With(finishLimit).Get("/account/orders/{number}/payment/finish", h.PaymentFinish)
}
