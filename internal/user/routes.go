package user

import "github.com/go-chi/chi/v5"

// Routes must be mounted behind auth.RequireUser.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/account", h.Account)
	r.Get("/account/profile", h.ProfileForm)
	r.Post("/account/profile", h.UpdateProfile)
}
