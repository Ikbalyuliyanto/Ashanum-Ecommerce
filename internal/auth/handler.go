package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

// CookieConfig describes the session cookie. In production the __Host-
// prefix makes browsers reject it unless it is Secure, host-only and Path=/.
type CookieConfig struct {
	Name   string
	Secure bool
}

func NewCookieConfig(production bool) CookieConfig {
	if production {
		return CookieConfig{Name: "__Host-sid", Secure: true}
	}
	return CookieConfig{Name: "sid", Secure: false}
}

type Handler struct {
	svc    *Service
	view   *view.Renderer
	cookie CookieConfig
	log    *slog.Logger
}

func NewHandler(svc *Service, v *view.Renderer, cookie CookieConfig, log *slog.Logger) *Handler {
	return &Handler{svc: svc, view: v, cookie: cookie, log: log}
}

type loginPage struct {
	Email string
	Next  string
	Error string
}

type registerPage struct {
	Email  string
	Next   string
	Errors validate.Errors
}

func (h *Handler) LoginForm(w http.ResponseWriter, r *http.Request) {
	h.view.Render(w, r, http.StatusOK, "auth/login", loginPage{Next: safeNext(r.URL.Query().Get("next"))})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	email, next := r.PostForm.Get("email"), safeNext(r.PostForm.Get("next"))

	_, sess, err := h.svc.Login(r.Context(), email, r.PostForm.Get("password"))
	if errors.Is(err, ErrInvalidCredentials) {
		h.view.Render(w, r, http.StatusUnprocessableEntity, "auth/login", loginPage{
			Email: email, Next: next, Error: "Email atau password salah.",
		})
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}

	h.setSessionCookie(w, sess)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *Handler) RegisterForm(w http.ResponseWriter, r *http.Request) {
	h.view.Render(w, r, http.StatusOK, "auth/register", registerPage{Next: safeNext(r.URL.Query().Get("next"))})
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	page := registerPage{Email: r.PostForm.Get("email"), Next: safeNext(r.PostForm.Get("next"))}

	u, err := h.svc.Register(r.Context(), RegisterInput{
		Email:           page.Email,
		Password:        r.PostForm.Get("password"),
		PasswordConfirm: r.PostForm.Get("password_confirm"),
	})
	switch {
	case errors.As(err, &page.Errors):
	case errors.Is(err, ErrEmailTaken):
		page.Errors = validate.Errors{"email": "Email sudah terdaftar. Silakan masuk."}
	case err != nil:
		h.view.ServerError(w, r, err)
		return
	}
	if page.Errors != nil {
		h.view.Render(w, r, http.StatusUnprocessableEntity, "auth/register", page)
		return
	}

	sess, err := h.svc.StartSession(r.Context(), u.ID)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.setSessionCookie(w, sess)
	h.view.SetFlash(w, "success", "Selamat datang di Ashanum! Lengkapi profil Anda agar belanja lebih mudah.")
	if page.Next == "/" {
		page.Next = "/account"
	}
	http.Redirect(w, r, page.Next, http.StatusSeeOther)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(h.cookie.Name); err == nil {
		if err := h.svc.Logout(r.Context(), c.Value); err != nil {
			h.view.ServerError(w, r, err)
			return
		}
	}
	h.clearSessionCookie(w)
	h.view.SetFlash(w, "info", "Anda telah keluar.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, s Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookie.Name,
		Value:    s.Token,
		Path:     "/",
		Expires:  s.ExpiresAt,
		MaxAge:   int(time.Until(s.ExpiresAt).Seconds()),
		HttpOnly: true,
		Secure:   h.cookie.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookie.Name,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookie.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// safeNext only allows local paths, preventing open redirects such as
// //evil.example or /\evil.example.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/\\") || strings.ContainsAny(next, "\r\n\t") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/"
	}
	return next
}
