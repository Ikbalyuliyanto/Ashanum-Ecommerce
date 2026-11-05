package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/checkout"
	"ecommerce/internal/order"
	"ecommerce/internal/shipping"
)

// ---------------------------------------------------------------- areas

type areaDTO struct {
	ID         string   `json:"id"`
	Provider   string   `json:"provider"`
	Label      string   `json:"label"`
	Province   string   `json:"province"`
	City       string   `json:"city"`
	District   string   `json:"district"`
	PostalCode *string  `json:"postal_code"`
	Latitude   *float64 `json:"latitude"`
	Longitude  *float64 `json:"longitude"`
}

func areaOut(a shipping.Area) areaDTO {
	return areaDTO{ID: a.ID, Provider: a.Provider, Label: a.Label, Province: a.Province, City: a.City, District: a.District,
		PostalCode: nonEmpty(a.PostalCode), Latitude: a.Latitude, Longitude: a.Longitude}
}

// shippingAreas searches delivery areas (kecamatan, city or postal code)
// for an address: GET /shipping/areas?q=. The chosen id goes into the
// address's shipping_area_id.
func (h *Handler) shippingAreas(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	areas, err := h.Shipping.SearchAreas(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]areaDTO, len(areas))
	for i, a := range areas {
		out[i] = areaOut(a)
	}
	ok(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- rates

type quoteDTO struct {
	QuoteID       string  `json:"quote_id"`
	Provider      string  `json:"provider"`
	CourierCode   string  `json:"courier_code"`
	CourierName   string  `json:"courier_name"`
	ServiceCode   string  `json:"service_code"`
	ServiceName   string  `json:"service_name"`
	Description   string  `json:"description"`
	Price         int64   `json:"price"`
	EstimatedDays *string `json:"estimated_days"`
	EstimateLabel *string `json:"estimate_label"`
	ExpiresAt     string  `json:"expires_at"`
}

func quoteOut(q shipping.Quote) quoteDTO {
	return quoteDTO{QuoteID: q.ID, Provider: q.Rate.Provider, CourierCode: q.Rate.CourierCode, CourierName: q.Rate.CourierName,
		ServiceCode: q.Rate.ServiceCode, ServiceName: q.Rate.ServiceName, Description: q.Rate.Description, Price: q.Rate.Price,
		EstimatedDays: nonEmpty(q.Rate.EstimatedDays), EstimateLabel: nonEmpty(q.Rate.EstimateLabel()), ExpiresAt: ts(q.ExpiresAt)}
}

// checkoutShippingDTO is the shipping part of a checkout preview: status
// READY with options (cheapest first) and the selected one, or why there
// are none. Cost is the selected price (0 = none selected, not free).
type checkoutShippingDTO struct {
	Status      string     `json:"status"`
	Message     *string    `json:"message"`
	Cost        int64      `json:"cost"`
	WeightGrams int        `json:"weight_grams"`
	Selected    *quoteDTO  `json:"selected"`
	Options     []quoteDTO `json:"options"`
	Changed     bool       `json:"changed"`
}

func checkoutShippingOut(d checkout.Draft) checkoutShippingDTO {
	s := d.Shipping
	out := checkoutShippingDTO{Status: string(s.State), Message: nonEmpty(s.Message()), WeightGrams: d.WeightGrams(),
		Options: make([]quoteDTO, 0, len(s.Options)), Changed: s.Changed}
	for _, q := range s.Options {
		out.Options = append(out.Options, quoteOut(q))
	}
	if s.Selected != nil {
		q := quoteOut(*s.Selected)
		out.Selected, out.Cost = &q, s.Selected.Rate.Price
	}
	return out
}

type ratesReq struct {
	CartItemIDs     []int64    `json:"cart_item_ids"`
	BuyNow          *buyNowReq `json:"buy_now"`
	AddressID       int64      `json:"address_id"`
	ShippingQuoteID string     `json:"shipping_quote_id"`
}

// shippingRates lists the shipping options for an address and a selection
// (the same calculation as the checkout preview, shipping part only):
// POST /shipping/rates. Unlike the preview it answers an error when no
// option can be shown: 503 when the provider is down, 422 when the address
// has no delivery area or a product has no weight.
func (h *Handler) shippingRates(w http.ResponseWriter, r *http.Request) {
	var req ratesReq
	if err := decode(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	src, err := checkoutReq{CartItemIDs: req.CartItemIDs, BuyNow: req.BuyNow}.source()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if req.AddressID <= 0 {
		h.fail(w, r, invalid(map[string]string{"address_id": "Pilih alamat pengiriman terlebih dahulu."}))
		return
	}
	d, err := h.Checkout.Prepare(r.Context(), userID(r), src, req.AddressID, req.ShippingQuoteID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if d.Address == nil || d.Address.ID != req.AddressID {
		h.fail(w, r, invalid(map[string]string{"address_id": "Alamat tidak ditemukan."}))
		return
	}
	switch d.Shipping.State {
	case checkout.ShippingReady:
		ok(w, http.StatusOK, checkoutShippingOut(d))
	case checkout.ShippingNeedArea:
		h.fail(w, r, &problem{status: http.StatusUnprocessableEntity, code: "SHIPPING_AREA_REQUIRED", msg: d.Shipping.Message(),
			fields: map[string]string{"address_id": d.Shipping.Message()}})
	case checkout.ShippingBlocked:
		h.fail(w, r, &problem{status: http.StatusUnprocessableEntity, code: "ITEMS_NOT_SHIPPABLE", msg: d.Shipping.Message()})
	case checkout.ShippingNoCourier:
		h.fail(w, r, &problem{status: http.StatusUnprocessableEntity, code: "NO_COURIER", msg: d.Shipping.Message()})
	default:
		h.fail(w, r, shipping.ErrUnavailable)
	}
}

// ---------------------------------------------------------------- tracking

type orderShippingDTO struct {
	Cost          int64   `json:"cost"`
	Provider      *string `json:"provider"`
	CourierCode   *string `json:"courier_code"`
	CourierName   *string `json:"courier_name"`
	ServiceCode   *string `json:"service_code"`
	ServiceName   *string `json:"service_name"`
	EstimatedDays *string `json:"estimated_days"`
	EstimateLabel *string `json:"estimate_label"`
	WeightGrams   *int    `json:"weight_grams"`
	AreaLabel     *string `json:"area_label"`
}

// orderShippingOut is the order's shipping snapshot; an order placed before
// shipping rates existed has only cost 0 and nulls.
func orderShippingOut(o order.Order) orderShippingDTO {
	d := orderShippingDTO{Cost: o.ShippingCost}
	if c := o.Shipping; c != nil {
		d.Provider, d.CourierCode, d.CourierName = &c.Provider, &c.CourierCode, &c.CourierName
		d.ServiceCode, d.ServiceName = &c.ServiceCode, &c.ServiceName
		d.EstimatedDays, d.EstimateLabel = nonEmpty(c.EstimatedDays), nonEmpty(c.EstimateLabel())
		w := c.WeightGrams
		d.WeightGrams, d.AreaLabel = &w, nonEmpty(c.AreaLabel)
	}
	return d
}

type shipmentDTO struct {
	Method         string  `json:"method"` // MANUAL | PROVIDER
	Provider       *string `json:"provider"`
	Status         string  `json:"status"`
	StatusLabel    string  `json:"status_label"`
	Courier        string  `json:"courier"`
	Service        *string `json:"service"`
	TrackingNumber *string `json:"tracking_number"`
	TrackingURL    *string `json:"tracking_url"`
	EstimatedDays  *string `json:"estimated_days"`
	ShippedAt      *string `json:"shipped_at"`
	DeliveredAt    *string `json:"delivered_at"`
	LiveTracking   bool    `json:"live_tracking"`
	LastSyncedAt   *string `json:"last_synced_at"`
}

func shipmentOut(s *order.Shipment) *shipmentDTO {
	if s == nil {
		return nil
	}
	return &shipmentDTO{Method: s.Method, Provider: nonEmpty(s.Provider), Status: string(s.Status), StatusLabel: s.Status.Label(),
		Courier: s.Courier, Service: nonEmpty(s.Service), TrackingNumber: nonEmpty(s.Tracking), TrackingURL: nonEmpty(s.TrackingURL),
		EstimatedDays: nonEmpty(s.EstimatedDays), ShippedAt: tsp(s.ShippedAt), DeliveredAt: tsp(s.DeliveredAt),
		LiveTracking: !s.Manual(), LastSyncedAt: tsp(s.LastSyncedAt)}
}

type trackingEventDTO struct {
	Status      string  `json:"status"`
	StatusLabel string  `json:"status_label"`
	Description string  `json:"description"`
	Location    *string `json:"location"`
	Time        string  `json:"time"`
}

type trackingDTO struct {
	OrderNumber string             `json:"order_number"`
	OrderStatus string             `json:"order_status"`
	StatusLabel string             `json:"order_status_label"`
	Shipping    orderShippingDTO   `json:"shipping"`
	Shipment    *shipmentDTO       `json:"shipment"`
	Events      []trackingEventDTO `json:"events"`
}

// orderTracking is the order's shipment and tracking history, newest event
// first: GET /orders/{number}/tracking. It reads stored data only (updated
// by provider webhooks, the shop, or reconciliation), never the provider.
func (h *Handler) orderTracking(w http.ResponseWriter, r *http.Request) {
	o, err := h.Orders.Get(r.Context(), userID(r), chi.URLParam(r, "number"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := trackingDTO{OrderNumber: o.Number, OrderStatus: string(o.Status), StatusLabel: o.Status.Label(),
		Shipping: orderShippingOut(o), Shipment: shipmentOut(o.Shipment), Events: []trackingEventDTO{}}
	if o.Shipment != nil {
		for _, e := range o.Shipment.Events {
			out.Events = append(out.Events, trackingEventDTO{Status: string(e.Status), StatusLabel: e.Status.Label(),
				Description: e.Text(), Location: nonEmpty(e.Location), Time: ts(e.Time)})
		}
	}
	ok(w, http.StatusOK, out)
}
