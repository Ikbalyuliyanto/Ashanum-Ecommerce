package address

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/auth"
	"ecommerce/internal/shipping"
	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

// AreaSearch is the delivery area search (shipping.Service).
type AreaSearch interface {
	Enabled() bool
	SearchAreas(ctx context.Context, query string) ([]shipping.Area, error)
	Area(ctx context.Context, id string) (shipping.Area, error)
}

type Handler struct {
	svc   *Service
	areas AreaSearch
	view  *view.Renderer
}

func NewHandler(svc *Service, areas AreaSearch, v *view.Renderer) *Handler {
	return &Handler{svc: svc, areas: areas, view: v}
}

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

	// Delivery area picker.
	ShippingEnabled bool
	AreaLabel       string // the chosen area
	AreaQuery       string
	AreaResults     []shipping.Area
	AreaError       string
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	h.renderList(w, r, p.UserID, "")
}

func (h *Handler) NewForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, formPage{
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
	page := formPage{Form: in, Next: view.SafePath(r.PostForm.Get("next"), "")}
	if h.searching(w, r, page) {
		return
	}
	_, err := h.svc.Create(r.Context(), p.UserID, in)
	h.afterSave(w, r, page, err)
}

// searching handles the "Cari" button of the area picker without
// JavaScript: the form is shown again with the results, nothing is saved.
func (h *Handler) searching(w http.ResponseWriter, r *http.Request, page formPage) bool {
	if r.PostForm.Get("intent") != "search_area" {
		return false
	}
	page.AreaQuery = r.PostForm.Get("area_q")
	page.AreaResults, page.AreaError = h.search(r.Context(), page.AreaQuery)
	h.render(w, r, http.StatusOK, page)
	return true
}

func (h *Handler) search(ctx context.Context, q string) ([]shipping.Area, string) {
	if h.areas == nil || !h.areas.Enabled() {
		return nil, shipping.Message(shipping.ErrNotConfigured)
	}
	areas, err := h.areas.SearchAreas(ctx, q)
	if msg := shipping.Message(err); err != nil && msg != "" {
		if errors.Is(err, shipping.ErrUnavailable) || errors.Is(err, shipping.ErrRejected) {
			msg = "Pencarian lokasi sedang tidak tersedia. Silakan coba lagi."
		}
		return nil, msg
	} else if err != nil {
		return nil, "Pencarian lokasi sedang tidak tersedia. Silakan coba lagi."
	}
	if len(areas) == 0 {
		return nil, "Lokasi tidak ditemukan. Coba nama kecamatan lain atau kode pos."
	}
	return areas, ""
}

// Areas answers the HTMX area search: GET /account/addresses/areas?area_q=
// returns the result list (a fragment of the address form).
func (h *Handler) Areas(w http.ResponseWriter, r *http.Request) {
	page := formPage{ShippingEnabled: h.areas != nil && h.areas.Enabled(), AreaQuery: strings.TrimSpace(r.URL.Query().Get("area_q"))}
	if len([]rune(page.AreaQuery)) >= 3 {
		page.AreaResults, page.AreaError = h.search(r.Context(), page.AreaQuery)
	}
	h.view.Fragment(w, r, http.StatusOK, "user/address-form", "area-results", page)
}

// render shows the address form with the chosen area's label.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, page formPage) {
	page.ShippingEnabled = h.areas != nil && h.areas.Enabled()
	if id := page.Form.ShippingAreaID; id != "" && page.AreaLabel == "" && page.ShippingEnabled {
		if a, err := h.areas.Area(r.Context(), id); err == nil {
			page.AreaLabel = a.Label
		} else {
			page.Form.ShippingAreaID = "" // unknown id: not shown as chosen
		}
	}
	h.view.Render(w, r, status, "user/address-form", page)
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
	h.render(w, r, http.StatusOK, formPage{ID: a.ID, Form: form, IsPrimary: a.IsPrimary, AreaLabel: a.ShippingAreaLabel,
		Next: view.SafePath(r.URL.Query().Get("next"), "")})
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
	page := formPage{ID: id, Form: in, Next: view.SafePath(r.PostForm.Get("next"), "")}
	var err error
	if r.PostForm.Get("intent") != "search_area" {
		_, err = h.svc.Update(r.Context(), p.UserID, id, in)
		if err == nil {
			h.afterSave(w, r, page, nil)
			return
		}
	}
	// Re-rendering (area search or failed save): show the stored primary state.
	cur, getErr := h.svc.Get(r.Context(), p.UserID, id)
	if errors.Is(getErr, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	page.IsPrimary = cur.IsPrimary
	if h.searching(w, r, page) {
		return
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
		h.render(w, r, http.StatusUnprocessableEntity, page)
	case errors.As(err, &page.Errors):
		h.render(w, r, http.StatusUnprocessableEntity, page)
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

		ShippingAreaID: strings.TrimSpace(f.Get("shipping_area_id")),
	}, true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}
