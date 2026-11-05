// Package biteshiptest is an in-memory imitation of the Biteship endpoints
// the biteship client uses (maps areas, courier rates, orders, cancel,
// trackings), answering with the documented field names, for tests without
// internet. Prices are a deterministic formula of weight, not real rates.
// It is never used by the application binary.
package biteshiptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	APIKey = "biteship_test.FAKE-TEST-ONLY"

	// WebhookHeader / WebhookSecret are what tests configure as the
	// webhook "Headers Signature Key" and "Secret".
	WebhookHeader = "X-Ashanum-Webhook-Secret"
	WebhookSecret = "fake-webhook-secret-for-tests-0123456789"
)

// Areas is the fake area catalog (kecamatan level, single search).
var Areas = []Area{
	{ID: "IDNP9IDNC22IDND318IDZ40132", Name: "Coblong, Bandung, Jawa Barat. 40132", Province: "Jawa Barat", City: "Bandung", District: "Coblong", PostalCode: 40132},
	{ID: "IDNP9IDNC22IDND313IDZ40115", Name: "Bandung Wetan, Bandung, Jawa Barat. 40115", Province: "Jawa Barat", City: "Bandung", District: "Bandung Wetan", PostalCode: 40115},
	{ID: "IDNP6IDNC148IDND836IDZ12430", Name: "Cilandak, Jakarta Selatan, DKI Jakarta. 12430", Province: "DKI Jakarta", City: "Jakarta Selatan", District: "Cilandak", PostalCode: 12430},
	{ID: "IDNP11IDNC256IDND3133IDZ60271", Name: "Tegalsari, Surabaya, Jawa Timur. 60271", Province: "Jawa Timur", City: "Surabaya", District: "Tegalsari", PostalCode: 60271},
}

type Area struct {
	ID         string
	Name       string
	Province   string
	City       string
	District   string
	PostalCode int
}

// services is the fake courier catalog: base price and price per kg.
var services = map[string][]service{
	"jne":      {{"reg", "Reguler", "Layanan reguler", "2 - 3", 9000, 9000}, {"yes", "Yakin Esok Sampai", "Sampai besok", "1 - 1", 18000, 15000}},
	"sicepat":  {{"reg", "Reguler", "Layanan reguler", "2 - 3", 8000, 8000}},
	"jnt":      {{"ez", "EZ", "Layanan reguler", "2 - 3", 8500, 8500}},
	"anteraja": {{"reg", "Regular", "Layanan reguler", "3 - 4", 7500, 7500}},
}

type service struct {
	Code, Name, Description, Range string
	Base, PerKg                    int64
}

// Order is the fake state of one booked order.
type Order struct {
	ID         string
	Reference  string
	TrackingID string
	Waybill    string
	Courier    string
	Service    string
	Status     string
	History    []History
}

type History struct {
	Status string
	Note   string
	At     time.Time
}

// Server fakes Biteship.
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	orders      map[string]*Order // by id
	byReference map[string]string
	byTracking  map[string]string
	calls       []string
	badAuth     int
	failNext    map[string]int // path prefix → number of 503 answers
	rejectNext  map[string]int // path prefix → number of 400 answers
	malformed   map[string]int // path prefix → number of broken JSON answers
	delay       time.Duration
	lastRates   map[string]any
	lastOrder   map[string]any
	nextID      int
	noCourier   bool
}

func New(t testing.TB) *Server {
	s := &Server{orders: map[string]*Order{}, byReference: map[string]string{}, byTracking: map[string]string{},
		failNext: map[string]int{}, rejectNext: map[string]int{}, malformed: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)

	if r.Header.Get("Authorization") != APIKey && r.Header.Get("Authorization") != "Bearer "+APIKey {
		s.badAuth++
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "Authorization failed", "code": 40000001})
		return
	}
	if take(s.failNext, r.URL.Path) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "Service unavailable", "code": 50000000})
		return
	}
	if take(s.rejectNext, r.URL.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Invalid request", "code": 40001002})
		return
	}
	if take(s.malformed, r.URL.Path) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success": true, "pricing": [`))
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/maps/areas":
		s.areas(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/rates/couriers":
		s.rates(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/orders":
		s.createOrder(w, r)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "orders":
		o, ok := s.orders[parts[2]]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Order not found", "code": 40002001})
			return
		}
		writeJSON(w, http.StatusOK, orderJSON(o))
	case r.Method == http.MethodPost && len(parts) == 4 && parts[1] == "orders" && parts[3] == "cancel":
		s.cancel(w, parts[2])
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "trackings":
		id, ok := s.byTracking[parts[2]]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Tracking not found", "code": 40003001})
			return
		}
		writeJSON(w, http.StatusOK, trackingJSON(s.orders[id]))
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Not found", "code": 40400000})
	}
}

