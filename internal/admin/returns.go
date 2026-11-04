package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/returns"
	"ecommerce/internal/view"
)

type returnsPage struct {
	shell
	Page  returns.AdminPage
	Tabs  []returns.AdminTab
	Tab   returns.AdminTab
	Pager pager
}

func (h *Handler) returnList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := returns.AdminFilter{Tab: q.Get("status"), Query: q.Get("q"), Page: queryInt(r, "page"), PageSize: perPage(r)}
	p, err := h.Returns.AdminList(r.Context(), f)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/returns", returnsPage{shell: shell{Nav: navReturns}, Page: p, Tabs: returns.AdminTabs,
		Tab: returns.AdminTabByKey(f.Tab), Pager: newPager(r, p.Filter.Page, p.HasNext)})
}

// estimate is the live approval calculation (HTMX fragment).
type estimate struct {
	Approved  int
	Requested int
	Amount    int64
	Exchange  bool
}

type returnPage struct {
	shell
	Return   returns.Return
	Audit    []audit.Log
	Estimate estimate
}

func (h *Handler) returnDetail(w http.ResponseWriter, r *http.Request) {
	rt, err := h.Returns.AdminGet(r.Context(), chi.URLParam(r, "number"))
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	logs, err := h.Audit.For(r.Context(), audit.EntityReturn, rt.ID, 30)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/return", returnPage{shell: shell{Nav: navReturns}, Return: rt, Audit: logs,
		Estimate: estimateFor(rt, nil)})
}

// estimateFor computes the approval preview from the chosen quantities
// (nil: everything requested). The service computes the real amount again.
func estimateFor(rt returns.Return, chosen map[int64]int) estimate {
	e := estimate{Exchange: rt.Type == returns.TypeExchange}
	for _, it := range rt.Items {
		n := it.Quantity
		if chosen != nil {
			n = min(max(chosen[it.ID], 0), it.Quantity)
		}
		e.Requested += it.Quantity
		e.Approved += n
		if !e.Exchange {
			e.Amount += it.UnitPrice * int64(n)
		}
	}
	return e
}

// approvedQuantities reads qty_<line id> fields of the approval form.
func approvedQuantities(r *http.Request, rt returns.Return, form func(string) string) map[int64]int {
	q := map[int64]int{}
	for _, it := range rt.Items {
		if v := form("qty_" + strconv.FormatInt(it.ID, 10)); v != "" {
			q[it.ID] = parseInt(v)
		}
	}
	return q
}

func (h *Handler) returnEstimate(w http.ResponseWriter, r *http.Request) {
	rt, err := h.Returns.AdminGet(r.Context(), chi.URLParam(r, "number"))
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	e := estimateFor(rt, approvedQuantities(r, rt, r.URL.Query().Get))
	h.View.Fragment(w, r, http.StatusOK, "admin/return", "approve-estimate-fragment", e)
}

func returnPath(number string) string { return "/admin/returns/" + number }

// returnAction parses the form, resolves the return and runs fn.
func (h *Handler) returnAction(w http.ResponseWriter, r *http.Request, msg string, fn func(id int64, rt *returns.Return) error) {
	number := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	id, err := h.Returns.IDByNumber(r.Context(), number)
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	if err := fn(id, nil); err != nil {
		h.refuse(w, r, returnPath(number), err, returns.AdminMessage)
		return
	}
	h.done(w, r, returnPath(number), msg)
}

func (h *Handler) returnApprove(w http.ResponseWriter, r *http.Request) {
	number := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	rt, err := h.Returns.AdminGet(r.Context(), number)
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	in := returns.ApproveInput{Quantities: approvedQuantities(r, rt, r.PostFormValue), Note: r.PostFormValue("note")}
	if err := h.Returns.Approve(r.Context(), adminID(r), rt.ID, in); err != nil {
		h.refuse(w, r, returnPath(number), err, returns.AdminMessage)
		return
	}
	h.done(w, r, returnPath(number), "Pengembalian disetujui.")
}

func (h *Handler) returnReject(w http.ResponseWriter, r *http.Request) {
	h.returnAction(w, r, "Pengembalian ditolak.", func(id int64, _ *returns.Return) error {
		return h.Returns.Reject(r.Context(), adminID(r), id, r.PostFormValue("note"))
	})
}

func (h *Handler) returnWaiting(w http.ResponseWriter, r *http.Request) {
	h.returnAction(w, r, "Status: menunggu pembeli mengirim barang.", func(id int64, _ *returns.Return) error {
		return h.Returns.MarkWaitingReturn(r.Context(), adminID(r), id, r.PostFormValue("note"))
	})
}

func (h *Handler) returnReceived(w http.ResponseWriter, r *http.Request) {
	number := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	rt, err := h.Returns.AdminGet(r.Context(), number)
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	in := returns.ReceiveInput{Conditions: map[int64]returns.Condition{}, Note: r.PostFormValue("note")}
	for _, it := range rt.Items {
		in.Conditions[it.ID] = returns.Condition(r.PostFormValue("condition_" + strconv.FormatInt(it.ID, 10)))
	}
	if err := h.Returns.MarkItemReceived(r.Context(), adminID(r), rt.ID, in); err != nil {
		h.refuse(w, r, returnPath(number), err, returns.AdminMessage)
		return
	}
	h.done(w, r, returnPath(number), "Barang diterima dan diperiksa. Stok belum berubah: gunakan Kembalikan ke Stok untuk barang layak jual.")
}

func (h *Handler) returnRestock(w http.ResponseWriter, r *http.Request) {
	h.returnActionMsg(w, r, func(id int64) (string, error) {
		units, err := h.Returns.Restock(r.Context(), adminID(r), id)
		return fmt.Sprintf("%d barang layak jual dikembalikan ke stok.", units), err
	}, chi.URLParam(r, "number"))
}

