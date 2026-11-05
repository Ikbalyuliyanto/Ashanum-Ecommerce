package biteship_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"ecommerce/internal/shipping"
	"ecommerce/internal/shipping/biteship"
	"ecommerce/internal/shipping/biteship/biteshiptest"
)

func newClient(t *testing.T, timeout time.Duration) (*biteship.Client, *biteshiptest.Server) {
	t.Helper()
	fake := biteshiptest.New(t)
	hc := biteship.NewHTTPClient()
	hc.Timeout = timeout
	c, err := biteship.New(biteship.Config{Env: biteship.EnvTest, APIKey: biteshiptest.APIKey, BaseURL: fake.URL,
		WebhookHeader: biteshiptest.WebhookHeader, WebhookSecret: biteshiptest.WebhookSecret, HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	return c, fake
}

var ctx = context.Background()

func pkg(grams, qty int) shipping.Package {
	return shipping.Package{Items: []shipping.Item{{Name: "Kaos", SKU: "K-1", Value: 100000, WeightGrams: grams, Quantity: qty}}}
}

func TestEnvironmentGuards(t *testing.T) {
	cases := []struct {
		name string
		cfg  biteship.Config
	}{
		{"live without SHIPPING_LIVE_ENABLED", biteship.Config{Env: biteship.EnvLive, APIKey: "biteship_live.x"}},
		{"live with a test key", biteship.Config{Env: biteship.EnvLive, APIKey: "biteship_test.x", LiveEnabled: true}},
		{"test with a live key", biteship.Config{Env: biteship.EnvTest, APIKey: "biteship_live.x"}},
		{"unknown environment", biteship.Config{Env: "prod", APIKey: "biteship_test.x"}},
	}
	for _, c := range cases {
		if _, err := biteship.New(c.cfg); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if strings.Contains(err.Error(), "biteship_live.x") || strings.Contains(err.Error(), "biteship_test.x") {
			t.Errorf("%s: error leaks the key: %v", c.name, err)
		}
	}
	if _, err := biteship.New(biteship.Config{Env: biteship.EnvLive, APIKey: "biteship_live.x", LiveEnabled: true}); err != nil {
		t.Errorf("explicitly enabled live: %v", err)
	}
}

func TestSearchAreas(t *testing.T) {
	c, fake := newClient(t, 2*time.Second)
	areas, err := c.SearchAreas(ctx, "Bandung")
	if err != nil {
		t.Fatal(err)
	}
	if len(areas) != 2 {
		t.Fatalf("areas: %+v", areas)
	}
	a := areas[0]
	if a.Provider != "biteship" || a.ID != biteshiptest.Areas[0].ID || a.Label != "Coblong, Bandung, Jawa Barat. 40132" ||
		a.Province != "Jawa Barat" || a.City != "Bandung" || a.District != "Coblong" || a.PostalCode != "40132" {
		t.Errorf("area mapping: %+v", a)
	}
	if none, err := c.SearchAreas(ctx, "Tidak Ada"); err != nil || len(none) != 0 {
		t.Errorf("no match: %v %v", none, err)
	}
	if fake.BadAuth() != 0 {
		t.Error("the key must be sent")
	}
}

func TestRates(t *testing.T) {
	c, fake := newClient(t, 2*time.Second)
	req := shipping.RateRequest{Origin: shipping.Location{AreaID: "ORIGIN-AREA"}, Destination: shipping.Location{AreaID: "DEST-AREA", PostalCode: "40132"},
		Package: pkg(700, 2), Couriers: []string{"jne", "sicepat"}}
	rates, err := c.Rates(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// 1.4 kg → 2 kg: JNE REG 9000+9000, JNE YES 18000+15000, SiCepat REG 8000+8000.
	got := map[string]shipping.Rate{}
	for _, r := range rates {
		got[r.Key()] = r
	}
	if r := got["jne:reg"]; r.Price != 18000 || r.CourierName != "JNE" || r.ServiceName != "Reguler" || r.EstimatedDays != "2-3" || r.Provider != "biteship" {
		t.Errorf("jne reg: %+v", r)
	}
	if r := got["jne:yes"]; r.Price != 33000 || r.EstimatedDays != "1" {
		t.Errorf("jne yes: %+v", r)
	}
	if r := got["sicepat:reg"]; r.Price != 16000 {
		t.Errorf("sicepat: %+v", r)
	}
	// The request carries server data only: area ids, item weights, couriers.
	body := fake.LastRates()
	items := body["items"].([]any)
	if body["origin_area_id"] != "ORIGIN-AREA" || body["destination_area_id"] != "DEST-AREA" || body["couriers"] != "jne,sicepat" ||
		len(items) != 1 || items[0].(map[string]any)["weight"] != float64(700) || items[0].(map[string]any)["quantity"] != float64(2) {
		t.Errorf("rate request: %v", body)
	}
	if _, ok := body["destination_postal_code"]; ok {
		t.Error("with an area id the postal code is not sent for rates")
	}

	fake.SetNoCourier(true)
	if _, err := c.Rates(ctx, req); !errors.Is(err, shipping.ErrNoCourier) {
		t.Errorf("no courier: %v", err)
	}
}

func TestProviderFailures(t *testing.T) {
	c, fake := newClient(t, 300*time.Millisecond)
	req := shipping.RateRequest{Origin: shipping.Location{PostalCode: "40111"}, Destination: shipping.Location{PostalCode: "40132"},
		Package: pkg(500, 1), Couriers: []string{"jne"}}

	fake.FailNext("/v1/rates", 1)
	if _, err := c.Rates(ctx, req); !errors.Is(err, shipping.ErrUnavailable) {
		t.Errorf("5xx: %v", err)
	}
	fake.RejectNext("/v1/rates", 1)
	if _, err := c.Rates(ctx, req); !errors.Is(err, shipping.ErrRejected) || errors.Is(err, shipping.ErrUnavailable) {
		t.Errorf("4xx: %v", err)
	}
	fake.MalformedNext("/v1/rates", 1)
	if _, err := c.Rates(ctx, req); !errors.Is(err, shipping.ErrUnavailable) {
		t.Errorf("malformed JSON: %v", err)
	}
	fake.SetDelay(time.Second)
	start := time.Now()
	_, err := c.Rates(ctx, req)
	if !errors.Is(err, shipping.ErrUnavailable) || time.Since(start) > 900*time.Millisecond {
		t.Errorf("timeout: %v after %s", err, time.Since(start))
	}
	fake.SetDelay(0)

	bad, err := biteship.New(biteship.Config{Env: biteship.EnvTest, APIKey: "biteship_test.WRONG-KEY", BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bad.Rates(ctx, req)
	if !errors.Is(err, shipping.ErrRejected) || strings.Contains(err.Error(), "WRONG-KEY") {
		t.Errorf("bad key: %v", err)
	}
	if _, err := c.Rates(ctx, req); err != nil {
		t.Errorf("recovers: %v", err)
	}
}

func booking(ref string) shipping.Booking {
	return shipping.Booking{Reference: ref,
		Origin:      shipping.Origin{Name: "Toko Ashanum", Phone: "+6281200000001", Address: "Jl. Asia Afrika No. 8, Bandung", PostalCode: "40111", AreaID: "ORIGIN-AREA"},
		Destination: shipping.Contact{Name: "Siti", Phone: "+6281298765432", Address: "Jl. Juanda 100, Coblong, Bandung", PostalCode: "40132", AreaID: "DEST-AREA"},
		CourierCode: "jne", ServiceCode: "reg", Package: pkg(300, 1), Note: "Pesanan ASH-1"}
}

func TestCreateShipmentIsIdempotentByReference(t *testing.T) {
	c, fake := newClient(t, 2*time.Second)
	b, err := c.CreateShipment(ctx, booking("ASH-1-S1"))
	if err != nil {
		t.Fatal(err)
	}
	if b.ShipmentID == "" || b.TrackingID == "" || b.Waybill == "" || b.Status != shipping.StatusCreated || b.Existing ||
		!strings.HasPrefix(b.TrackingURL, "https://") {
		t.Errorf("booked: %+v", b)
	}
	sent := fake.LastOrder()
	if sent["reference_id"] != "ASH-1-S1" || sent["courier_company"] != "jne" || sent["courier_type"] != "reg" ||
		sent["destination_contact_phone"] != "081298765432" || sent["origin_area_id"] != "ORIGIN-AREA" || sent["delivery_type"] != "now" {
		t.Errorf("order request: %v", sent)
	}

	again, err := c.CreateShipment(ctx, booking("ASH-1-S1"))
	if err != nil || !again.Existing || again.ShipmentID != b.ShipmentID || again.Waybill != b.Waybill {
		t.Errorf("same reference: %+v %v", again, err)
	}
	if fake.OrderCount() != 1 {
		t.Errorf("provider orders = %d, want 1", fake.OrderCount())
	}
}

func TestTrackingAndCancel(t *testing.T) {
	c, fake := newClient(t, 2*time.Second)
	b, err := c.CreateShipment(ctx, booking("ASH-2-S1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CancelShipment(ctx, b.ShipmentID, "salah kurir"); err != nil {
		t.Fatalf("cancel before pickup: %v", err)
	}

	b2, _ := c.CreateShipment(ctx, booking("ASH-3-S1"))
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	fake.Advance(b2.ShipmentID, "allocated", "Courier has been allocated.", t0)
	fake.Advance(b2.ShipmentID, "picking_up", "Courier is on the way to pick up item.", t0.Add(10*time.Minute))
	fake.Advance(b2.ShipmentID, "picked", "Item has been picked.", t0.Add(30*time.Minute))
	fake.Advance(b2.ShipmentID, "dropping_off", "Item is on the way to customer.", t0.Add(time.Hour))
	tr, err := c.Tracking(ctx, b2.ShipmentID, b2.TrackingID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != shipping.StatusOutForDelivery || tr.RawStatus != "dropping_off" || tr.Waybill != b2.Waybill || len(tr.Events) != 5 {
		t.Fatalf("tracking: %+v", tr)
	}
	if e := tr.Events[3]; e.Status != shipping.StatusPickedUp || e.RawStatus != "picked" || !e.Time.Equal(t0.Add(30*time.Minute)) || e.Description == "" {
		t.Errorf("event mapping: %+v", e)
	}
	// Without a tracking id the order is read first.
	if tr2, err := c.Tracking(ctx, b2.ShipmentID, ""); err != nil || tr2.Status != shipping.StatusOutForDelivery {
		t.Errorf("tracking by shipment id: %+v %v", tr2, err)
	}
	if err := c.CancelShipment(ctx, b2.ShipmentID, "terlambat"); !errors.Is(err, shipping.ErrRejected) {
		t.Errorf("cancel after pickup: %v", err)
	}
}

func TestMapStatus(t *testing.T) {
	cases := map[string]shipping.Status{
		"confirmed": shipping.StatusCreated, "allocated": shipping.StatusCreated, "picking_up": shipping.StatusCreated,
		"pickingUp": shipping.StatusCreated, "picked": shipping.StatusPickedUp, "inTransit": shipping.StatusInTransit,
		"in_transit": shipping.StatusInTransit, "dropping_off": shipping.StatusOutForDelivery, "droppingOff": shipping.StatusOutForDelivery,
		"delivered": shipping.StatusDelivered, "on_hold": shipping.StatusException, "onHold": shipping.StatusException,
		"rejected": shipping.StatusException, "returnInTransit": shipping.StatusException, "disposed": shipping.StatusException,
		"returned": shipping.StatusReturned, "cancelled": shipping.StatusCancelled, "courierNotFound": shipping.StatusCancelled,
	}
	for raw, want := range cases {
		if got, ok := biteship.MapStatus(raw); !ok || got != want {
			t.Errorf("%s → %s (%v), want %s", raw, got, ok, want)
		}
	}
	if _, ok := biteship.MapStatus("teleported"); ok {
		t.Error("an unknown status must not map")
	}
}

func TestParseWebhook(t *testing.T) {
	c, _ := newClient(t, time.Second)
	signed := http.Header{biteshiptest.WebhookHeader: {biteshiptest.WebhookSecret}}
	body := biteshiptest.Webhook("order.status", "ord-1", "picked")

	ev, err := c.ParseWebhook(signed, body)
	if err != nil || ev.Kind != shipping.WebhookStatus || ev.ShipmentID != "ord-1" || ev.Status != shipping.StatusPickedUp || ev.RawStatus != "picked" {
		t.Errorf("valid: %+v %v", ev, err)
	}
	for name, h := range map[string]http.Header{
		"no header":    {},
		"wrong secret": {biteshiptest.WebhookHeader: {"guess"}},
		"other header": {"X-Other": {biteshiptest.WebhookSecret}},
	} {
		if _, err := c.ParseWebhook(h, body); !errors.Is(err, shipping.ErrInvalidSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ev, err := c.ParseWebhook(http.Header{}, []byte("  ")); err != nil || ev.Kind != shipping.WebhookPing {
		t.Errorf("installation ping: %+v %v", ev, err)
	}
	if _, err := c.ParseWebhook(signed, []byte(`{"event": "order.status", "order_id": `)); !errors.Is(err, shipping.ErrMalformed) {
		t.Errorf("malformed: %v", err)
	}
	if _, err := c.ParseWebhook(signed, []byte(`{"event": "order.status", "status": "picked"}`)); !errors.Is(err, shipping.ErrMalformed) {
		t.Errorf("without order id: %v", err)
	}
	price, _ := json.Marshal(map[string]any{"event": "order.price", "order_id": "ord-1", "price": 1})
	if ev, err := c.ParseWebhook(signed, price); err != nil || ev.Kind != shipping.WebhookOther {
		t.Errorf("price update is ignored: %+v %v", ev, err)
	}
	unconfigured, _ := biteship.New(biteship.Config{Env: biteship.EnvTest, APIKey: biteshiptest.APIKey, BaseURL: "http://127.0.0.1:1"})
	if _, err := unconfigured.ParseWebhook(signed, body); !errors.Is(err, shipping.ErrInvalidSignature) {
		t.Errorf("no secret configured must accept nothing: %v", err)
	}
	if bytes.Contains(body, []byte(biteshiptest.APIKey)) {
		t.Error("fixture sanity")
	}
}
