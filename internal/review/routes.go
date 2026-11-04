package review

import "github.com/go-chi/chi/v5"

// Routes are the buyer's review pages; mount them behind auth.RequireUser.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/account/reviews", h.History)
	r.Get("/account/reviews/new", h.NewForm)
	r.Post("/account/reviews", h.Create)
	r.Get("/account/reviews/{id}/edit", h.EditForm)
	r.Post("/account/reviews/{id}", h.Update)
}

// PublicRoutes are the storefront review pages.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/products/{slug}/reviews", h.ProductReviews)
}
