package review

import (
	"errors"
	"net/http"
	"net/url"
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

type formPage struct {
	Target   Target
	ReviewID int64 // editing when set
	Rating   int
	Comment  string
	ReturnTo string
	Errors   validate.Errors
	Error    string
}

// StarOptions are the rating radios in DOM order (5 → 1, shown 1 → 5).
func (formPage) StarOptions() []int { return []int{5, 4, 3, 2, 1} }

// BackURL is where the back arrow and the redirect after saving go.
func (p formPage) BackURL() string { return view.SafePath(p.ReturnTo, "/account/reviews") }

// NewForm renders the review form for one purchased item (?item=ID).
func (h *Handler) NewForm(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	itemID, _ := strconv.ParseInt(r.URL.Query().Get("item"), 10, 64)
	returnTo := view.SafePath(r.URL.Query().Get("return_to"), "")
	t, err := h.svc.Target(r.Context(), p.UserID, itemID)
	switch {
	case err == nil && t.ReviewID != 0:
		http.Redirect(w, r, editPath(t.ReviewID, returnTo), http.StatusSeeOther)
	case err == nil:
		h.view.Render(w, r, http.StatusOK, "review/form", formPage{Target: t, ReturnTo: returnTo})
	default:
		h.fail(w, r, err)
	}
}

// Create handles POST /account/reviews.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	itemID, _ := strconv.ParseInt(r.PostForm.Get("item"), 10, 64)
	in := Input{Rating: ParseRating(r.PostForm.Get("rating")), Comment: r.PostForm.Get("comment")}
	returnTo := view.SafePath(r.PostForm.Get("return_to"), "")

	_, err := h.svc.Create(r.Context(), p.UserID, itemID, in)
	if err == nil {
		h.view.SetFlash(w, "success", "Terima kasih! Ulasan Anda sudah terkirim.")
		http.Redirect(w, r, view.SafePath(returnTo, "/account/reviews"), http.StatusSeeOther)
		return
	}
	var verr validate.Errors
	if errors.As(err, &verr) {
		t, terr := h.svc.Target(r.Context(), p.UserID, itemID)
		if terr != nil {
			h.fail(w, r, terr)
			return
		}
		h.view.Render(w, r, http.StatusUnprocessableEntity, "review/form",
			formPage{Target: t, Rating: in.Rating, Comment: in.Comment, ReturnTo: returnTo, Errors: verr})
		return
	}
	if errors.Is(err, ErrAlreadyReviewed) {
		if t, terr := h.svc.Target(r.Context(), p.UserID, itemID); terr == nil && t.ReviewID != 0 {
			h.view.SetFlash(w, "info", Message(err))
			http.Redirect(w, r, editPath(t.ReviewID, returnTo), http.StatusSeeOther)
			return
		}
	}
	h.fail(w, r, err)
}

// EditForm renders /account/reviews/{id}/edit for the user's own review.
func (h *Handler) EditForm(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	rv, err := h.svc.Get(r.Context(), p.UserID, pathID(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.view.Render(w, r, http.StatusOK, "review/form", formPage{Target: rv.Target(), ReviewID: rv.ID,
		Rating: rv.Rating, Comment: rv.Comment, ReturnTo: view.SafePath(r.URL.Query().Get("return_to"), "")})
}

// Update handles POST /account/reviews/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := view.ParseForm(w, r); err != nil {
		h.view.Error(w, r, http.StatusBadRequest)
		return
	}
	id := pathID(r)
	in := Input{Rating: ParseRating(r.PostForm.Get("rating")), Comment: r.PostForm.Get("comment")}
	returnTo := view.SafePath(r.PostForm.Get("return_to"), "")

	err := h.svc.Update(r.Context(), p.UserID, id, in)
	if err == nil {
		h.view.SetFlash(w, "success", "Ulasan Anda sudah diperbarui.")
		http.Redirect(w, r, view.SafePath(returnTo, "/account/reviews"), http.StatusSeeOther)
		return
	}
	var verr validate.Errors
	if errors.As(err, &verr) {
		rv, gerr := h.svc.Get(r.Context(), p.UserID, id)
		if gerr != nil {
			h.fail(w, r, gerr)
			return
		}
		h.view.Render(w, r, http.StatusUnprocessableEntity, "review/form", formPage{Target: rv.Target(), ReviewID: id,
			Rating: in.Rating, Comment: in.Comment, ReturnTo: returnTo, Errors: verr})
		return
	}
	h.fail(w, r, err)
}

type historyPage struct {
	Result HistoryPage
}

// TabURL links to a tab of "Ulasan Saya".
func (historyPage) TabURL(tab string) string {
	if tab == "" {
		return "/account/reviews"
	}
	return "/account/reviews?tab=" + url.QueryEscape(tab)
}

// NextURL is the "Muat lebih banyak" link (also plain).
func (p historyPage) NextURL() string {
	v := url.Values{"page": {strconv.Itoa(p.Result.Page + 1)}}
	if p.Result.Tab != "" {
		v.Set("tab", p.Result.Tab)
	}
	return "/account/reviews?" + v.Encode()
}

// History renders /account/reviews (?tab=pending, ?page=N).
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	res, err := h.svc.History(r.Context(), p.UserID, r.URL.Query().Get("tab"), page)
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	data := historyPage{Result: res}
	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "reviews-more" {
		h.view.Fragment(w, r, http.StatusOK, "review/history", "review-cards", data)
		return
	}
	h.view.Render(w, r, http.StatusOK, "review/history", data)
}

// ProductReviews renders /products/{slug}/reviews?page=N (public); HTMX
// "load more" gets only the next items.
func (h *Handler) ProductReviews(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	res, err := h.svc.ProductPageBySlug(r.Context(), chi.URLParam(r, "slug"), page)
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.view.ServerError(w, r, err)
		return
	}
	if view.IsHTMX(r) && r.Header.Get("HX-Target") == "reviews-more" {
		h.view.Fragment(w, r, http.StatusOK, "review/product", "review-items-fragment", res)
		return
	}
	h.view.Render(w, r, http.StatusOK, "review/product", res)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		h.view.Error(w, r, http.StatusNotFound)
		return
	}
	if msg := Message(err); msg != "" {
		h.view.SetFlash(w, "error", msg)
		http.Redirect(w, r, "/account/reviews", http.StatusSeeOther)
		return
	}
	h.view.ServerError(w, r, err)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

func editPath(id int64, returnTo string) string {
	p := "/account/reviews/" + strconv.FormatInt(id, 10) + "/edit"
	if returnTo != "" {
		p += "?return_to=" + url.QueryEscape(returnTo)
	}
	return p
}
