package cart

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/auth"
	"ecommerce/internal/view"
)

type Handler struct {
	svc  *Service
	view *view.Renderer
}

func NewHandler(svc *Service, v *view.Renderer) *Handler { return &Handler{svc: svc, view: v} }

// page is the cart page. Selection lives in the request (checkboxes named
// "item", plus sel=1 once the customer has changed it); it is never stored.
type page struct {
	Cart     Cart
	Selected map[int64]bool
	Summary  Summary
	Error    string
	Notice   string
	Partial  bool // HTMX response: also update the header badge out of band
}

func (p page) IsSelected(id int64) bool { return p.Selected[id] }

// CartQuantity lets the header badge reuse the loaded cart (no extra query).
func (p page) CartQuantity() int { return p.Cart.Quantity() }

// Selectable counts the checkout-ready lines.
func (p page) Selectable() int {
	n := 0
	for _, it := range p.Cart.Items {
		if it.Checkoutable() {
			n++
		}
	}
	return n
}

func (p page) AllSelected() bool { return p.Selectable() > 0 && p.Summary.Lines == p.Selectable() }

// ToggleAllURL selects everything, or nothing when everything is selected.
func (p page) ToggleAllURL() string {
	if p.AllSelected() {
		return "/cart?sel=1"
	}
	return "/cart"
}

func newPage(c Cart, form url.Values) page {
	p := page{Cart: c, Selected: make(map[int64]bool)}
	explicit := form.Get("sel") == "1"
	chosen := make(map[int64]bool)
	for _, v := range form["item"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			chosen[id] = true
		}
	}
	for _, it := range c.Items {
		if it.Checkoutable() && (!explicit || chosen[it.ID]) {
			p.Selected[it.ID] = true
		}
	}
	p.Summary = c.Summarize(p.IsSelected)
	return p
}

// selectionQuery keeps the customer's selection across a redirect.
func selectionQuery(form url.Values) string {
	if form.Get("sel") != "1" {
		return "/cart"
	}
	v := url.Values{"sel": {"1"}}
	for _, id := range form["item"] {
		if _, err := strconv.ParseInt(id, 10, 64); err == nil {
			v.Add("item", id)
		}
	}
	return "/cart?" + v.Encode()
}

// Show renders /cart; with HTMX (target "cart") only the cart body.
func (h *Handler) Show(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	c, err := h.svc.Get(r.Context(), p.UserID)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, newPage(c, r.URL.Query()))
}

type feedback struct {
	Kind         string // "success" | "error"
	Message      string
	CartQuantity int
}

// Add handles the product page form. HTMX gets a small status message and
// an out-of-band badge update; a plain form post is redirected back with a
// flash message.
func (h *Handler) Add(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	variantID, _ := strconv.ParseInt(r.PostForm.Get("variant_id"), 10, 64)
	quantity := 1
	if q := r.PostForm.Get("quantity"); q != "" {
		quantity, _ = strconv.Atoi(strings.TrimSpace(q)) // invalid → 0 → refused
	}
	back := view.SafePath(r.PostForm.Get("return_to"), "/cart")

	res, err := h.svc.AddItem(r.Context(), p.UserID, variantID, quantity)
	msg := Message(err)
	if err != nil && msg == "" {
		h.view.ServerError(w, r, err)
		return
	}

	if view.IsHTMX(r) {
		fb, status := feedback{Kind: "success", Message: "Berhasil ditambahkan ke keranjang.", CartQuantity: res.CartQuantity}, http.StatusOK
		if err != nil {
			fb, status = feedback{Kind: "error", Message: msg}, http.StatusUnprocessableEntity
		}
		h.view.Fragment(w, r, status, "cart/index", "cart-feedback", fb)
		return
	}
	if err != nil {
		h.view.SetFlash(w, "error", msg)
	} else {
		h.view.SetFlash(w, "success", "Berhasil ditambahkan ke keranjang.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(userID, id int64) error {
		q, err := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("quantity")))
		if err != nil {
			q = 0
		}
		return h.svc.UpdateQuantity(r.Context(), userID, id, q)
	}, "")
}

func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(userID, id int64) error {
		return h.svc.RemoveItem(r.Context(), userID, id)
	}, "Produk dihapus dari keranjang.")
}

func (h *Handler) Clear(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	if err := h.svc.Clear(r.Context(), p.UserID); err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.after(w, r, p.UserID, nil, "Keranjang dikosongkan.")
}

// mutate runs a change on one owned line ({id}) and answers like Show.
func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, do func(userID, id int64) error, done string) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	err = do(p.UserID, id)
	if errors.Is(err, ErrItemNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil && Message(err) == "" {
		h.view.ServerError(w, r, err)
		return
	}
	h.after(w, r, p.UserID, err, done)
}

// after answers a change: HTMX gets the refreshed cart body (422 with the
// reason when refused), a plain post a redirect keeping the selection.
func (h *Handler) after(w http.ResponseWriter, r *http.Request, userID int64, err error, done string) {
	if !view.IsHTMX(r) {
		if err != nil {
			h.view.SetFlash(w, "error", Message(err))
		} else if done != "" {
			h.view.SetFlash(w, "success", done)
		}
		http.Redirect(w, r, selectionQuery(r.PostForm), http.StatusSeeOther)
		return
	}
	c, getErr := h.svc.Get(r.Context(), userID)
	if getErr != nil {
		h.view.ServerError(w, r, getErr)
		return
	}
	p := newPage(c, r.PostForm)
	status := http.StatusOK
	if err != nil {
		p.Error, status = Message(err), http.StatusUnprocessableEntity
	} else {
		p.Notice = done
	}
	h.render(w, r, status, p)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, p page) {
	if view.IsHTMX(r) {
		p.Partial = true
		h.view.Fragment(w, r, status, "cart/index", "cart-body", p)
		return
	}
	h.view.Render(w, r, status, "cart/index", p)
}