func (s *Server) areas(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("input")))
	if r.URL.Query().Get("countries") != "ID" || q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Invalid input", "code": 40001002})
		return
	}
	out := []map[string]any{}
	for _, a := range Areas {
		if strings.Contains(strings.ToLower(a.Name), q) {
			out = append(out, map[string]any{
				"id": a.ID, "name": a.Name, "country_name": "Indonesia", "country_code": "ID",
				"administrative_division_level_1_name": a.Province, "administrative_division_level_1_type": "province",
				"administrative_division_level_2_name": a.City, "administrative_division_level_2_type": "city",
				"administrative_division_level_3_name": a.District, "administrative_division_level_3_type": "district",
				"postal_code": a.PostalCode,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "areas": out})
}

func (s *Server) rates(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Invalid JSON", "code": 40001002})
		return
	}
	s.lastRates = body
	_, oa := body["origin_area_id"]
	_, op := body["origin_postal_code"]
	_, da := body["destination_area_id"]
	_, dp := body["destination_postal_code"]
	if (!oa && !op) || (!da && !dp) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Origin and destination are required", "code": 40001002})
		return
	}
	grams := totalWeight(body)
	if grams <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Items weight is required", "code": 40001002})
		return
	}
	if s.noCourier {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "No courier available for requested route", "code": 40001010})
		return
	}
	kg := int64((grams + 999) / 1000)
	couriers, _ := body["couriers"].(string)
	pricing := []map[string]any{}
	for _, c := range strings.Split(couriers, ",") {
		for _, sv := range services[strings.TrimSpace(c)] {
			price := sv.Base + sv.PerKg*(kg-1)
			pricing = append(pricing, map[string]any{
				"available_collection_method": []string{"pickup"}, "available_for_cash_on_delivery": false,
				"company": c, "courier_name": strings.ToUpper(c), "courier_code": c,
				"courier_service_name": sv.Name, "courier_service_code": sv.Code, "currency": "IDR",
				"description": sv.Description, "duration": sv.Range + " days",
				"shipment_duration_range": sv.Range, "shipment_duration_unit": "days",
				"service_type": "standard", "shipping_type": "parcel",
				"shipping_fee": price, "shipping_fee_discount": 0, "shipping_fee_surcharge": 0, "insurance_fee": 0,
				"cash_on_delivery_fee": 0, "price": price, "type": sv.Code,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "object": "courier_pricing", "message": "Success to retrieve courier pricing",
		"code": 20001007, "origin": map[string]any{}, "destination": map[string]any{}, "pricing": pricing})
}

func totalWeight(body map[string]any) int {
	list, _ := body["items"].([]any)
	n := 0
	for _, it := range list {
		m, _ := it.(map[string]any)
		wt, _ := m["weight"].(float64)
		q, _ := m["quantity"].(float64)
		n += int(wt) * int(q)
	}
	return n
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Invalid JSON", "code": 40002002})
		return
	}
	s.lastOrder = body
	ref, _ := body["reference_id"].(string)
	if id, used := s.byReference[ref]; used && ref != "" {
		o := s.orders[id]
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "code": 40002060,
			"error":   "Reference id has already been used before",
			"details": map[string]any{"order_id": o.ID, "waybill_id": o.Waybill, "reference_id": ref}})
		return
	}
	for _, f := range []string{"origin_contact_name", "origin_contact_phone", "origin_address", "destination_contact_name",
		"destination_contact_phone", "destination_address", "courier_company", "courier_type", "delivery_type"} {
		if v, _ := body[f].(string); v == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": f + " is required", "code": 40002002})
			return
		}
	}
	if totalWeight(body) <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Items weight is required", "code": 40002002})
		return
	}
	s.nextID++
	o := &Order{ID: fmt.Sprintf("fake-ord-%04d", s.nextID), Reference: ref, TrackingID: fmt.Sprintf("fake-trk-%04d", s.nextID),
		Waybill: fmt.Sprintf("WB%010d", s.nextID), Status: "confirmed"}
	o.Courier, _ = body["courier_company"].(string)
	o.Service, _ = body["courier_type"].(string)
	o.History = []History{{Status: "confirmed", Note: "Order has been confirmed. Locating nearest driver to pick up.", At: time.Now().UTC()}}
	s.orders[o.ID] = o
	s.byTracking[o.TrackingID] = o.ID
	if ref != "" {
		s.byReference[ref] = o.ID
	}
	writeJSON(w, http.StatusOK, orderJSON(o))
}

