package auth

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/middleware"
)

// Routes registers the public auth pages. Login and registration attempts
// are rate limited per client IP (generous enough for carrier-grade NAT).
func (h *Handler) Routes(r chi.Router) {
	tooMany := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.view.Error(w, req, http.StatusTooManyRequests)
	})
	loginLimit := middleware.RateLimit(20, 5*time.Minute, tooMany)
	registerLimit := middleware.RateLimit(10, 10*time.Minute, tooMany)

	r.Group(func(r chi.Router) {
		r.Use(RedirectIfSignedIn)
		r.Get("/login", h.LoginForm)
		r.With(loginLimit).Post("/login", h.Login)
		r.Get("/register", h.RegisterForm)
		r.With(registerLimit).Post("/register", h.Register)
	})
	r.Post("/logout", h.Logout)
}
