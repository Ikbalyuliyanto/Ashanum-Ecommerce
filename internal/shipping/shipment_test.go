package shipping_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/fulfillment"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/shipping"
	"ecommerce/internal/shipping/biteship/biteshiptest"
	"ecommerce/internal/shipping/shippingtest"
	"ecommerce/internal/testutil"
)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	db      *pgxpool.Pool
	ship    *shippingtest.Env
	svc     *shipping.Service
	co      *checkout.Service
	ful     *fulfillment.Service
	admin   int64
	buyer   int64
	addr    int64
	variant int64
}

func newFixture(t *testing.T) *fixture {
	db := testutil.DB(t)
	ctx := context.Background()
	sh := shippingtest.New(t, db)
	addrs := address.NewService(address.NewRepository(db), sh.Service)
	pay := payment.NewService(payment.NewRepository(db), nil, payment.Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	f := &fixture{t: t, ctx: ctx, db: db, ship: sh, svc: sh.Service, co: checkout.NewService(checkout.NewRepository(db), addrs, sh.Service),
		ful: fulfillment.NewService(db, pay)}
	c, err := category.NewService(category.NewRepository(db)).Create(ctx, category.Input{Name: "Atasan", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.NewService(product.NewRepository(db)).Create(ctx, product.Input{CategoryID: c.ID, Name: "Kaos Basic",
		Active: true, WeightGrams: 300, Images: []product.ImageInput{{URL: "data:image/svg+xml;base64,PHN2Zy8+"}},
		Variants: []product.VariantInput{{SKU: "KB-1", Price: 100000, Stock: 50}}}); err != nil {
		t.Fatal(err)
	}
	f.variant = f.int("SELECT id FROM product_variants WHERE sku = 'KB-1'")
	f.admin = testutil.CreateAdmin(t, db, "admin@example.com")
	f.buyer = testutil.CreateUser(t, db, "budi@example.com")
	a, err := addrs.Create(ctx, f.buyer, address.Input{ShippingAreaID: sh.AreaID(t), Label: "Rumah", RecipientName: "Budi Santoso",
		Phone: "081234567890", Village: "Dago", StreetAddress: "Jl. Ir. H. Juanda No. 100"})
	if err != nil {
		t.Fatal(err)
	}
	f.addr = a.ID
	return f
}

// order places a buy-now order with the cheapest quote (unpaid).
func (f *fixture) order(qty int) string {
	f.t.Helper()
	src := checkout.Source{VariantID: f.variant, Quantity: qty}
	d, err := f.co.Prepare(f.ctx, f.buyer, src, f.addr, "")
	if err != nil || d.Shipping.Selected == nil {
		f.t.Fatalf("prepare: %v %+v", err, d.Shipping)
	}
	p, err := f.co.PlaceOrder(f.ctx, f.buyer, checkout.PlaceRequest{Source: src, AddressID: f.addr, ShippingQuoteID: d.Shipping.Selected.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	return p.OrderNumber
}

// processing is a paid order the shop accepted (PROCESSING).
func (f *fixture) processing(qty int) string {
	f.t.Helper()
	n := f.order(qty)
	testutil.PayOrder(f.t, f.db, n)
	if err := f.ful.Confirm(f.ctx, f.admin, n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) int(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) str(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) orderStatus(n string) string {
	return f.str("SELECT status FROM orders WHERE order_number = $1", n)
}

// shipment is the active shipment of the order: "status provider_id awb".
func (f *fixture) shipment(n string) string {
	return f.str(`SELECT s.status || ' ' || coalesce(s.provider_shipment_id, '-') || ' ' || coalesce(nullif(s.tracking_number, ''), '-')
		FROM shipments s JOIN orders o ON o.id = s.order_id
		WHERE o.order_number = $1 AND s.status NOT IN ('CANCELLED', 'FAILED')`, n)
}

func (f *fixture) providerID(n string) string {
	return f.str(`SELECT s.provider_shipment_id FROM shipments s JOIN orders o ON o.id = s.order_id
		WHERE o.order_number = $1 AND s.status NOT IN ('CANCELLED', 'FAILED')`, n)
}

func TestBookOnlyPaidProcessingOrders(t *testing.T) {
	f := newFixture(t)
	unpaid := f.order(1)
	if err := f.svc.Book(f.ctx, f.admin, unpaid); !errors.Is(err, shipping.ErrNotBookable) {
		t.Errorf("unpaid: %v", err)
	}
	testutil.PayOrder(t, f.db, unpaid) // paid, not accepted yet
	if err := f.svc.Book(f.ctx, f.admin, unpaid); !errors.Is(err, shipping.ErrNotBookable) {
		t.Errorf("waiting confirmation: %v", err)
	}
	if err := f.svc.Book(f.ctx, f.admin, "ASH-TIDAK-ADA"); !errors.Is(err, shipping.ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
	if f.ship.Fake.OrderCount() != 0 || f.int("SELECT count(*) FROM shipments") != 0 {
		t.Fatal("nothing may be booked for an order that is not processing")
	}

	if err := f.ful.Confirm(f.ctx, f.admin, unpaid); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Book(f.ctx, f.admin, unpaid); err != nil {
		t.Fatal(err)
	}
	id := f.providerID(unpaid)
	sent := f.ship.Fake.LastOrder()
	if sent["courier_company"] != "jne" || sent["courier_type"] != "reg" || sent["reference_id"] != unpaid+"-S1" ||
		sent["destination_area_id"] != biteshiptest.Areas[0].ID || sent["origin_contact_name"] != shippingtest.Origin.Name {
		t.Errorf("booking request: %v", sent)
	}
	items := sent["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["weight"] != float64(300) {
		t.Errorf("items sent with the checkout weight: %v", items)
	}
	// AWB and provider ids stored; the order waits for pickup.
	if got := f.shipment(unpaid); got != "CREATED "+id+" "+f.ship.Fake.Order(id).Waybill || f.orderStatus(unpaid) != "PROCESSING" {
		t.Errorf("after booking: %q, order %s", got, f.orderStatus(unpaid))
	}
	if f.int("SELECT count(*) FROM shipment_events WHERE status = 'CREATED' AND source = 'booking'") != 1 ||
		f.int("SELECT count(*) FROM admin_audit_logs WHERE action = 'shipment.book'") != 1 {
		t.Error("first event and audit entry")
	}
	// Booking again changes nothing.
	if err := f.svc.Book(f.ctx, f.admin, unpaid); err != nil || f.ship.Fake.OrderCount() != 1 {
		t.Errorf("second booking: %v, provider orders %d", err, f.ship.Fake.OrderCount())
	}
}

func TestDoubleBookingCreatesOneShipment(t *testing.T) {
	f := newFixture(t)
	n := f.processing(2)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.svc.Book(f.ctx, f.admin, n) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, shipping.ErrBookingPending) {
			t.Errorf("concurrent booking: %v", err)
		}
	}
	if f.ship.Fake.OrderCount() != 1 || f.int("SELECT count(*) FROM shipments") != 1 || f.int("SELECT count(*) FROM admin_audit_logs WHERE action = 'shipment.book'") != 1 {
		t.Errorf("provider orders %d, shipment rows %d", f.ship.Fake.OrderCount(), f.int("SELECT count(*) FROM shipments"))
	}
}

func TestBookingFailuresKeepTheOrder(t *testing.T) {
	f := newFixture(t)
	n := f.processing(1)

	// Provider unreachable: unknown outcome, the row stays PENDING.
	f.ship.Fake.FailNext("/v1/orders", 1)
	if err := f.svc.Book(f.ctx, f.admin, n); !errors.Is(err, shipping.ErrUnavailable) {
		t.Fatalf("provider down: %v", err)
	}
	if f.str("SELECT status FROM shipments") != "PENDING" || f.orderStatus(n) != "PROCESSING" {
		t.Fatal("a provider failure must not change the order")
	}
	// A retry right away is refused (another request may still be waiting).
	if err := f.svc.Book(f.ctx, f.admin, n); !errors.Is(err, shipping.ErrBookingPending) {
		t.Errorf("retry within the lease: %v", err)
	}
	// The provider did create it but the answer was lost: the retry sends
	// the same reference and adopts that shipment instead of a second one.
	ref := f.str("SELECT booking_key FROM shipments")
	raw, _ := json.Marshal(map[string]any{"reference_id": ref, "origin_contact_name": "Toko", "origin_contact_phone": "0812",
		"origin_address": "Jl. A", "destination_contact_name": "Budi", "destination_contact_phone": "0813", "destination_address": "Jl. B",
		"courier_company": "jne", "courier_type": "reg", "delivery_type": "now", "items": []any{map[string]any{"name": "Kaos", "value": 1, "weight": 300, "quantity": 1}}})
	req, _ := http.NewRequest(http.MethodPost, f.ship.Fake.URL+"/v1/orders", bytes.NewReader(raw))
	req.Header.Set("Authorization", biteshiptest.APIKey)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("provider-side order: %v", err)
	}
	f.exec("UPDATE shipments SET booking_started_at = now() - interval '1 minute'")
	if err := f.svc.Book(f.ctx, f.admin, n); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.ship.Fake.OrderCount() != 1 || f.int("SELECT count(*) FROM shipments") != 1 || f.str("SELECT status FROM shipments") != "CREATED" {
		t.Errorf("retry must adopt the provider's shipment: orders %d, rows %d", f.ship.Fake.OrderCount(), f.int("SELECT count(*) FROM shipments"))
	}

	// A refusal marks that attempt FAILED; a new attempt gets a new reference.
	m := f.processing(1)
	f.ship.Fake.RejectNext("/v1/orders", 1)
	if err := f.svc.Book(f.ctx, f.admin, m); !errors.Is(err, shipping.ErrRejected) {
		t.Fatalf("rejected: %v", err)
	}
	if got := f.str(`SELECT s.status || ' ' || s.booking_key FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.order_number = $1`, m); got != "FAILED "+m+"-S1" ||
		f.orderStatus(m) != "PROCESSING" {
		t.Errorf("after refusal: %s, order %s", got, f.orderStatus(m))
	}
	if err := f.svc.Book(f.ctx, f.admin, m); err != nil {
		t.Fatal(err)
	}
	if f.ship.Fake.LastOrder()["reference_id"] != m+"-S2" {
		t.Errorf("new attempt reference: %v", f.ship.Fake.LastOrder()["reference_id"])
	}
}

func TestManualFallbackAndCancel(t *testing.T) {
	f := newFixture(t)
	n := f.processing(1)
	if err := f.svc.Book(f.ctx, f.admin, n); err != nil {
		t.Fatal(err)
	}
	manual := fulfillment.ShipInput{Courier: "JNE", Service: "REG", Tracking: "MANUAL-123"}
	if err := f.ful.Ship(f.ctx, f.admin, n, manual); !errors.Is(err, fulfillment.ErrShipmentActive) {
		t.Errorf("manual ship over a booking: %v", err)
	}
	if _, err := f.ful.Cancel(f.ctx, f.admin, n, "stok rusak"); !errors.Is(err, fulfillment.ErrShipmentActive) {
		t.Errorf("cancel a booked order: %v", err)
	}

	if err := f.svc.CancelBooking(f.ctx, f.admin, n, "salah kurir"); err != nil {
		t.Fatal(err)
	}
	if f.ship.Fake.Order(f.str("SELECT provider_shipment_id FROM shipments")).Status != "cancelled" ||
		f.str("SELECT status FROM shipments") != "CANCELLED" || f.orderStatus(n) != "PROCESSING" {
		t.Fatal("cancel before pickup: provider cancelled, order still processing")
	}
	// Fallback: ship manually.
	if err := f.ful.Ship(f.ctx, f.admin, n, manual); err != nil {
		t.Fatal(err)
	}
	if f.orderStatus(n) != "SHIPPED" || f.shipment(n) != "IN_TRANSIT - MANUAL-123" ||
		f.int("SELECT count(*) FROM admin_audit_logs WHERE action = 'shipment.cancel'") != 1 {
		t.Errorf("manual fallback: %s / %s", f.orderStatus(n), f.shipment(n))
	}

	// After pickup the provider refuses cancellation.
	m := f.processing(1)
	if err := f.svc.Book(f.ctx, f.admin, m); err != nil {
		t.Fatal(err)
	}
	f.ship.Fake.Advance(f.providerID(m), "picked", "Item has been picked.", time.Now())
	if err := f.svc.Sync(f.ctx, f.admin, m); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelBooking(f.ctx, f.admin, m, "terlambat"); !errors.Is(err, shipping.ErrNotCancellable) {
		t.Errorf("cancel after pickup: %v", err)
	}
}

func TestTrackingMovesTheOrder(t *testing.T) {
	f := newFixture(t)
	n := f.processing(1)
	if err := f.svc.Book(f.ctx, f.admin, n); err != nil {
		t.Fatal(err)
	}
	id := f.providerID(n)
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	f.ship.Fake.Advance(id, "picked", "Paket dijemput kurir.", t0)

	f.ship.Fake.FailNext("/v1/trackings", 1)
	if err := f.svc.Sync(f.ctx, f.admin, n); !errors.Is(err, shipping.ErrUnavailable) {
		t.Errorf("provider down: %v", err)
	}
	if f.str("SELECT status FROM shipments") != "CREATED" || f.orderStatus(n) != "PROCESSING" {
		t.Fatal("nothing changes while the provider is down")
	}

	if err := f.svc.Sync(f.ctx, f.admin, n); err != nil {
		t.Fatal(err)
	}
	if f.str("SELECT status FROM shipments") != "PICKED_UP" || f.orderStatus(n) != "SHIPPED" ||
		f.int("SELECT count(*) FROM shipments WHERE shipped_at = $1", t0) != 1 {
		t.Errorf("picked: %s / %s", f.str("SELECT status FROM shipments"), f.orderStatus(n))
	}
	f.ship.Fake.Advance(id, "dropping_off", "Paket diantar ke penerima.", t0.Add(time.Hour))
	f.ship.Fake.Advance(id, "delivered", "Paket diterima oleh BUDI.", t0.Add(2*time.Hour))
	if err := f.svc.Sync(f.ctx, f.admin, n); err != nil {
		t.Fatal(err)
	}
	// Delivered: the shipment says so, the order waits for the shop to
	// complete it (the return window starts at completion).
	if f.str("SELECT status FROM shipments") != "DELIVERED" || f.orderStatus(n) != "SHIPPED" ||
		f.int("SELECT count(*) FROM shipments WHERE delivered_at = $1", t0.Add(2*time.Hour)) != 1 {
		t.Errorf("delivered: %s / %s", f.str("SELECT status FROM shipments"), f.orderStatus(n))
	}
	// Syncing again stores nothing twice.
	events := f.int("SELECT count(*) FROM shipment_events")
	if err := f.svc.Sync(f.ctx, f.admin, n); err != nil || f.int("SELECT count(*) FROM shipment_events") != events {
		t.Errorf("repeated sync: %v, events %d → %d", err, events, f.int("SELECT count(*) FROM shipment_events"))
	}
	if err := f.ful.Complete(f.ctx, f.admin, n); err != nil || f.orderStatus(n) != "COMPLETED" {
		t.Errorf("complete after delivery: %v", err)
	}
}

func TestWebhooks(t *testing.T) {
	f := newFixture(t)
	n := f.processing(1)
	if err := f.svc.Book(f.ctx, f.admin, n); err != nil {
		t.Fatal(err)
	}
	id := f.providerID(n)
	signed := http.Header{biteshiptest.WebhookHeader: {biteshiptest.WebhookSecret}}
	send := func(h http.Header, body []byte) (shipping.WebhookResult, error) {
		return f.svc.HandleWebhook(f.ctx, h, body)
	}

	// Out of order: "dropping off" arrives before "picked".
	if res, err := send(signed, f.ship.Fake.WebhookWith("order.status", id, "dropping_off")); err != nil || res != shipping.WebhookApplied {
		t.Fatalf("dropping_off: %v %v", res, err)
	}
	if res, err := send(signed, f.ship.Fake.WebhookWith("order.status", id, "picked")); err != nil || res != shipping.WebhookApplied {
		t.Fatal(err)
	}
	if f.str("SELECT status FROM shipments") != "OUT_FOR_DELIVERY" || f.orderStatus(n) != "SHIPPED" {
		t.Errorf("late picked must not move back: %s", f.str("SELECT status FROM shipments"))
	}
	// Duplicates are stored once.
	before := f.int("SELECT count(*) FROM shipment_events")
	for range 3 {
		if _, err := send(signed, f.ship.Fake.WebhookWith("order.status", id, "picked")); err != nil {
			t.Fatal(err)
		}
	}
	if f.int("SELECT count(*) FROM shipment_events") != before {
		t.Error("duplicate webhook stored twice")
	}
	if _, err := send(http.Header{biteshiptest.WebhookHeader: {"guess"}}, f.ship.Fake.WebhookWith("order.status", id, "delivered")); !errors.Is(err, shipping.ErrInvalidSignature) {
		t.Errorf("invalid signature: %v", err)
	}
	if _, err := send(signed, []byte(`{"event":`)); !errors.Is(err, shipping.ErrMalformed) {
		t.Errorf("malformed: %v", err)
	}
	if res, err := send(signed, biteshiptest.Webhook("order.status", "unknown-order", "delivered")); err != nil || res != shipping.WebhookIgnored {
		t.Errorf("unknown shipment: %v %v", res, err)
	}
	if res, err := send(http.Header{}, nil); err != nil || res != shipping.WebhookIgnored {
		t.Errorf("installation ping: %v %v", res, err)
	}
	if f.str("SELECT status FROM shipments") != "OUT_FOR_DELIVERY" {
		t.Error("refused webhooks must change nothing")
	}
	if res, err := send(signed, f.ship.Fake.WebhookWith("order.status", id, "delivered")); err != nil || res != shipping.WebhookApplied ||
		f.str("SELECT status FROM shipments") != "DELIVERED" || f.orderStatus(n) != "SHIPPED" {
		t.Errorf("delivered: %v %s %s", err, f.str("SELECT status FROM shipments"), f.orderStatus(n))
	}
}

func TestReconcile(t *testing.T) {
	f := newFixture(t)
	n := f.processing(1)
	if err := f.svc.Book(f.ctx, f.admin, n); err != nil {
		t.Fatal(err)
	}
	f.ship.Fake.Advance(f.providerID(n), "picked", "Paket dijemput.", time.Now().Add(-time.Hour))
	f.exec("UPDATE shipments SET last_synced_at = now() - interval '2 hours'")

	stuck := f.processing(1)
	f.ship.Fake.FailNext("/v1/orders", 1)
	_ = f.svc.Book(f.ctx, f.admin, stuck)
	f.exec("UPDATE shipments SET booking_started_at = now() - interval '1 hour' WHERE status = 'PENDING'")
	f.exec("UPDATE shipping_quotes SET expires_at = now() - interval '2 days' WHERE order_id IS NULL")

	rep, err := f.svc.Reconcile(f.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 1 || rep.Errors != 0 || len(rep.StalePending) != 1 || rep.StalePending[0] != stuck {
		t.Errorf("report: %+v", rep)
	}
	if f.orderStatus(n) != "SHIPPED" || f.int("SELECT count(*) FROM shipping_quotes WHERE order_id IS NULL") != 0 ||
		f.int("SELECT count(*) FROM shipping_quotes WHERE order_id IS NOT NULL") != 2 {
		t.Errorf("after reconcile: %s, unused quotes %d", f.orderStatus(n), f.int("SELECT count(*) FROM shipping_quotes WHERE order_id IS NULL"))
	}
	// Recently synced shipments are skipped.
	if rep, _ := f.svc.Reconcile(f.ctx, 50); rep.Checked != 0 {
		t.Errorf("second run checked %d", rep.Checked)
	}
}
