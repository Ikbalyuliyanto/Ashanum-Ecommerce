// Package admin is the shop's back office under /admin: server-rendered
// pages (HTMX only as an enhancement) over the domain services. It holds no
// business rules: every status change, stock change and refund step is a
// service call that checks the state machine, locks rows and writes the
// audit log itself.
//
// Access: the router mounts these routes behind auth.RequireAdmin (signed
// in + ADMIN role, read from the database on every request); mutations are
// POST only and pass the global same-origin (CSRF) check. Handlers read an
// explicit whitelist of form fields — never user IDs, payment status,
// totals, refund amounts or roles.
package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/auth"
	"ecommerce/internal/category"
	"ecommerce/internal/fulfillment"
	"ecommerce/internal/inventory"
	"ecommerce/internal/product"
	"ecommerce/internal/report"
	"ecommerce/internal/returns"
	"ecommerce/internal/review"
	"ecommerce/internal/shipping"
	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

type Deps struct {
	View       *view.Renderer
	Log        *slog.Logger
	Orders     *fulfillment.Service
	Products   *product.Service
	Categories *category.Service
	Inventory  *inventory.Service
	Returns    *returns.Service
	Reviews    *review.Service
	Reports    *report.Service
	Audit      *audit.Service
	// Shipping books and tracks provider shipments (nil: not configured).
	Shipping *shipping.Service
}

type Handler struct{ Deps }

func NewHandler(d Deps) *Handler { return &Handler{Deps: d} }

// Routes registers the admin pages. The caller mounts them under /admin
// behind auth.RequireAdmin.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/", h.dashboard)

	r.Get("/orders", h.orders)
	r.Get("/orders/{number}", h.order)
	r.Post("/orders/{number}/confirm", h.orderConfirm)
	r.Post("/orders/{number}/ship", h.orderShip)
	r.Post("/orders/{number}/shipment/book", h.orderBook)
	r.Post("/orders/{number}/shipment/sync", h.orderShipmentSync)
	r.Post("/orders/{number}/shipment/cancel", h.orderShipmentCancel)
	r.Post("/orders/{number}/complete", h.orderComplete)
	r.Post("/orders/{number}/cancel", h.orderCancel)

	r.Get("/products", h.products)
	r.Get("/products/new", h.productNew)
	r.Post("/products", h.productCreate)
	r.Get("/products/{id}/edit", h.productEdit)
	r.Post("/products/{id}", h.productUpdate)
	r.Post("/products/{id}/activate", h.productActivate)
	r.Post("/products/{id}/deactivate", h.productDeactivate)
	r.Post("/products/{id}/variants", h.variantCreate)
	r.Post("/products/{id}/variants/{variant}", h.variantUpdate)
	r.Post("/products/{id}/attributes", h.attributeCreate)

	r.Get("/stock", h.stock)
	r.Get("/stock/{variant}", h.stockForm)
	r.Post("/stock/{variant}", h.stockAdjust)

	r.Get("/categories", h.categories)
	r.Post("/categories", h.categoryCreate)
	r.Get("/categories/{id}/edit", h.categoryEdit)
	r.Post("/categories/{id}", h.categoryUpdate)

	r.Get("/returns", h.returnList)
	r.Get("/returns/{number}", h.returnDetail)
	r.Get("/returns/{number}/estimate", h.returnEstimate)
	r.Post("/returns/{number}/approve", h.returnApprove)
	r.Post("/returns/{number}/reject", h.returnReject)
	r.Post("/returns/{number}/waiting-return", h.returnWaiting)
	r.Post("/returns/{number}/received", h.returnReceived)
	r.Post("/returns/{number}/restock", h.returnRestock)
	r.Post("/returns/{number}/prepare-refund", h.returnPrepareRefund)
	r.Post("/returns/{number}/complete-refund", h.returnCompleteRefund)
	r.Post("/returns/{number}/complete-exchange", h.returnCompleteExchange)
	r.Post("/returns/{number}/complete", h.returnComplete)
	r.Post("/returns/{number}/cancel", h.returnCancel)

	r.Get("/refunds", h.refunds)
	r.Get("/refunds/{id}", h.refund)
	r.Post("/refunds/{id}/processing", h.refundProcessing)
	r.Post("/refunds/{id}/complete", h.refundComplete)
	r.Post("/refunds/{id}/fail", h.refundFail)

	r.Get("/reviews", h.reviews)
	r.Post("/reviews/{id}/hide", h.reviewHide)
	r.Post("/reviews/{id}/unhide", h.reviewUnhide)

	r.Get("/reports", h.reports)
	r.Get("/audit", h.auditLog)
}