// returnActionMsg is returnAction with a message computed by fn.
func (h *Handler) returnActionMsg(w http.ResponseWriter, r *http.Request, fn func(id int64) (string, error), number string) {
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	id, err := h.Returns.IDByNumber(r.Context(), number)
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	msg, err := fn(id)
	if err != nil {
		h.refuse(w, r, returnPath(number), err, returns.AdminMessage)
		return
	}
	h.done(w, r, returnPath(number), msg)
}

func (h *Handler) returnPrepareRefund(w http.ResponseWriter, r *http.Request) {
	h.returnActionMsg(w, r, func(id int64) (string, error) {
		f, err := h.Returns.PrepareRefund(r.Context(), adminID(r), id)
		return fmt.Sprintf("Refund %s dicatat (menunggu diproses). Tidak ada dana yang dikirim otomatis.", view.FormatRupiah(f.Amount)), err
	}, chi.URLParam(r, "number"))
}

func refundResult(r *http.Request) returns.RefundResult {
	return returns.RefundResult{Provider: r.PostFormValue("method"), Reference: r.PostFormValue("reference"), Note: r.PostFormValue("note")}
}

func (h *Handler) returnCompleteRefund(w http.ResponseWriter, r *http.Request) {
	h.returnAction(w, r, "Refund dicatat berhasil (manual).", func(id int64, _ *returns.Return) error {
		return h.Returns.CompleteRefund(r.Context(), adminID(r), id, refundResult(r))
	})
}

func (h *Handler) returnCompleteExchange(w http.ResponseWriter, r *http.Request) {
	number := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	rt, err := h.Returns.AdminGet(r.Context(), number)
	if errors.Is(err, returns.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	in := returns.ExchangeInput{Replacements: map[int64]int64{}, Note: r.PostFormValue("note")}
	for _, it := range rt.Items {
		in.Replacements[it.ID] = int64(parseInt(r.PostFormValue("replacement_" + strconv.FormatInt(it.ID, 10))))
	}
	if err := h.Returns.CompleteExchange(r.Context(), adminID(r), rt.ID, in); err != nil {
		h.refuse(w, r, returnPath(number), err, returns.AdminMessage)
		return
	}
	h.done(w, r, returnPath(number), "Barang pengganti dicatat dikirim; stok pengganti sudah dikurangi.")
}

func (h *Handler) returnComplete(w http.ResponseWriter, r *http.Request) {
	h.returnAction(w, r, "Pengembalian selesai.", func(id int64, _ *returns.Return) error {
		return h.Returns.Complete(r.Context(), adminID(r), id, r.PostFormValue("note"))
	})
}

func (h *Handler) returnCancel(w http.ResponseWriter, r *http.Request) {
	h.returnAction(w, r, "Pengembalian dibatalkan.", func(id int64, _ *returns.Return) error {
		return h.Returns.Cancel(r.Context(), adminID(r), id, r.PostFormValue("note"))
	})
}

// ---------------------------------------------------------------- refunds

type refundsPage struct {
	shell
	Page  returns.AdminPage
	Tabs  []returns.RefundTab
	Tab   returns.RefundTab
	Pager pager
}

func (h *Handler) refunds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := returns.AdminFilter{Tab: q.Get("status"), Query: q.Get("q"), Page: queryInt(r, "page"), PageSize: perPage(r)}
	p, err := h.Returns.AdminRefunds(r.Context(), f)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/refunds", refundsPage{shell: shell{Nav: navReturns}, Page: p, Tabs: returns.RefundTabs,
		Tab: returns.RefundTabByKey(f.Tab), Pager: newPager(r, p.Filter.Page, p.HasNext)})
}

type refundPage struct {
	shell
	Refund returns.Refund
	Audit  []audit.Log
}

func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	f, err := h.Returns.AdminRefund(r.Context(), id)
	if errors.Is(err, returns.ErrRefundNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	logs, err := h.Audit.For(r.Context(), audit.EntityRefund, id, 20)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/refund", refundPage{shell: shell{Nav: navReturns}, Refund: f, Audit: logs})
}

func (h *Handler) refundAction(w http.ResponseWriter, r *http.Request, msg string, fn func(id int64) error) {
	id := pathID(r, "id")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	to := returnTo(r, fmt.Sprintf("/admin/refunds/%d", id))
	err := fn(id)
	switch {
	case errors.Is(err, returns.ErrRefundNotFound):
		h.View.Error(w, r, http.StatusNotFound)
	case err != nil:
		h.refuse(w, r, to, err, returns.AdminMessage)
	default:
		h.done(w, r, to, msg)
	}
}

func (h *Handler) refundProcessing(w http.ResponseWriter, r *http.Request) {
	h.refundAction(w, r, "Refund ditandai sedang diproses.", func(id int64) error {
		return h.Returns.MarkRefundProcessing(r.Context(), adminID(r), id, r.PostFormValue("note"))
	})
}

func (h *Handler) refundComplete(w http.ResponseWriter, r *http.Request) {
	h.refundAction(w, r, "Refund dicatat berhasil (manual).", func(id int64) error {
		return h.Returns.CompleteRefundByID(r.Context(), adminID(r), id, refundResult(r))
	})
}

func (h *Handler) refundFail(w http.ResponseWriter, r *http.Request) {
	h.refundAction(w, r, "Refund ditandai gagal.", func(id int64) error {
		return h.Returns.FailRefund(r.Context(), adminID(r), id, r.PostFormValue("reason"))
	})
}
