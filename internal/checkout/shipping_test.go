package checkout

import (
	"errors"
	"testing"

	"ecommerce/internal/address"
	"ecommerce/internal/shipping"
)

func TestShippingQuotesFromServerData(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 50)
	f.exec("UPDATE products SET weight_grams = 400") // 400 g per unit

	// The weight is the sum of catalog weights × quantity; more units, more kg.
	d1, err := f.svc.Prepare(f.ctx, uid, Source{VariantID: v, Quantity: 2}, addr, "")
	if err != nil {
		t.Fatal(err)
	}
	if d1.WeightGrams() != 800 || d1.Shipping.State != ShippingReady || d1.Shipping.Selected == nil || d1.ShippingCost != 9000 ||
		d1.Total != 200000+9000 || len(d1.Shipping.Options) != 2 || d1.Shipping.Options[0].Rate.Price > d1.Shipping.Options[1].Rate.Price {
		t.Fatalf("2 units: weight %d, %+v, total %d", d1.WeightGrams(), d1.Shipping, d1.Total)
	}
	body := f.ship.Fake.LastRates()
	item := body["items"].([]any)[0].(map[string]any)
	if item["weight"] != float64(400) || item["quantity"] != float64(2) || body["destination_area_id"] == nil {
		t.Errorf("rate request: %v", body)
	}
	d3, _ := f.svc.Prepare(f.ctx, uid, Source{VariantID: v, Quantity: 3}, addr, "")
	if d3.WeightGrams() != 1200 || d3.ShippingCost != 18000 { // 1.2 kg → 2 kg
		t.Errorf("3 units: weight %d cost %d", d3.WeightGrams(), d3.ShippingCost)
	}

	// Same route and weight: rates come from the cache, the quote id is stable.
	calls := f.ship.Fake.Calls("POST /v1/rates")
	again, _ := f.svc.Prepare(f.ctx, uid, Source{VariantID: v, Quantity: 2}, addr, d1.Shipping.Options[1].ID)
	if f.ship.Fake.Calls("POST /v1/rates") != calls || again.Shipping.Selected.ID != d1.Shipping.Options[1].ID ||
		again.ShippingCost != d1.Shipping.Options[1].Rate.Price || again.Shipping.Changed {
		t.Errorf("cached rates / chosen option: calls %d→%d, %+v", calls, f.ship.Fake.Calls("POST /v1/rates"), again.Shipping.Selected)
	}
	// An unknown quote id falls back to the cheapest and says so.
	other, _ := f.svc.Prepare(f.ctx, uid, Source{VariantID: v, Quantity: 2}, addr, "not-a-quote")
	if !other.Shipping.Changed || other.Shipping.Selected.ID != d1.Shipping.Options[0].ID {
		t.Errorf("unknown quote: %+v", other.Shipping)
	}
}

func TestShippingStates(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	v := f.product("Kaos A", "A-1", 100000, 5)
	src := Source{VariantID: v, Quantity: 1}

	// Address without a delivery area (saved before Phase 10).
	noArea, err := f.addrs.Create(f.ctx, uid, address.Input{Label: "Lama", RecipientName: "Budi", Phone: "081234567890",
		Province: "Jawa Barat", City: "Bandung", District: "Coblong", Village: "Dago", PostalCode: "40135", StreetAddress: "Jl. Lama No. 1"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := f.svc.Prepare(f.ctx, uid, src, noArea.ID, "")
	if d.Shipping.State != ShippingNeedArea || d.Ready() || d.Total != 100000 {
		t.Errorf("no area: %+v", d.Shipping)
	}
	if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: noArea.ID, ShippingQuoteID: "x"}); !errors.Is(err, ErrAreaRequired) {
		t.Errorf("place without area: %v", err)
	}

	// Inactive variant: no rates asked, cannot be ordered.
	calls := f.ship.Fake.Calls("POST /v1/rates")
	f.exec("UPDATE product_variants SET active = false")
	if d, _ := f.svc.Prepare(f.ctx, uid, src, addr, ""); d.Shipping.State != ShippingBlocked || f.ship.Fake.Calls("POST /v1/rates") != calls {
		t.Errorf("inactive variant: %+v", d.Shipping)
	}
	f.exec("UPDATE product_variants SET active = true")

	// No weight in the catalog: never an assumed weight.
	f.exec("UPDATE products SET weight_grams = 0")
	d, _ = f.svc.Prepare(f.ctx, uid, src, addr, "")
	if d.Lines[0].Problem != ProblemNoWeight || d.Shipping.State != ShippingBlocked || d.Ready() {
		t.Errorf("no weight: %+v %+v", d.Lines[0], d.Shipping)
	}
	if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: "x"}); !errors.Is(err, ErrNotShippable) {
		t.Errorf("place without weight: %v", err)
	}
	f.exec("UPDATE product_variants SET weight_grams = 250") // a variant weight overrides the product's
	if d, _ := f.svc.Prepare(f.ctx, uid, src, addr, ""); !d.Ready() || d.WeightGrams() != 250 {
		t.Errorf("variant weight: %d %+v", d.WeightGrams(), d.Shipping)
	}
	f.exec("UPDATE products SET weight_grams = 100")
	f.exec("UPDATE product_variants SET weight_grams = NULL")

	// Provider down (2 units: a weight not in the rate cache yet): honest
	// state, no order, never shipping 0.
	f.ship.Fake.FailNext("/v1/rates", 1)
	d, err = f.svc.Prepare(f.ctx, uid, Source{VariantID: v, Quantity: 2}, addr, "")
	if err != nil || d.Shipping.State != ShippingUnavailable || d.Ready() || d.Shipping.Message() != "Pengiriman sementara tidak dapat dihitung. Silakan coba lagi." {
		t.Errorf("provider down: %v %+v", err, d.Shipping)
	}
	if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: Source{VariantID: v, Quantity: 2}, AddressID: addr}); !errors.Is(err, ErrShippingRequired) {
		t.Errorf("place without a quote: %v", err)
	}
	f.ship.Fake.SetNoCourier(true)
	if d, _ := f.svc.Prepare(f.ctx, uid, Source{VariantID: v, Quantity: 3}, addr, ""); d.Shipping.State != ShippingNoCourier || d.Ready() {
		t.Errorf("no courier: %+v", d.Shipping)
	}
	if f.id("SELECT count(*) FROM orders") != 0 {
		t.Error("no order may be created in these states")
	}
}

