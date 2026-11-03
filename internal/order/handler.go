package order

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/auth"
	"ecommerce/internal/view"
)

type Handler struct {
	svc  *Service
	view *view.Renderer
}

func NewHandler(svc *Service, v *view.Renderer) *Handler { return &Handler{svc: svc, view: v} }

type historyPage struct {
	Result Page
	Tabs   []Tab
}

// TabURL links to a history tab.
func (p historyPage) TabURL(key string) string {
	if key == "" {
		return "/account/orders"
	}
	return "/account/orders?" + url.Values{"status": {key}}.Encode()
}

// NextURL links to the next history page (also the HTMX load-more link).
func (p historyPage) NextURL() string {
	v := url.Values{"page": {strconv.Itoa(p.Result.Page + 1)}}
	if p.Result.Tab.Key != "" {
		v.Set("status", p.Result.Tab.Key)
	}
	return "/account/orders?" + v.Encode()
}

// History renders /account/orders?status=…&page=….
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	res, err := h.svc.List(r.Context(), p.UserID, r.URL.Query().Get("status"), n)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	page := historyPage{Result: res, Tabs: Tabs}
	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "orders-more" {
		h.view.Fragment(w, r, http.StatusOK, "order/history", "order-cards", page)
		return
	}
	h.view.Render(w, r, http.StatusOK, "order/history", page)
}

// Detail renders /account/orders/{number}; other users' orders are 404.
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	o, err := h.svc.Get(r.Context(), p.UserID, chi.URLParam(r, "number"))
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "order/detail", o)
}

// PaymentFinish is where the payment provider sends the buyer back. Its
// query parameters (status etc.) are ignored: the order is refreshed from
// the provider and the page shows what the database now says.
func (h *Handler) PaymentFinish(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	o, err := h.svc.Sync(r.Context(), p.UserID, chi.URLParam(r, "number"))
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	switch o.Status {
	case StatusWaitingPayment, StatusWaitingConfirmation, StatusCancelled:
		h.view.Render(w, r, http.StatusOK, "order/payment", o)
	default:
		http.Redirect(w, r, "/account/orders/"+url.PathEscape(o.Number), http.StatusSeeOther)
	}
}
