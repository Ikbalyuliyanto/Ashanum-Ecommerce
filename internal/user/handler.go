package user

import (
	"errors"
	"net/http"

	"ecommerce/internal/auth"
	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

type Handler struct {
	svc  *Service
	view *view.Renderer
}

func NewHandler(svc *Service, v *view.Renderer) *Handler { return &Handler{svc: svc, view: v} }

type profilePage struct {
	Email  string
	Form   ProfileInput
	Errors validate.Errors
	Saved  bool
}

// Account renders the "Akun Saya" hub.
func (h *Handler) Account(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	acc, err := h.svc.GetAccount(r.Context(), p.UserID)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "user/account", acc)
}

func (h *Handler) ProfileForm(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	acc, err := h.svc.GetAccount(r.Context(), p.UserID)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "user/profile", profilePage{
		Email: acc.Email,
		Form: ProfileInput{
			FirstName: acc.Profile.FirstName,
			LastName:  acc.Profile.LastName,
			Phone:     view.LocalPhone(acc.Profile.Phone),
			Gender:    string(acc.Profile.Gender),
		},
	})
}

// UpdateProfile answers HTMX with the form fragment (errors or a success
// note) and plain form posts with a full page or a redirect.
func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	page := profilePage{
		Email: p.Email,
		Form: ProfileInput{
			FirstName: r.PostForm.Get("first_name"),
			LastName:  r.PostForm.Get("last_name"),
			Phone:     r.PostForm.Get("phone"),
			Gender:    r.PostForm.Get("gender"),
		},
	}

	saved, err := h.svc.UpdateProfile(r.Context(), p.UserID, page.Form)
	if err != nil && !errors.As(err, &page.Errors) {
		h.view.ServerError(w, r, err)
		return
	}

	status := http.StatusOK
	if page.Errors != nil {
		status = http.StatusUnprocessableEntity
	} else {
		page.Saved = true
		page.Form = ProfileInput{
			FirstName: saved.FirstName,
			LastName:  saved.LastName,
			Phone:     view.LocalPhone(saved.Phone),
			Gender:    string(saved.Gender),
		}
	}

	switch {
	case view.IsHTMX(r):
		h.view.Fragment(w, r, status, "user/profile", "profile-form", page)
	case page.Errors != nil:
		h.view.Render(w, r, status, "user/profile", page)
	default:
		h.view.SetFlash(w, "success", "Profil berhasil disimpan.")
		http.Redirect(w, r, "/account/profile", http.StatusSeeOther)
	}
}
