package api

import (
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/checkout"
	"ecommerce/internal/order"
)

// ---------------------------------------------------------------- cart

func (h *Handler) cart(w http.ResponseWriter, r *http.Request) {
	c, err := h.Cart.Get(r.Context(), userID(r)) // one query, lines ready to render
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, http.StatusOK, cartOut(c))
}

type addItemReq struct {
	VariantID int64 `json:"variant_id"`
	Quantity  int   `json:"quantity"`
}

// addCartItem adds a variant (merging with the same variant already in the
// cart) and returns the whole cart.
func (h *Handler) addCartItem(w http.ResponseWriter, r *http.Request) {
	var req addItemReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.VariantID <= 0 {
		h.fail(w, r, invalid(map[string]string{"variant_id": "Pilih varian produk."}))
		return
	}
	if _, err := h.Cart.AddItem(r.Context(), userID(r), req.VariantID, req.Quantity); err != nil {
		h.fail(w, r, err)
		return
	}
	h.cartStatus(w, r, http.StatusCreated)
}

type quantityReq struct {
	Quantity int `json:"quantity"`
}

func (h *Handler) updateCartItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req quantityReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Cart.UpdateQuantity(r.Context(), userID(r), id, req.Quantity); err != nil {
		h.fail(w, r, err)
		return
	}
	h.cartStatus(w, r, http.StatusOK)
}

