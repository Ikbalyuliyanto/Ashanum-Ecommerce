package checkout

import "github.com/go-chi/chi/v5"

// Routes must be mounted behind auth.RequireUser. The checkout page is a
// GET (refreshable; the source is in the URL), placing the order a POST
// guarded by CrossOriginProtection.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/checkout", h.Show)
	r.Post("/checkout/order", h.Place)
	r.Get("/checkout/success/{number}", h.Success)
}
