package category

import "github.com/go-chi/chi/v5"

// Routes registers /categories. Product listings per category
// (/categories/{slug}) belong to the product module.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/categories", h.Index)
}
