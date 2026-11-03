package address

import "github.com/go-chi/chi/v5"

// Routes must be mounted behind auth.RequireUser. HTML forms only send GET
// and POST, so edits and actions are POSTs to explicit paths.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/account/addresses", h.List)
	r.Get("/account/addresses/new", h.NewForm)
	r.Post("/account/addresses", h.Create)
	r.Get("/account/addresses/{id}/edit", h.EditForm)
	r.Post("/account/addresses/{id}", h.Update)
	r.Post("/account/addresses/{id}/primary", h.SetPrimary)
	r.Post("/account/addresses/{id}/delete", h.Delete)
}
