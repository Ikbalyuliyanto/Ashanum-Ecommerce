package address

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/auth"
	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

type Handler struct {
	svc  *Service
	view *view.Renderer
}

func NewHandler(svc *Service, v *view.Renderer) *Handler { return &Handler{svc: svc, view: v} }

type listPage struct {
	Addresses []Address
	Max       int
	Notice    string // shown inside the list after an HTMX action
}

type formPage struct {
	Next      string // safe local path to return to after saving (e.g. checkout)
	ID        int64  // 0 for a new address
	Form      Input
	IsPrimary bool
	Errors    validate.Errors
	Error     string
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	h.renderList(w, r, p.UserID, "")
}

func (h *Handler) NewForm(w http.ResponseWriter, r *http.Request) {
	h.view.Render(w, r, http.StatusOK, "user/address-form", formPage{
		Form: Input{Label: "Rumah"},
		Next: view.SafePath(r.URL.Query().Get("next"), ""),
	})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	in, ok := h.parseInput(w, r)
	if !ok {
		return
	}
	_, err := h.svc.Create(r.Context(), p.UserID, in)
	h.afterSave(w, r, formPage{Form: in, Next: view.SafePath(r.PostForm.Get("next"), "")}, err)
}

func (h *Handler) EditForm(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id, ok := pathID(r)
	if !ok {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	a, err := h.svc.Get(r.Context(), p.UserID, id)
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	form := InputFrom(a)
	form.Phone = view.LocalPhone(form.Phone)
	h.view.Render(w, r, http.StatusOK, "user/address-form", formPage{ID: a.ID, Form: form, IsPrimary: a.IsPrimary})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id, ok := pathID(r)
	if !ok {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	in, ok := h.parseInput(w, r)
	if !ok {
		return
	}
	_, err := h.svc.Update(r.Context(), p.UserID, id, in)
	page := formPage{ID: id, Form: in}
	if err != nil {
		// Re-rendering after a failed save: show the stored primary state.
		if cur, getErr := h.svc.Get(r.Context(), p.UserID, id); getErr == nil {
			page.IsPrimary = cur.IsPrimary
		}
	}
	h.afterSave(w, r, page, err)
}

func (h *Handler) SetPrimary(w http.ResponseWriter, r *http.Request) {
	h.action(w, r, h.svc.SetPrimary, "Alamat utama berhasil diubah.")
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	h.action(w, r, h.svc.Delete, "Alamat berhasil dihapus.")
}

// action runs a list action and answers HTMX with the refreshed list, or a
// plain form post with a redirect back to the list.
func (h *Handler) action(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, userID, id int64) error, done string) {
	p, _ := auth.FromContext(r.Context())
	id, ok := pathID(r)
	if !ok {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	err := do(r.Context(), p.UserID, id)
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	if view.IsHTMX(r) {
		h.renderList(w, r, p.UserID, done)
		return
	}
	h.view.SetFlash(w, "success", done)
	http.Redirect(w, r, "/account/addresses", http.StatusSeeOther)
}

func (h *Handler) afterSave(w http.ResponseWriter, r *http.Request, page formPage, err error) {
	switch {
	case err == nil:
		h.view.SetFlash(w, "success", "Alamat berhasil disimpan.")
		http.Redirect(w, r, view.SafePath(page.Next, "/account/addresses"), http.StatusSeeOther)
	case errors.Is(err, ErrNotFound):
		h.view.Error(w, r, http.StatusNotFound)
	case errors.Is(err, ErrLimitReached):
		page.Error = "Anda sudah memiliki " + strconv.Itoa(MaxPerUser) + " alamat. Hapus salah satu sebelum menambah alamat baru."
		h.view.Render(w, r, http.StatusUnprocessableEntity, "user/address-form", page)
	case errors.As(err, &page.Errors):
		h.view.Render(w, r, http.StatusUnprocessableEntity, "user/address-form", page)
	default:
		h.view.ServerError(w, r, err)
	}
}

func (h *Handler) renderList(w http.ResponseWriter, r *http.Request, userID int64, notice string) {
	list, err := h.svc.List(r.Context(), userID)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	page := listPage{Addresses: list, Max: MaxPerUser, Notice: notice}
	if view.IsHTMX(r) {
		h.view.Fragment(w, r, http.StatusOK, "user/addresses", "address-list", page)
		return
	}
	h.view.Render(w, r, http.StatusOK, "user/addresses", page)
}

func (h *Handler) parseInput(w http.ResponseWriter, r *http.Request) (Input, bool) {
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return Input{}, false
	}
	f := r.PostForm
	return Input{
		Label:         f.Get("label"),
		RecipientName: f.Get("recipient_name"),
		Phone:         f.Get("phone"),
		Province:      f.Get("province"),
		City:          f.Get("city"),
		District:      f.Get("district"),
		Village:       f.Get("village"),
		PostalCode:    f.Get("postal_code"),
		StreetAddress: f.Get("street_address"),
		Latitude:      f.Get("latitude"),
		Longitude:     f.Get("longitude"),
		MapsURL:       f.Get("maps_url"),
		MakePrimary:   f.Get("make_primary") == "1",
	}, true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}
