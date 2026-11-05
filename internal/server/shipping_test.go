package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ecommerce/internal/shipping/biteship/biteshiptest"
	"ecommerce/internal/testutil"
)

// shippingWebhook posts a notification like Biteship does: no cookie, no
// Origin, the configured secret header (unless secret is "").
func shippingWebhook(t *testing.T, base, secret string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/webhooks/shipping/biteship", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set(biteshiptest.WebhookHeader, secret)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// TestShipmentBookingAndTrackingFlow: paid order → admin books the
// buyer's courier → webhook updates → the buyer sees status and timeline.
func TestShipmentBookingAndTrackingFlow(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	fake := shipFor(t, db).Fake
	buyer := newBrowser(t, base)
	buyer.register("siti@example.com")
	buyer.addAddress()
	number := buyNow(t, buyer, db)
	orderPath := "/admin/orders/" + number
	admin := newAdmin(t, base, db)

	// Unpaid: no booking (the button is not offered, the action refuses).
	if res := admin.post(orderPath+"/shipment/book", nil); res.status != http.StatusSeeOther || fake.OrderCount() != 0 {
		t.Fatalf("book unpaid: %d, provider orders %d", res.status, fake.OrderCount())
	}
	expect(t, buyer.get("/account/orders/"+number), http.StatusOK, "JNE Reguler", "Kurir dipanggil setelah pembayaran dikonfirmasi")

	testutil.PayOrder(t, db, number)
	expect(t, admin.post(orderPath+"/confirm", nil), http.StatusSeeOther)
	page := admin.get(orderPath)
	expect(t, page, http.StatusOK, "Buat Pengiriman", "Pembeli memilih <strong>JNE Reguler</strong>", "Kirim manual (cadangan)", "Pilihan pembeli (checkout)")

	// Double submit: one provider shipment.
	for range 2 {
		if res := admin.post(orderPath+"/shipment/book", nil); res.status != http.StatusSeeOther {
			t.Fatalf("book: %d", res.status)
		}
	}
	if fake.OrderCount() != 1 {
		t.Fatalf("provider orders = %d", fake.OrderCount())
	}
	providerID := queryString(t, db, "SELECT provider_shipment_id FROM shipments")
	awb := fake.Order(providerID).Waybill
	expect(t, admin.get(orderPath), http.StatusOK, "Menunggu Penjemputan", awb, "Perbarui Status", "Batalkan pengiriman")
	expect(t, buyer.get("/account/orders/"+number), http.StatusOK, "Menunggu Penjemputan", awb, "menunggu dijemput kurir")

	// Webhook: picked, then delivered (order stays SHIPPED until the shop completes it).
	if code, body := shippingWebhook(t, base, biteshiptest.WebhookSecret, fake.WebhookWith("order.status", providerID, "picked")); code != 200 || !strings.Contains(body, "applied") {
		t.Fatalf("webhook: %d %s", code, body)
	}
	expect(t, buyer.get("/account/orders/"+number), http.StatusOK, "Dikirim", "Dijemput Kurir", "Riwayat pengiriman", "Lacak di situs kurir")
	shippingWebhook(t, base, biteshiptest.WebhookSecret, fake.WebhookWith("order.status", providerID, "delivered"))
	if s := queryString(t, db, "SELECT status FROM orders WHERE order_number = $1", number); s != "SHIPPED" {
		t.Errorf("delivered order status = %s, want SHIPPED", s)
	}
	expect(t, buyer.get("/account/orders/"+number), http.StatusOK, "Terkirim", "Paket sudah diterima")
	expect(t, admin.get(orderPath), http.StatusOK, "Kurir melaporkan paket <strong>terkirim</strong>", "Selesaikan Pesanan")

	// Another buyer sees nothing of it.
	other := newBrowser(t, base)
	other.register("other@example.com")
	expect(t, other.get("/account/orders/"+number), http.StatusNotFound)
}

func TestShippingWebhookEndpoint(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	fake := shipFor(t, db).Fake
	buyer := newBrowser(t, base)
	buyer.register("siti@example.com")
	buyer.addAddress()
	number := buyNow(t, buyer, db)
	testutil.PayOrder(t, db, number)
	admin := newAdmin(t, base, db)
	admin.post("/admin/orders/"+number+"/confirm", nil)
	admin.post("/admin/orders/"+number+"/shipment/book", nil)
	id := queryString(t, db, "SELECT provider_shipment_id FROM shipments")
	events := func() int64 { return queryID(t, db, "SELECT count(*) FROM shipment_events") }

	if code, _ := shippingWebhook(t, base, "", nil); code != 200 {
		t.Errorf("installation ping: %d", code)
	}
	if code, _ := shippingWebhook(t, base, "", fake.WebhookWith("order.status", id, "picked")); code != 401 {
		t.Errorf("unsigned: %d", code)
	}
	if code, _ := shippingWebhook(t, base, "tebakan-rahasia-yang-salah-123", fake.WebhookWith("order.status", id, "picked")); code != 401 {
		t.Errorf("wrong secret: %d", code)
	}
	if code, _ := shippingWebhook(t, base, biteshiptest.WebhookSecret, []byte(`{"event":"order.status","order_id":`)); code != 400 {
		t.Errorf("malformed: %d", code)
	}
	big := bytes.Repeat([]byte("x"), 70<<10)
	if code, _ := shippingWebhook(t, base, biteshiptest.WebhookSecret, big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", code)
	}
	if code, body := shippingWebhook(t, base, biteshiptest.WebhookSecret, biteshiptest.Webhook("order.status", "unknown-id", "delivered")); code != 200 || !strings.Contains(body, "ignored") {
		t.Errorf("unknown shipment: %d %s", code, body)
	}
	if queryString(t, db, "SELECT status FROM shipments") != "CREATED" || events() != 1 {
		t.Fatal("refused notifications must change nothing")
	}
	for range 3 {
		if code, _ := shippingWebhook(t, base, biteshiptest.WebhookSecret, fake.WebhookWith("order.status", id, "picked")); code != 200 {
			t.Fatalf("valid: %d", code)
		}
	}
	if events() != 2 || queryString(t, db, "SELECT status FROM shipments") != "PICKED_UP" {
		t.Errorf("duplicates: %d events, %s", events(), queryString(t, db, "SELECT status FROM shipments"))
	}
	// The waybill can arrive later.
	exec(t, db, "UPDATE shipments SET tracking_number = ''")
	shippingWebhook(t, base, biteshiptest.WebhookSecret, fake.WebhookWith("order.waybill_id", id, "picked"))
	if queryString(t, db, "SELECT tracking_number FROM shipments") != fake.Order(id).Waybill {
		t.Error("waybill webhook")
	}
}

func TestAPIShippingAndTracking(t *testing.T) {
	base, db := newAPIApp(t, nil)
	seedCatalog(t, db)
	fake := shipFor(t, db).Fake
	a, b := newAPI(t, base), newAPI(t, base)
	want(t, a.get("/shipping/areas?q=Coblong"), http.StatusUnauthorized, "")
	a.signIn("a@example.com")
	b.signIn("b@example.com")

	want(t, a.get("/shipping/areas?q=ab"), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	res := a.get("/shipping/areas?q=Coblong")
	area := res.list()[0].(map[string]any)
	if area["label"] != "Coblong, Bandung, Jawa Barat. 40132" || area["postal_code"] != "40132" || area["provider"] != "biteship" {
		t.Errorf("areas: %s", res.raw)
	}
	addr := num(a.post("/me/addresses", map[string]any{"label": "Rumah", "recipient_name": "Siti", "phone": "081298765432",
		"village": "Dago", "street_address": "Jl. Ir. H. Juanda No. 100", "shipping_area_id": area["id"]}).data()["id"])
	noArea := num(a.post("/me/addresses", map[string]any{"label": "Lama", "recipient_name": "Siti", "phone": "081298765432",
		"province": "Jawa Barat", "city": "Bandung", "district": "Coblong", "village": "Dago", "postal_code": "40135",
		"street_address": "Jl. Ir. H. Juanda No. 101"}).data()["id"])
	want(t, a.post("/me/addresses", map[string]any{"label": "Palsu", "recipient_name": "Siti", "phone": "081298765432",
		"village": "Dago", "street_address": "Jl. Ir. H. Juanda No. 102", "shipping_area_id": "IDNP-INVENTED"}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	buy := map[string]any{"variant_id": v, "quantity": 1}

	res = a.post("/shipping/rates", map[string]any{"buy_now": buy, "address_id": addr})
	want(t, res, http.StatusOK, "")
	if sel := res.data()["selected"].(map[string]any); sel["price"] != float64(9000) || sel["estimate_label"] != "2–3 hari" || len(res.data()["options"].([]any)) != 2 {
		t.Errorf("rates: %s", res.raw)
	}
	want(t, a.post("/shipping/rates", map[string]any{"buy_now": buy, "address_id": noArea}), http.StatusUnprocessableEntity, "SHIPPING_AREA_REQUIRED")
	want(t, b.post("/shipping/rates", map[string]any{"buy_now": buy, "address_id": addr}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	fake.FailNext("/v1/rates", 1)
	want(t, a.post("/shipping/rates", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 3}, "address_id": addr}),
		http.StatusServiceUnavailable, "SHIPPING_PROVIDER_UNAVAILABLE")

	// Expired quote: 409 SHIPPING_CHANGED, no order.
	q, total := a.quote(map[string]any{"buy_now": buy, "address_id": addr})
	exec(t, db, "UPDATE shipping_quotes SET expires_at = now() - interval '1 minute'")
	want(t, a.post("/orders", map[string]any{"buy_now": buy, "address_id": addr, "shipping_quote_id": q, "idempotency_key": "ship-001",
		"expected_total": total}), http.StatusConflict, "SHIPPING_CHANGED")
	// The preview with the old id refreshes the same option (new expiry).
	p := a.post("/checkout/preview", map[string]any{"buy_now": buy, "address_id": addr, "shipping_quote_id": q}).data()
	if sel := p["shipping"].(map[string]any)["selected"].(map[string]any); sel["quote_id"] != q || p["total"] != total {
		t.Errorf("refreshed quote: %v", p["shipping"])
	}
	res = a.post("/orders", map[string]any{"buy_now": buy, "address_id": addr, "shipping_quote_id": q, "idempotency_key": "ship-002", "expected_total": total})
	want(t, res, http.StatusCreated, "")
	number := res.data()["order_number"].(string)
	if sh := res.data()["shipping"].(map[string]any); sh["cost"] != float64(9000) || sh["courier_code"] != "jne" || sh["service_name"] != "Reguler" ||
		res.data()["shipment"] != nil {
		t.Errorf("order shipping: %s", res.raw)
	}

	tr := a.get("/orders/" + number + "/tracking")
	want(t, tr, http.StatusOK, "")
	if tr.data()["shipment"] != nil || len(tr.data()["events"].([]any)) != 0 || tr.data()["shipping"].(map[string]any)["courier_name"] != "JNE" {
		t.Errorf("tracking before shipping: %s", tr.raw)
	}
	want(t, b.get("/orders/"+number+"/tracking"), http.StatusNotFound, "NOT_FOUND")

	// Paid, booked and picked up (service level): the API reads it from the database.
	testutil.PayOrder(t, db, number)
	admin := newAdmin(t, base, db)
	admin.post("/admin/orders/"+number+"/confirm", nil)
	admin.post("/admin/orders/"+number+"/shipment/book", nil)
	id := queryString(t, db, "SELECT provider_shipment_id FROM shipments")
	fake.Advance(id, "picked", "Paket dijemput kurir.", time.Now())
	admin.post("/admin/orders/"+number+"/shipment/sync", nil)
	calls := fake.Calls("")
	tr = a.get("/orders/" + number + "/tracking")
	ship := tr.data()["shipment"].(map[string]any)
	evs := tr.data()["events"].([]any)
	if tr.data()["order_status"] != "SHIPPED" || ship["status"] != "PICKED_UP" || ship["live_tracking"] != true || ship["tracking_number"] == nil ||
		len(evs) != 2 || evs[0].(map[string]any)["status"] != "PICKED_UP" || fake.Calls("") != calls {
		t.Errorf("tracking: %s (provider calls %d → %d)", tr.raw, calls, fake.Calls(""))
	}
	if !rfc3339WIB.MatchString(evs[0].(map[string]any)["time"].(string)) {
		t.Errorf("event time: %v", evs[0])
	}
}
