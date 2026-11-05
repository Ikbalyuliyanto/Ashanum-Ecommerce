package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/returns"
	"ecommerce/internal/review"
)

// ---------------------------------------------------------------- reviews

type reviewsPageMeta struct {
	pageMeta
	Tab          string `json:"tab"`
	PendingCount int    `json:"pending_count"`
}

// myReviews is "Ulasan Saya": the user's reviews, or with tab=pending the
// purchased items still waiting for a review.
func (h *Handler) myReviews(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab != "" && tab != "pending" {
		h.fail(w, r, badRequest("Parameter tidak valid.", map[string]string{"tab": "Kosongkan atau gunakan pending."}))
		return
	}
	page, err := queryInt(r, "page")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Reviews.History(r.Context(), userID(r), tab, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	meta := reviewsPageMeta{pageMeta: pageMeta{Page: p.Page, PerPage: review.PageSize, HasMore: p.HasNext}, Tab: tab, PendingCount: p.PendingCount}
	if tab == "pending" {
		out := make([]pendingReviewDTO, len(p.Pending))
		for i, it := range p.Pending {
			out[i] = pendingReviewDTO{OrderItemID: it.OrderItemID, ProductName: it.ProductName, VariantLabel: it.VariantLabel,
				ImageURL: nonEmpty(img(it.ImageURL, thumbWidth)), OrderNumber: it.OrderNumber, CompletedAt: tsp(it.CompletedAt)}
		}
		okPage(w, out, meta)
		return
	}
	out := make([]myReviewDTO, len(p.Reviews))
	for i, rv := range p.Reviews {
		out[i] = myReview(rv)
	}
	okPage(w, out, meta)
}

type reviewReq struct {
	OrderItemID int64   `json:"order_item_id"`
	Rating      *int    `json:"rating"`
	Comment     *string `json:"comment"`
}

// createReview reviews one item of the user's completed order
// (review.Service.Create: eligibility, one per item, rating recalculated).
func (h *Handler) createReview(w http.ResponseWriter, r *http.Request) {
	var req reviewReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	fields := map[string]string{}
	if req.OrderItemID <= 0 {
		fields["order_item_id"] = "Pilih barang yang diulas."
	}
	if req.Rating == nil {
		fields["rating"] = "Pilih rating 1 sampai 5 bintang."
	}
	if len(fields) > 0 {
		h.fail(w, r, invalid(fields))
		return
	}
	in := review.Input{Rating: *req.Rating}
	set(&in.Comment, req.Comment)
	uid := userID(r)
	id, err := h.Reviews.Create(r.Context(), uid, req.OrderItemID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.reviewStatus(w, r, http.StatusCreated, uid, id)
}

// updateReview changes the user's own review (only the fields sent).
func (h *Handler) updateReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req reviewReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.OrderItemID != 0 {
		h.fail(w, r, invalid(map[string]string{"order_item_id": "Tidak bisa diubah."}))
		return
	}
	uid := userID(r)
	cur, err := h.Reviews.Get(r.Context(), uid, id) // another user's review is ErrNotFound
	if err != nil {
		h.fail(w, r, err)
		return
	}
	in := review.Input{Rating: cur.Rating, Comment: cur.Comment}
	if req.Rating != nil {
		in.Rating = *req.Rating
	}
	set(&in.Comment, req.Comment)
	if err := h.Reviews.Update(r.Context(), uid, id, in); err != nil {
		h.fail(w, r, err)
		return
	}
	h.reviewStatus(w, r, http.StatusOK, uid, id)
}

func (h *Handler) reviewStatus(w http.ResponseWriter, r *http.Request, status int, uid, id int64) {
	rv, err := h.Reviews.Get(r.Context(), uid, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, status, myReview(rv))
}

// ---------------------------------------------------------------- returns

func (h *Handler) myReturns(w http.ResponseWriter, r *http.Request) {
	page, err := queryInt(r, "page")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Returns.List(r.Context(), userID(r), page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	okPage(w, returnSummaries(p.Returns), pageMeta{Page: p.Page, PerPage: returns.PageSize, HasMore: p.HasNext})
}

func (h *Handler) myReturn(w http.ResponseWriter, r *http.Request) {
	h.returnStatus(w, r, http.StatusOK, chi.URLParam(r, "number"))
}

type returnItemReq struct {
	OrderItemID int64 `json:"order_item_id"`
	Quantity    int   `json:"quantity"`
}

type returnReq struct {
	OrderNumber    string          `json:"order_number"`
	Items          []returnItemReq `json:"items"`
	Type           string          `json:"type"`
	Reason         string          `json:"reason"`
	Description    string          `json:"description"`
	Evidence       []string        `json:"evidence"`
	IdempotencyKey string          `json:"idempotency_key"`
}

// createReturn requests a refund or exchange (returns.Service.Create:
// eligibility, window, quantities, amount from the prices paid).
func (h *Handler) createReturn(w http.ResponseWriter, r *http.Request) {
	var req returnReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.OrderNumber == "" {
		h.fail(w, r, invalid(map[string]string{"order_number": "Pilih pesanan."}))
		return
	}
	if len(req.Items) > 50 || len(req.Evidence) > 20 {
		h.fail(w, r, invalid(map[string]string{"items": "Terlalu banyak data."}))
		return
	}
	in := returns.Request{OrderNumber: req.OrderNumber, Type: returns.Type(req.Type), Reason: returns.Reason(req.Reason),
		Description: req.Description, Evidence: req.Evidence, IdempotencyKey: req.IdempotencyKey}
	for _, it := range req.Items {
		in.Items = append(in.Items, returns.ItemRequest{OrderItemID: it.OrderItemID, Quantity: it.Quantity})
	}
	number, err := h.Returns.Create(r.Context(), userID(r), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/me/returns/"+number)
	h.returnStatus(w, r, http.StatusCreated, number)
}

// cancelReturn withdraws the user's request while it waits for review.
func (h *Handler) cancelReturn(w http.ResponseWriter, r *http.Request) {
	number := chi.URLParam(r, "number")
	if err := h.Returns.CancelByUser(r.Context(), userID(r), number); err != nil {
		h.fail(w, r, err)
		return
	}
	h.returnStatus(w, r, http.StatusOK, number)
}

func (h *Handler) returnStatus(w http.ResponseWriter, r *http.Request, status int, number string) {
	rt, err := h.Returns.Get(r.Context(), userID(r), number) // another user's return is ErrNotFound
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, status, returnOut(rt))
}
