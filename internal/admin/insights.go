package admin

import (
	"errors"
	"net/http"
	"time"

	"ecommerce/internal/audit"
	"ecommerce/internal/report"
	"ecommerce/internal/review"
	"ecommerce/internal/view"
)

// ---------------------------------------------------------------- reviews

type reviewsPage struct {
	shell
	Page  review.AdminPage
	Pager pager
}

func (h *Handler) reviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := review.AdminFilter{Status: q.Get("status"), Rating: queryInt(r, "rating"), Query: q.Get("q"), Page: queryInt(r, "page"), PageSize: perPage(r)}
	p, err := h.Reviews.AdminList(r.Context(), f)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/reviews", reviewsPage{shell: shell{Nav: navReviews}, Page: p, Pager: newPager(r, p.Filter.Page, p.HasNext)})
}

func (h *Handler) reviewHide(w http.ResponseWriter, r *http.Request) {
	h.moderate(w, r, func(id int64) error { return h.Reviews.Hide(r.Context(), adminID(r), id, r.PostFormValue("reason")) },
		"Ulasan disembunyikan dan rating produk dihitung ulang.")
}

func (h *Handler) reviewUnhide(w http.ResponseWriter, r *http.Request) {
	h.moderate(w, r, func(id int64) error { return h.Reviews.Unhide(r.Context(), adminID(r), id) },
		"Ulasan ditampilkan kembali dan rating produk dihitung ulang.")
}

func (h *Handler) moderate(w http.ResponseWriter, r *http.Request, fn func(id int64) error, msg string) {
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	to := returnTo(r, "/admin/reviews")
	err := fn(pathID(r, "id"))
	switch {
	case errors.Is(err, review.ErrNotFound):
		h.View.Error(w, r, http.StatusNotFound)
	case errors.Is(err, review.ErrModerated):
		h.done(w, r, to, "") // already in that state (double click)
	case err != nil:
		h.refuse(w, r, to, err)
	default:
		h.done(w, r, to, msg)
	}
}

// ---------------------------------------------------------------- reports

type reportsPage struct {
	shell
	Report   report.Report
	BadRange bool
}

func (h *Handler) reports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rg, err := report.ParseRange(q.Get("range"), q.Get("from"), q.Get("to"), time.Now())
	bad := errors.Is(err, report.ErrBadRange)
	rep, err := h.Reports.Build(r.Context(), rg)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/reports", reportsPage{shell: shell{Nav: navReports}, Report: rep, BadRange: bad})
}

// ---------------------------------------------------------------- audit

type auditPage struct {
	shell
	Page   audit.Page
	Entity string
	Types  []string
	Pager  pager
}

func (h *Handler) auditLog(w http.ResponseWriter, r *http.Request) {
	entity := r.URL.Query().Get("entity")
	types := []string{audit.EntityOrder, audit.EntityProduct, audit.EntityVariant, audit.EntityCategory,
		audit.EntityReturn, audit.EntityRefund, audit.EntityReview, audit.EntityUser}
	known := false
	for _, t := range types {
		known = known || t == entity
	}
	if !known {
		entity = ""
	}
	p, err := h.Audit.List(r.Context(), audit.Filter{EntityType: entity, Page: queryInt(r, "page"), PageSize: perPage(r)})
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/audit", auditPage{shell: shell{Nav: navReports}, Page: p, Entity: entity, Types: types,
		Pager: newPager(r, p.Page, p.HasNext)})
}
