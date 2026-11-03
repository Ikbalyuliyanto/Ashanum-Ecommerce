package cart

import "github.com/go-chi/chi/v5"

// Routes must be mounted behind auth.RequireUser; changes are POSTs, which
// CrossOriginProtection guards against cross-site requests.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/cart", h.Show)
	r.Post("/cart/items", h.Add)
	r.Post("/cart/items/{id}", h.Update)
	r.Post("/cart/items/{id}/delete", h.Remove)
	r.Post("/cart/clear", h.Clear)
}
