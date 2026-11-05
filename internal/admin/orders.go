package admin

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/fulfillment"
	"ecommerce/internal/payment"
	"ecommerce/internal/report"
	"ecommerce/internal/shipping"
	"ecommerce/internal/validate"
	"ecommerce/internal/view"
)

type dashboardPage struct {
	shell
	D report.Dashboard
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := h.Reports.Dashboard(r.Context())
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/dashboard", dashboardPage{shell: shell{Nav: navDashboard}, D: d})
}

type ordersPage struct {
	shell
	Page  fulfillment.Page
	Tabs  []fulfillment.Tab
	Pager pager
}

func (h *Handler) orders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := h.Orders.List(r.Context(), fulfillment.Filter{Tab: q.Get("status"), Query: q.Get("q"), Page: queryInt(r, "page"), PageSize: perPage(r)})
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	h.View.Render(w, r, http.StatusOK, "admin/orders", ordersPage{shell: shell{Nav: navOrders}, Page: p, Tabs: fulfillment.Tabs,
		Pager: newPager(r, p.Filter.Page, p.HasNext)})
}

type orderPage struct {
	shell
	Order  fulfillment.Detail
	Audit  []audit.Log
	Ship   fulfillment.ShipInput
	Errors validate.Errors
	// BookingEnabled: a shipping provider is configured.
	BookingEnabled bool
	ProviderName   string
}

func (h *Handler) order(w http.ResponseWriter, r *http.Request) {
	h.renderOrder(w, r, http.StatusOK, chi.URLParam(r, "number"), fulfillment.ShipInput{}, nil)
}

func (h *Handler) renderOrder(w http.ResponseWriter, r *http.Request, status int, number string, ship fulfillment.ShipInput, errs validate.Errors) {
	d, err := h.Orders.Get(r.Context(), number)
	if errors.Is(err, fulfillment.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	logs, err := h.Audit.For(r.Context(), audit.EntityOrder, d.ID, 20)
	if err != nil {
		h.View.ServerError(w, r, err)
		return
	}
	page := orderPage{shell: shell{Nav: navOrders}, Order: d, Audit: logs, Ship: ship, Errors: errs}
	if h.Shipping != nil && h.Shipping.Enabled() {
		page.BookingEnabled, page.ProviderName = true, shipping.ProviderLabel(h.Shipping.ProviderName())
	}
	h.View.Render(w, r, status, "admin/order", page)
}

// orderBook books the buyer's courier choice with the shipping provider.
// A double submit never books twice (see shipping.Service.Book).
func (h *Handler) orderBook(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := h.shipping().Book(r.Context(), adminID(r), n); err != nil {
		h.shipmentFail(w, r, n, err)
		return
	}
	h.done(w, r, orderPath(n), "Pengiriman dibuat di penyedia. Kurir akan menjemput paket; status diperbarui otomatis.")
}

// orderShipmentSync refreshes the shipment status from the provider.
func (h *Handler) orderShipmentSync(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := h.shipping().Sync(r.Context(), adminID(r), n); err != nil {
		h.shipmentFail(w, r, n, err)
		return
	}
	h.done(w, r, orderPath(n), "Status pengiriman diperbarui dari penyedia.")
}

// orderShipmentCancel cancels the provider booking before pickup.
func (h *Handler) orderShipmentCancel(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	if err := h.shipping().CancelBooking(r.Context(), adminID(r), n, r.PostFormValue("reason")); err != nil {
		h.shipmentFail(w, r, n, err)
		return
	}
	h.done(w, r, orderPath(n), "Pengiriman dibatalkan. Pesanan tetap diproses: buat pengiriman lagi atau kirim manual.")
}

func (h *Handler) shipping() *shipping.Service {
	if h.Shipping == nil {
		return shipping.NewService(nil, nil, shipping.Options{}) // not configured: every call is ErrNotConfigured
	}
	return h.Shipping
}

func (h *Handler) shipmentFail(w http.ResponseWriter, r *http.Request, number string, err error) {
	if errors.Is(err, shipping.ErrNotFound) && !h.orderExists(r, number) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	h.refuse(w, r, orderPath(number), err, func(err error) string {
		if errors.Is(err, shipping.ErrNotConfigured) {
			return "Penyedia pengiriman belum dikonfigurasi (BITESHIP_API_KEY). Gunakan pengiriman manual."
		}
		if errors.Is(err, shipping.ErrUnavailable) {
			return "Penyedia pengiriman tidak dapat dihubungi. Pesanan tidak berubah; coba lagi beberapa saat (permintaan yang sama tidak akan membuat pengiriman ganda)."
		}
		return shipping.Message(err)
	})
}

func (h *Handler) orderExists(r *http.Request, number string) bool {
	_, err := h.Orders.Get(r.Context(), number)
	return err == nil
}

func orderPath(number string) string { return "/admin/orders/" + number }

func (h *Handler) orderConfirm(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := h.Orders.Confirm(r.Context(), adminID(r), n); err != nil {
		h.orderFail(w, r, n, err)
		return
	}
	h.done(w, r, orderPath(n), "Pesanan dikonfirmasi dan sedang diproses.")
}

func (h *Handler) orderShip(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	in := fulfillment.ShipInput{Courier: r.PostFormValue("courier"), Service: r.PostFormValue("service"),
		Tracking: r.PostFormValue("tracking"), Note: r.PostFormValue("note")}
	err := h.Orders.Ship(r.Context(), adminID(r), n, in)
	var verr validate.Errors
	if errors.As(err, &verr) {
		h.renderOrder(w, r, http.StatusUnprocessableEntity, n, in, verr)
		return
	}
	if err != nil {
		h.orderFail(w, r, n, err)
		return
	}
	h.done(w, r, orderPath(n), "Pesanan ditandai dikirim.")
}

func (h *Handler) orderComplete(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := h.Orders.Complete(r.Context(), adminID(r), n); err != nil {
		h.orderFail(w, r, n, err)
		return
	}
	h.done(w, r, orderPath(n), "Pesanan selesai. Pembeli kini bisa memberi ulasan dan mengajukan pengembalian.")
}

func (h *Handler) orderCancel(w http.ResponseWriter, r *http.Request) {
	n := chi.URLParam(r, "number")
	if err := view.ParseForm(w, r); err != nil {
		h.View.Error(w, r, http.StatusBadRequest)
		return
	}
	res, err := h.Orders.Cancel(r.Context(), adminID(r), n, r.PostFormValue("note"))
	if err != nil {
		h.orderFail(w, r, n, err)
		return
	}
	msg := "Pesanan dibatalkan dan stok dikembalikan."
	if res.Paid {
		msg = fmt.Sprintf("Pesanan dibatalkan, stok dikembalikan, dan refund %s dicatat (belum dikirim ke pembeli).", view.FormatRupiah(res.RefundAmount))
	}
	h.done(w, r, orderPath(n), msg)
}

func (h *Handler) orderFail(w http.ResponseWriter, r *http.Request, number string, err error) {
	if errors.Is(err, fulfillment.ErrNotFound) || errors.Is(err, payment.ErrNotFound) {
		h.View.Error(w, r, http.StatusNotFound)
		return
	}
	h.refuse(w, r, orderPath(number), err, fulfillment.Message, func(err error) string {
		if errors.Is(err, payment.ErrAlreadyPaid) {
			return "Pesanan sudah dibayar; muat ulang halaman lalu batalkan sebagai pesanan berbayar."
		}
		return ""
	})
}