func TestQuoteValidation(t *testing.T) {
	f := newFixture(t)
	uid, addr := f.user("a@example.com")
	bob, bobAddr := f.user("bob@example.com")
	v := f.product("Kaos A", "A-1", 100000, 20)
	src := Source{VariantID: v, Quantity: 1}
	quote := func(user, address int64, s Source) string {
		t.Helper()
		d, err := f.svc.Prepare(f.ctx, user, s, address, "")
		if err != nil || d.Shipping.Selected == nil {
			t.Fatalf("prepare: %v %+v", err, d.Shipping)
		}
		return d.Shipping.Selected.ID
	}
	refused := func(label string, req PlaceRequest, user int64) {
		t.Helper()
		if _, err := f.svc.PlaceOrder(f.ctx, user, req); !errors.Is(err, shipping.ErrQuoteChanged) {
			t.Errorf("%s: %v", label, err)
		}
		if f.id("SELECT count(*) FROM orders") != 0 || f.stock(v) != 20 {
			t.Fatalf("%s: nothing may be written", label)
		}
	}

	q := quote(uid, addr, src)
	refused("another user's quote", PlaceRequest{Source: src, AddressID: bobAddr, ShippingQuoteID: q}, bob)
	refused("package changed", PlaceRequest{Source: Source{VariantID: v, Quantity: 2}, AddressID: addr, ShippingQuoteID: q}, uid)
	f.exec("UPDATE products SET weight_grams = 900")
	refused("weight changed", PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: q}, uid)
	f.exec("UPDATE products SET weight_grams = 100")
	cilandak := f.ship.Area(t, "Cilandak")
	f.exec("UPDATE user_addresses SET shipping_area_id = $1, postal_code = '12430' WHERE id = $2", cilandak.ID, addr)
	refused("address changed", PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: q}, uid)
	f.exec("UPDATE user_addresses SET shipping_area_id = $1, postal_code = '40132' WHERE id = $2", f.ship.AreaID(t), addr)
	f.exec("UPDATE shipping_quotes SET expires_at = now() - interval '1 second'")
	refused("expired", PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: q}, uid)
	refused("unknown", PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: "00000000-0000-0000-0000-000000000000"}, uid)

	// A fresh quote works once; the shipping cost comes from it only.
	q = quote(uid, addr, src)
	f.exec("UPDATE shipping_quotes SET price = 12345 WHERE id = $1", q) // what the server stored is what is charged
	if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: q, ExpectedTotal: 100000 + 9000}); !errors.Is(err, ErrPriceChanged) {
		t.Errorf("total seen by the buyer differs: %v", err)
	}
	p, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: q, ExpectedTotal: 100000 + 12345})
	if err != nil || p.Total != 112345 {
		t.Fatalf("place: %+v %v", p, err)
	}
	o, err := f.orders.Get(f.ctx, uid, p.OrderNumber)
	if err != nil || o.ShippingCost != 12345 || o.Total != 112345 || o.Shipping == nil || o.Shipping.CourierCode != "jne" ||
		o.Shipping.ServiceCode != "reg" || o.Shipping.WeightGrams != 100 || o.Shipping.AreaLabel == "" || o.Shipping.Name() != "JNE Reguler" {
		t.Errorf("order snapshot: %+v %+v", o.Shipping, err)
	}
	if f.str("SELECT shipping_origin->>'postal_code' FROM orders") != "40111" || f.id("SELECT weight_grams FROM order_items") != 100 {
		t.Error("origin and item weight snapshots")
	}
	refused2 := func() {
		if _, err := f.svc.PlaceOrder(f.ctx, uid, PlaceRequest{Source: src, AddressID: addr, ShippingQuoteID: q, IdempotencyKey: "other"}); !errors.Is(err, shipping.ErrQuoteChanged) {
			t.Errorf("a used quote: %v", err)
		}
	}
	refused2()

	// Later rate changes never touch the order.
	f.exec("UPDATE shipping_quotes SET price = 99999")
	f.ship.Fake.SetNoCourier(true)
	if o, _ := f.orders.Get(f.ctx, uid, p.OrderNumber); o.ShippingCost != 12345 || o.Total != 112345 {
		t.Errorf("historical order changed: %d %d", o.ShippingCost, o.Total)
	}
}

func (f *fixture) str(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return s
}