func (h *Handler) deleteCartItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err == nil {
		err = h.Cart.RemoveItem(r.Context(), userID(r), id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.cartStatus(w, r, http.StatusOK)
}

func (h *Handler) clearCart(w http.ResponseWriter, r *http.Request) {
	if err := h.Cart.Clear(r.Context(), userID(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	noContent(w)
}

// cartStatus answers a cart change with the current cart, so the app needs
// no second request.
func (h *Handler) cartStatus(w http.ResponseWriter, r *http.Request, status int) {
	c, err := h.Cart.Get(r.Context(), userID(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, status, cartOut(c))
}

// ---------------------------------------------------------------- checkout

type buyNowReq struct {
	VariantID int64 `json:"variant_id"`
	Quantity  int   `json:"quantity"`
}

// checkoutReq selects what to buy: cart lines, or one variant "Buy Now"
// (never stored in the cart), the address and the shipping option (a quote
// id from the preview or POST /shipping/rates). Prices, stock, weights,
// shipping cost and totals always come from the server: there is no
// shipping_cost field (an unknown field is refused). expected_total only
// guards against a change since the preview.
type checkoutReq struct {
	CartItemIDs     []int64    `json:"cart_item_ids"`
	BuyNow          *buyNowReq `json:"buy_now"`
	AddressID       int64      `json:"address_id"`
	ShippingQuoteID string     `json:"shipping_quote_id"`
	IdempotencyKey  string     `json:"idempotency_key"`
	ExpectedTotal   int64      `json:"expected_total"`
}

func (q checkoutReq) source() (checkout.Source, error) {
	switch {
	case len(q.CartItemIDs) > 0 && q.BuyNow != nil:
		return checkout.Source{}, invalid(map[string]string{"buy_now": "Pilih item keranjang atau beli langsung, bukan keduanya."})
	case q.BuyNow != nil:
		if q.BuyNow.VariantID <= 0 {
			return checkout.Source{}, invalid(map[string]string{"buy_now.variant_id": "Pilih varian produk."})
		}
		return checkout.Source{VariantID: q.BuyNow.VariantID, Quantity: q.BuyNow.Quantity}, nil
	case len(q.CartItemIDs) > 0:
		if len(q.CartItemIDs) > 50 {
			return checkout.Source{}, invalid(map[string]string{"cart_item_ids": "Maksimal 50 item."})
		}
		return checkout.Source{CartItemIDs: q.CartItemIDs}, nil
	}
	return checkout.Source{}, invalid(map[string]string{"cart_item_ids": "Pilih produk yang ingin dibeli terlebih dahulu."})
}

// checkoutPreview recalculates the selection from current prices and stock
// (checkout.Service.Prepare, the web's checkout page logic).
func (h *Handler) checkoutPreview(w http.ResponseWriter, r *http.Request) {
	var req checkoutReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.IdempotencyKey != "" || req.ExpectedTotal != 0 {
		h.fail(w, r, invalid(map[string]string{"idempotency_key": "Hanya untuk POST /orders."}))
		return
	}
	src, err := req.source()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d, err := h.Checkout.Prepare(r.Context(), userID(r), src, req.AddressID, req.ShippingQuoteID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Prepare falls back to the primary address; an address that is not the
	// caller's must not be silently replaced.
	if req.AddressID != 0 && (d.Address == nil || d.Address.ID != req.AddressID) {
		h.fail(w, r, invalid(map[string]string{"address_id": "Alamat tidak ditemukan."}))
		return
	}
	ok(w, http.StatusOK, previewOut(d))
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// createOrder places the order (checkout.Service.PlaceOrder: one
// transaction, stock guarded, snapshot copied). The idempotency key makes a
// retry return the same order (200 instead of 201).
func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	var req checkoutReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	src, err := req.source()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	fields := map[string]string{}
	if !keyPattern.MatchString(req.IdempotencyKey) {
		fields["idempotency_key"] = "Wajib diisi: 8–64 karakter huruf, angka, - atau _ (unik per pesanan)."
	}
	if req.AddressID <= 0 {
		fields["address_id"] = "Pilih alamat pengiriman terlebih dahulu."
	}
	if req.ShippingQuoteID == "" || len(req.ShippingQuoteID) > 64 {
		fields["shipping_quote_id"] = "Pilih pengiriman (quote_id dari preview checkout atau /shipping/rates)."
	}
	if req.ExpectedTotal < 0 {
		fields["expected_total"] = "Tidak boleh negatif."
	}
	if len(fields) > 0 {
		h.fail(w, r, invalid(fields))
		return
	}
	uid := userID(r)
	placed, err := h.Checkout.PlaceOrder(r.Context(), uid, checkout.PlaceRequest{Source: src, AddressID: req.AddressID,
		ShippingQuoteID: req.ShippingQuoteID, ExpectedTotal: req.ExpectedTotal, IdempotencyKey: req.IdempotencyKey})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	o, err := h.Orders.Get(r.Context(), uid, placed.OrderNumber)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if placed.Existing {
		status = http.StatusOK
	}
	w.Header().Set("Location", Prefix+"/orders/"+placed.OrderNumber)
	ok(w, status, orderOut(o))
}

// ---------------------------------------------------------------- orders

var orderTabs = map[string]bool{"": true, "unpaid": true, "processing": true, "shipped": true, "completed": true, "cancelled": true}

// orders lists the user's orders (one query); status filters by the same
// tabs as the web history.
func (h *Handler) orders(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("status")
	if !orderTabs[tab] {
		h.fail(w, r, badRequest("Parameter tidak valid.", map[string]string{"status": "Pilih unpaid, processing, shipped, completed, atau cancelled."}))
		return
	}
	page, err := queryInt(r, "page")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Orders.List(r.Context(), userID(r), tab, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	okPage(w, orderSummaries(p.Orders), pageMeta{Page: p.Page, PerPage: order.PageSize, HasMore: p.HasNext})
}

func (h *Handler) order(w http.ResponseWriter, r *http.Request) {
	o, err := h.Orders.Get(r.Context(), userID(r), chi.URLParam(r, "number"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ok(w, http.StatusOK, orderOut(o))
}

// cancelOrder cancels an unpaid order (payment.Service.CancelOrder: the
// provider transaction first, stock back once).
func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	uid, number := userID(r), chi.URLParam(r, "number")
	if err := h.Payments.CancelOrder(r.Context(), uid, number); err != nil {
		h.fail(w, r, err)
		return
	}
	h.order(w, r)
}

type paymentStartDTO struct {
	OrderNumber string      `json:"order_number"`
	OrderStatus string      `json:"order_status"`
	RedirectURL string      `json:"redirect_url"`
	Payment     *paymentDTO `json:"payment"`
	Sandbox     bool        `json:"sandbox"`
}

// startPayment returns the Midtrans (sandbox) checkout URL to open in a
// browser/in-app browser; Midtrans sends the buyer back to the configured
// app link. Opening it changes nothing: only the provider's notification
// or status check marks an order paid.
func (h *Handler) startPayment(w http.ResponseWriter, r *http.Request) {
	uid, number := userID(r), chi.URLParam(r, "number")
	url, err := h.Payments.PayFromApp(r.Context(), uid, number)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	o, err := h.Orders.Get(r.Context(), uid, number)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := paymentStartDTO{OrderNumber: o.Number, OrderStatus: string(o.Status), RedirectURL: url, Sandbox: o.Sandbox}
	if p := o.LastPayment(); p != nil {
		out.Payment = paymentOut(*p)
	}
	ok(w, http.StatusOK, out)
}

type paymentStatusDTO struct {
	OrderNumber string      `json:"order_number"`
	OrderStatus string      `json:"order_status"`
	Paid        bool        `json:"paid"`
	CanPay      bool        `json:"can_pay"`
	Payment     *paymentDTO `json:"payment"`
	Sandbox     bool        `json:"sandbox"`
}

// paymentStatus is what the app asks after returning from the provider: the
// server checks the status with the provider (never trusting the redirect)
// and answers the order's current state.
func (h *Handler) paymentStatus(w http.ResponseWriter, r *http.Request) {
	o, err := h.Orders.Sync(r.Context(), userID(r), chi.URLParam(r, "number"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := paymentStatusDTO{OrderNumber: o.Number, OrderStatus: string(o.Status), Paid: o.PaidAt != nil, CanPay: o.CanPay(), Sandbox: o.Sandbox}
	if p := o.LastPayment(); p != nil {
		out.Payment = paymentOut(*p)
	}
	ok(w, http.StatusOK, out)
}