func (s *Server) cancel(w http.ResponseWriter, id string) {
	o, ok := s.orders[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Order not found", "code": 40002001})
		return
	}
	switch o.Status {
	case "confirmed", "allocated", "picking_up":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Order can not be cancelled", "code": 40002036})
		return
	}
	o.Status = "cancelled"
	o.History = append(o.History, History{Status: "cancelled", Note: "Order is cancelled.", At: time.Now().UTC()})
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Order successfully cancelled", "object": "order",
		"id": o.ID, "status": "cancelled", "cancellation_reason_code": "others"})
}

func orderJSON(o *Order) map[string]any {
	return map[string]any{"success": true, "message": "Order successfully created", "object": "order", "id": o.ID,
		"reference_id": o.Reference, "status": o.Status, "price": 9000, "currency": "IDR",
		"courier": map[string]any{"tracking_id": o.TrackingID, "waybill_id": o.Waybill, "company": o.Courier, "type": o.Service,
			"link": "https://track.example.test/" + o.Waybill}}
}

func trackingJSON(o *Order) map[string]any {
	hist := make([]map[string]any, len(o.History))
	for i, h := range o.History {
		hist[i] = map[string]any{"note": h.Note, "status": h.Status, "updated_at": h.At.Format(time.RFC3339), "service_type": o.Service}
	}
	return map[string]any{"success": true, "messsage": "Successfully get tracking info", "object": "tracking", "id": o.TrackingID,
		"waybill_id": o.Waybill, "courier": map[string]any{"company": o.Courier}, "origin": map[string]any{}, "destination": map[string]any{},
		"history": hist, "link": "https://track.example.test/" + o.Waybill, "order_id": o.ID, "status": o.Status}
}

// ---------------------------------------------------------------- controls

// Advance sets an order's status and appends it to its history at time at.
func (s *Server) Advance(orderID, status, note string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.orders[orderID]
	o.Status = status
	o.History = append(o.History, History{Status: status, Note: note, At: at.UTC()})
}

// Order returns a copy of a booked order (nil when unknown).
func (s *Server) Order(id string) *Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.orders[id]; ok {
		c := *o
		return &c
	}
	return nil
}

// OrderCount is the number of distinct orders booked.
func (s *Server) OrderCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.orders)
}

// FailNext makes the next n requests whose path starts with prefix fail
// with 503.
func (s *Server) FailNext(prefix string, n int) { s.set(s.failNext, prefix, n) }

// RejectNext makes the next n requests whose path starts with prefix fail
// with 400.
func (s *Server) RejectNext(prefix string, n int) { s.set(s.rejectNext, prefix, n) }

// MalformedNext makes the next n requests whose path starts with prefix
// answer 200 with broken JSON.
func (s *Server) MalformedNext(prefix string, n int) { s.set(s.malformed, prefix, n) }

// SetDelay delays every answer (timeouts).
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// SetNoCourier makes rates answer "no courier available".
func (s *Server) SetNoCourier(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noCourier = v
}

func (s *Server) set(m map[string]int, prefix string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m[prefix] = n
}

// Calls counts requests whose "METHOD /path" starts with prefix.
func (s *Server) Calls(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// BadAuth is the number of requests with a wrong key.
func (s *Server) BadAuth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badAuth
}

// LastRates / LastOrder are the last request bodies (copies).
func (s *Server) LastRates() map[string]any { return s.copyBody(s.lastRates) }
func (s *Server) LastOrder() map[string]any { return s.copyBody(s.lastOrder) }

func (s *Server) copyBody(m map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

// Webhook builds an order.status notification body as Biteship sends it.
func Webhook(event, orderID, status string) []byte {
	b, _ := json.Marshal(map[string]any{"event": event, "courier_tracking_id": "", "courier_waybill_id": "",
		"courier_company": "jne", "courier_type": "reg", "order_id": orderID, "order_price": 9000, "status": status})
	return b
}

// WebhookWith builds a notification for a booked order (tracking id and
// waybill filled in).
func (s *Server) WebhookWith(event, orderID, status string) []byte {
	s.mu.Lock()
	o := s.orders[orderID]
	s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"event": event, "courier_tracking_id": o.TrackingID, "courier_waybill_id": o.Waybill,
		"courier_company": o.Courier, "courier_type": o.Service, "courier_link": "https://track.example.test/" + o.Waybill,
		"order_id": o.ID, "order_price": 9000, "status": status})
	return b
}

func take(m map[string]int, path string) bool {
	for prefix, n := range m {
		if n > 0 && strings.HasPrefix(path, prefix) {
			m[prefix] = n - 1
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