// Nav keys of the sidebar.
const (
	navDashboard  = "dashboard"
	navOrders     = "orders"
	navProducts   = "products"
	navCategories = "categories"
	navStock      = "stock"
	navReturns    = "returns"
	navReviews    = "reviews"
	navReports    = "reports"
)

// shell is embedded in every admin page's data: the active menu item.
type shell struct {
	Nav string
}

// adminID is the signed-in admin (RequireAdmin guarantees one).
func adminID(r *http.Request) int64 {
	p, _ := auth.FromContext(r.Context())
	return p.UserID
}

// pager builds previous/next links that keep the current filters.
type pager struct {
	Path    string
	Query   url.Values
	Page    int
	HasNext bool
}

func newPager(r *http.Request, page int, hasNext bool) pager {
	q := url.Values{}
	for k, v := range r.URL.Query() {
		if k != "page" && len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	return pager{Path: r.URL.Path, Query: q, Page: page, HasNext: hasNext}
}

func (p pager) url(page int) string {
	q := url.Values{}
	for k, v := range p.Query {
		q[k] = v
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return p.Path
	}
	return p.Path + "?" + q.Encode()
}

func (p pager) HasPrev() bool   { return p.Page > 1 }
func (p pager) PrevURL() string { return p.url(p.Page - 1) }
func (p pager) NextURL() string { return p.url(p.Page + 1) }

// With returns the list URL with key set to value (filter tabs), page 1.
func (p pager) With(key, value string) string {
	q := url.Values{}
	for k, v := range p.Query {
		q[k] = v
	}
	if value == "" {
		q.Del(key)
	} else {
		q.Set(key, value)
	}
	if len(q) == 0 {
		return p.Path
	}
	return p.Path + "?" + q.Encode()
}

// Current is the list URL itself (forms return to it).
func (p pager) Current() string { return p.url(p.Page) }

func queryInt(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return n
}

func pathID(r *http.Request, key string) int64 {
	n, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// perPage reads ?per= (default and maximum are enforced by the services).
func perPage(r *http.Request) int { return queryInt(r, "per") }

// parseRupiah reads an amount typed as "125000", "125.000" or "Rp 125.000"
// (integer rupiah; no fractions). Empty is 0; garbage is -1 (invalid).
func parseRupiah(s string) int64 {
	s = strings.NewReplacer("Rp", "", "rp", "", ".", "", ",", "", " ", "").Replace(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// optionalInt is parseInt where an empty field means 0.
func optionalInt(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return parseInt(s)
}

func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	return n
}

// done redirects after a successful POST with a flash message.
func (h *Handler) done(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		h.View.SetFlash(w, "success", msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// refuse redirects back with an error flash for a known domain error, or
// renders the 500 page for anything else.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, to string, err error, known ...func(error) string) {
	var verr validate.Errors
	if errors.As(err, &verr) {
		h.View.SetFlash(w, "error", firstError(verr))
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	for _, k := range known {
		if msg := k(err); msg != "" {
			h.View.SetFlash(w, "error", msg)
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
	}
	h.View.ServerError(w, r, err)
}

func firstError(e validate.Errors) string {
	for _, k := range []string{"note", "reason", "items", "quantity", "reference"} {
		if m, ok := e[k]; ok {
			return m
		}
	}
	for _, m := range e {
		return m
	}
	return "Data tidak valid."
}

// returnTo is a same-site path from the form (e.g. the filtered list the
// action came from), or fallback.
func returnTo(r *http.Request, fallback string) string {
	if p := view.SafePath(r.PostFormValue("return_to"), ""); strings.HasPrefix(p, "/admin") {
		return p
	}
	return fallback
}
