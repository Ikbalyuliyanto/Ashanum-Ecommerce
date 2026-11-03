package order

import "github.com/go-chi/chi/v5"

// Routes must be mounted behind auth.RequireUser.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/account/orders", h.History)
	r.Get("/account/orders/{number}", h.Detail)
}
