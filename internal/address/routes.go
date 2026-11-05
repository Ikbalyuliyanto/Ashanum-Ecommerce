package address

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/middleware"
)

// Routes must be mounted behind auth.RequireUser. HTML forms only send GET
// and POST, so edits and actions are POSTs to explicit paths. The area
// search calls the shipping provider (monthly quota): rate limited.
func (h *Handler) Routes(r chi.Router) {
	limit := middleware.RateLimit(40, time.Minute, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.view.Error(w, req, http.StatusTooManyRequests)
	}))
	r.With(limit).Get("/account/addresses/areas", h.Areas)
	r.Get("/account/addresses", h.List)
	r.Get("/account/addresses/new", h.NewForm)
	r.Post("/account/addresses", h.Create)
	r.Get("/account/addresses/{id}/edit", h.EditForm)
	r.Post("/account/addresses/{id}", h.Update)
	r.Post("/account/addresses/{id}/primary", h.SetPrimary)
	r.Post("/account/addresses/{id}/delete", h.Delete)
}
