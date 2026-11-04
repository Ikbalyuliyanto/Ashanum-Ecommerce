package returns

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

type formPage struct {
	Draft       Draft
	Selected    map[int64]int // order item → chosen quantity
	Type        Type
	Reason      Reason
	Description string
	Evidence    string // one URL per line
	Key         string
	Errors      validate.Errors
	WindowDays  int
}

func (formPage) Reasons() []Reason { return Reasons }
func (formPage) Types() []Type     { return []Type{TypeRefund, TypeExchange} }

func (p formPage) Chosen(id int64) bool { return p.Selected[id] > 0 }

// Quantity is the picker value of an item (1 when not chosen yet).
func (p formPage) Quantity(id int64) int { return max(p.Selected[id], 1) }

// Estimate is the refund for the current choice, recomputed on the server
// when the request is submitted.
func (p formPage) Estimate() int64 {
	if p.Type == TypeExchange {
		return 0
	}
	var sum int64
	for _, it := range p.Draft.Items {
		if q := p.Selected[it.OrderItemID]; q > 0 {
			sum += it.UnitPrice * int64(min(q, it.Remaining()))
		}
	}
	return sum
}

// ChosenUnits is the number of units chosen.
func (p formPage) ChosenUnits() int {
	n := 0
	for _, it := range p.Draft.Items {
		n += min(p.Selected[it.OrderItemID], it.Remaining())
	}
	return n
}

// parseForm reads the chosen items (item=ID, qty_ID=N) and the rest of the
// request from query or form values.
func parseForm(v url.Values) formPage {
	p := formPage{Selected: map[int64]int{}, Type: Type(v.Get("type")), Reason: Reason(v.Get("reason")),
		Description: v.Get("description"), Evidence: v.Get("evidence"), Key: v.Get("key")}
	for _, raw := range v["item"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		q, _ := strconv.Atoi(v.Get("qty_" + raw))
		p.Selected[id] = max(q, 1)
	}
	if !p.Type.Valid() {
		p.Type = TypeRefund
	}
	return p
}

func (p formPage) request(orderNumber string) Request {
	req := Request{OrderNumber: orderNumber, Type: p.Type, Reason: p.Reason, Description: p.Description,
		Evidence: strings.Split(p.Evidence, "\n"), IdempotencyKey: p.Key}
	for id, q := range p.Selected {
		req.Items = append(req.Items, ItemRequest{OrderItemID: id, Quantity: q})
	}
	return req
}

// NewForm renders /account/returns/new?order=NUMBER. HTMX requests for
// #return-summary get the recalculated summary only.
func (h *Handler) NewForm(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	number := q.Get("order")
	draft, err := h.svc.Draft(r.Context(), p.UserID, number)
	if err != nil {
		h.fail(w, r, number, err)
		return
	}
	page := parseForm(q)
	page.Draft, page.WindowDays = draft, h.svc.WindowDays()
	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "return-summary" {
		h.view.Fragment(w, r, http.StatusOK, "returns/new", "return-summary", page)
		return
	}
	if len(q["item"]) == 0 && len(draft.Items) == 1 {
		page.Selected[draft.Items[0].OrderItemID] = 1
	}
	page.Key = newKey()
	h.view.Render(w, r, http.StatusOK, "returns/new", page)
}

// Create handles POST /account/returns.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	number := r.PostForm.Get("order")
	page := parseForm(r.PostForm)
	ret, err := h.svc.Create(r.Context(), p.UserID, page.request(number))
	if err == nil {
		h.view.SetFlash(w, "success", "Pengajuan pengembalian terkirim. Kami akan meninjaunya.")
		http.Redirect(w, r, "/account/returns/"+url.PathEscape(ret), http.StatusSeeOther)
		return
	}
	var verr validate.Errors
	if !errors.As(err, &verr) {
		h.fail(w, r, number, err)
		return
	}
	draft, derr := h.svc.Draft(r.Context(), p.UserID, number)
	if derr != nil {
		h.fail(w, r, number, derr)
		return
	}
	page.Draft, page.WindowDays, page.Errors = draft, h.svc.WindowDays(), verr
	if page.Key == "" {
		page.Key = newKey()
	}
	h.view.Render(w, r, http.StatusUnprocessableEntity, "returns/new", page)
}

type historyPage struct{ Result Page }

func (p historyPage) NextURL() string {
	return "/account/returns?page=" + strconv.Itoa(p.Result.Page+1)
}

// History renders /account/returns?page=N.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	res, err := h.svc.List(r.Context(), p.UserID, n)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	data := historyPage{Result: res}
	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "returns-more" {
		h.view.Fragment(w, r, http.StatusOK, "returns/history", "return-cards", data)
		return
	}
	h.view.Render(w, r, http.StatusOK, "returns/history", data)
}

// Detail renders /account/returns/{number}; other users' returns are 404.
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ret, err := h.svc.Get(r.Context(), p.UserID, chi.URLParam(r, "number"))
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "returns/detail", ret)
}

// Cancel withdraws the user's request (POST /account/returns/{number}/cancel).
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	number := chi.URLParam(r, "number")
	err := h.svc.CancelByUser(r.Context(), p.UserID, number)
	switch {
	case errors.Is(err, ErrNotFound):
		h.view.Error(w, r, http.StatusNotFound)
		return
	case err == nil:
		h.view.SetFlash(w, "success", "Pengajuan pengembalian dibatalkan.")
	case Message(err) != "":
		h.view.SetFlash(w, "error", Message(err))
	default:
		h.view.ServerError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/returns/"+url.PathEscape(number), http.StatusSeeOther)
}

// fail sends eligibility problems back to the order with a message.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, orderNumber string, err error) {
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if msg := Message(err); msg != "" {
		h.view.SetFlash(w, "error", msg)
		http.Redirect(w, r, "/account/orders/"+url.PathEscape(orderNumber), http.StatusSeeOther)
		return
	}
	h.view.ServerError(w, r, err)
}

func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
