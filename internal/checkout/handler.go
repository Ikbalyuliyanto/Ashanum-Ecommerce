package checkout

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/auth"
	"ecommerce/internal/cart"
	"ecommerce/internal/order"
	"ecommerce/internal/view"
)

type Handler struct {
	svc    *Service
	carts  *cart.Service
	orders *order.Service
	view   *view.Renderer
}

func NewHandler(svc *Service, carts *cart.Service, orders *order.Service, v *view.Renderer) *Handler {
	return &Handler{svc: svc, carts: carts, orders: orders, view: v}
}

type page struct {
	Draft   Draft
	Key     string // idempotency key for this rendering of the page
	Warning string
}

// query encodes the checkout source (and address) as URL parameters.
func (p page) query(withAddress bool) url.Values {
	v := url.Values{}
	if s := p.Draft.Source; s.BuyNow() {
		v.Set("variant_id", strconv.FormatInt(s.VariantID, 10))
		v.Set("quantity", strconv.Itoa(s.Quantity))
	} else {
		for _, id := range s.CartItemIDs {
			v.Add("item", strconv.FormatInt(id, 10))
		}
	}
	if withAddress && p.Draft.Address != nil {
		v.Set("address", strconv.FormatInt(p.Draft.Address.ID, 10))
	}
	return v
}

// CurrentURL is this checkout (return target after adding an address).
func (p page) CurrentURL() string { return "/checkout?" + p.query(true).Encode() }

// AddAddressURL opens the address form and comes back here.
func (p page) AddAddressURL() string {
	return "/account/addresses/new?" + url.Values{"next": {p.CurrentURL()}}.Encode()
}

// BackURL returns to the product (Buy Now) or the cart with this selection.
func (p page) BackURL() string {
	if p.Draft.Source.BuyNow() {
		if len(p.Draft.Lines) > 0 && p.Draft.Lines[0].ProductSlug != "" {
			return "/products/" + p.Draft.Lines[0].ProductSlug
		}
		return "/"
	}
	v := p.query(false)
	v.Set("sel", "1")
	return "/cart?" + v.Encode()
}

func sourceFrom(v url.Values) Source {
	if id, err := strconv.ParseInt(v.Get("variant_id"), 10, 64); err == nil && id > 0 {
		q, err := strconv.Atoi(v.Get("quantity"))
		if err != nil {
			q = 1
		}
		return Source{VariantID: id, Quantity: q}
	}
	var src Source
	for _, s := range v["item"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 && len(src.CartItemIDs) < 100 {
			src.CartItemIDs = append(src.CartItemIDs, id)
		}
	}
	return src
}

// Show renders GET /checkout?item=… (selected cart lines) or
// /checkout?variant_id=…&quantity=… (Buy Now; the cart is not touched).
func (h *Handler) Show(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	src := sourceFrom(q)

	// Buy Now goes through the same purchase rules as add-to-cart.
	if src.BuyNow() {
		if _, err := h.carts.Quote(r.Context(), src.VariantID, src.Quantity); err != nil {
			msg := cart.Message(err)
			if msg == "" {
				h.view.ServerError(w, r, err)
				return
			}
			h.view.SetFlash(w, "error", msg)
			http.Redirect(w, r, view.SafePath(q.Get("return_to"), "/"), http.StatusSeeOther)
			return
		}
	}

	addressID, _ := strconv.ParseInt(q.Get("address"), 10, 64)
	d, err := h.svc.Prepare(r.Context(), p.UserID, src, addressID)
	if errors.Is(err, ErrNothingToCheckout) {
		h.view.SetFlash(w, "error", Message(err))
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, page{Draft: d, Key: newKey()})
}

// Place handles POST /checkout/order. Success redirects to the success
// page; a stock, price or availability change re-renders the checkout with
// fresh data and the reason.
func (h *Handler) Place(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	f := r.PostForm
	src := sourceFrom(f)
	addressID, _ := strconv.ParseInt(f.Get("address_id"), 10, 64)
	expected, _ := strconv.ParseInt(f.Get("expected_total"), 10, 64)

	placed, err := h.svc.PlaceOrder(r.Context(), p.UserID, PlaceRequest{
		Source:         src,
		AddressID:      addressID,
		ExpectedTotal:  expected,
		IdempotencyKey: f.Get("key"),
	})
	if err == nil {
		http.Redirect(w, r, "/checkout/success/"+url.PathEscape(placed.OrderNumber), http.StatusSeeOther)
		return
	}
	msg := Message(err)
	if msg == "" {
		h.view.ServerError(w, r, err)
		return
	}
	d, prepErr := h.svc.Prepare(r.Context(), p.UserID, src, addressID)
	if prepErr != nil {
		h.view.SetFlash(w, "error", msg)
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusUnprocessableEntity, page{Draft: d, Key: newKey(), Warning: msg})
}

// Success renders GET /checkout/success/{number} for the owner only.
func (h *Handler) Success(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	o, err := h.orders.Get(r.Context(), p.UserID, chi.URLParam(r, "number"))
	if errors.Is(err, order.ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "checkout/success", o)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, p page) {
	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "checkout" {
		h.view.Fragment(w, r, status, "checkout/index", "checkout-body", p)
		return
	}
	h.view.Render(w, r, status, "checkout/index", p)
}

func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
