package product

import "github.com/go-chi/chi/v5"

// Routes registers the public storefront catalog pages.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/products/{slug}", h.Detail)
	r.Get("/search", h.Search)
	r.Get("/categories/{slug}", h.CategoryProducts)
}
