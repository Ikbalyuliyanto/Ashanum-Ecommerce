package returns

import "github.com/go-chi/chi/v5"

// Routes are the buyer's return pages; mount them behind auth.RequireUser.
// Approving, rejecting and the other admin steps have no route in this
// phase: the admin phase calls the Service methods.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/account/returns", h.History)
	r.Get("/account/returns/new", h.NewForm)
	r.Post("/account/returns", h.Create)
	r.Get("/account/returns/{number}", h.Detail)
	r.Post("/account/returns/{number}/cancel", h.Cancel)
}
