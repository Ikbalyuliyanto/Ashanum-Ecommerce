package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var successPath = regexp.MustCompile(`^/checkout/success/(ASH-\d{8}-\d{6,})$`)

// addAddress saves the buyer's home address with its delivery area, picked
// from the area search like the address form does.
func (b *browser) addAddress() {
	b.t.Helper()
	res := b.post("/account/addresses", url.Values{
		"label": {"Rumah"}, "recipient_name": {"Siti Rahmawati"}, "phone": {"081298765432"},
		"province": {"Jawa Barat"}, "city": {"Kota Bandung"}, "district": {"Coblong"}, "village": {"Dago"},
		"postal_code": {"40135"}, "street_address": {"Jl. Ir. H. Juanda No. 100"}, "shipping_area_id": {b.areaID("Coblong")},
	})
	if res.status != http.StatusSeeOther {
		b.t.Fatalf("add address: %d", res.status)
	}
}

// areaID searches delivery areas (HTMX fragment of the address form) and
// returns the first result's id.
func (b *browser) areaID(q string) string {
	b.t.Helper()
	res := b.get("/account/addresses/areas?area_q=" + url.QueryEscape(q))
	m := regexp.MustCompile(`name="shipping_area_id" value="([^"]+)"`).FindStringSubmatch(res.body)
	if res.status != http.StatusOK || m == nil {
		b.t.Fatalf("area search %q: %d %s", q, res.status, res.body)
	}
	return m[1]
}

func hiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("hidden input %q not found", name)
	}
	return m[1]
}

func TestCheckoutFromCart(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")

	if res := newBrowser(t, base).get("/checkout?item=1"); res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "/login?next=") {
		t.Errorf("guest checkout: %d %q", res.status, res.location)
	}

	blkM := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	t1 := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'TNK-01'")
	t2 := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'TNK-02'")
	for _, v := range []int64{blkM, t1, t2} {
		b.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(v)}, "quantity": {"1"}}, htmx)
	}
	itemK := queryID(t, db, "SELECT id FROM cart_items WHERE variant_id = $1", blkM)
	itemT2 := queryID(t, db, "SELECT id FROM cart_items WHERE variant_id = $1", t2)
	path := fmt.Sprintf("/checkout?item=%d&item=%d", itemK, itemT2)

	// Without an address the order cannot be placed yet.
	page := b.get(path)
	expect(t, page, http.StatusOK, "Belum ada alamat pengiriman", "Tambah Alamat", "Buat Pesanan</button>")
	if !strings.Contains(page.body, `type="submit" disabled>Buat Pesanan`) {
		t.Error("Buat Pesanan must be disabled without an address")
	}

	b.addAddress()
	page = b.get(path)
	// Shipping: the cheapest option is selected and included in the total
	// (2 × 100 g → 1 kg, JNE Reguler Rp9.000 in the fake provider).
	expect(t, page, http.StatusOK, "Siti Rahmawati", "Kaos Ashanum Basic", "Tunik Polos 02",
		"Rp181.000", "JNE Reguler", "Rp9.000", "Rp190.000", "Estimasi tiba 2–3 hari", "Belum tersedia")
	if strings.Contains(page.body, "Gratis Ongkir") || strings.Contains(page.body, "Tunik Polos 01") || strings.Contains(page.body, "Total belum termasuk ongkos kirim") {
		t.Error("checkout must show only the selected lines, never 'Gratis Ongkir', and include shipping in the total")
	}
	if hiddenValue(t, page.body, "expected_total") != "190000" {
		t.Errorf("expected_total = %s", hiddenValue(t, page.body, "expected_total"))
	}

	form := url.Values{
		"item":              {fmt.Sprint(itemK), fmt.Sprint(itemT2)},
		"address_id":        {hiddenValue(t, page.body, "address_id")},
		"shipping_quote_id": {hiddenValue(t, page.body, "shipping_quote_id")},
		"expected_total":    {hiddenValue(t, page.body, "expected_total")},
		"key":               {hiddenValue(t, page.body, "key")},
	}

	// A total that no longer matches is refused and re-rendered fresh.
	stale := url.Values{}
	for k, v := range form {
		stale[k] = v
	}
	stale.Set("expected_total", "1")
	expect(t, b.post("/checkout/order", stale), http.StatusUnprocessableEntity, "Harga produk berubah", "Rp181.000")

	res := b.post("/checkout/order", form)
	m := successPath.FindStringSubmatch(res.location)
	if res.status != http.StatusSeeOther || m == nil {
		t.Fatalf("place order: %d %q\n%s", res.status, res.location, res.body)
	}
	number := m[1]
	expect(t, b.get(res.location), http.StatusOK, "Pesanan berhasil dibuat", number, "Rp190.000", "Belum Bayar")

	// Double submit returns the same order.
	if again := b.post("/checkout/order", form); again.location != res.location {
		t.Errorf("double submit: %q", again.location)
	}

	// Only the purchased lines left the cart.
	expect(t, b.get("/cart"), http.StatusOK, "Tunik Polos 01")
	if strings.Contains(b.get("/cart").body, "Kaos Ashanum Basic") {
		t.Error("purchased line still in cart")
	}

	// History and detail.
	expect(t, b.get("/account/orders"), http.StatusOK, number, "Belum Bayar", "Kaos Ashanum Basic", "+1 produk lainnya", "Rp190.000")
	expect(t, b.get("/account/orders?status=unpaid"), http.StatusOK, number)
	expect(t, b.get("/account/orders?status=completed"), http.StatusOK, "Belum ada pesanan")
	expect(t, b.get("/account/orders/"+number), http.StatusOK, number, "Menunggu pembayaran",
		"Siti Rahmawati", "Jl. Ir. H. Juanda No. 100", "Varian: Hitam / M", "Tunik Polos 02", "JNE Reguler", "Rp9.000", "Rp181.000", "Rp190.000")

	// Another user cannot see the order.
	other := newBrowser(t, base)
	other.register("other@example.com")
	expect(t, other.get("/account/orders/"+number), http.StatusNotFound)
	expect(t, other.get("/checkout/success/"+number), http.StatusNotFound)
	if strings.Contains(other.get("/account/orders").body, number) {
		t.Error("another user's history lists the order")
	}
}

func TestBuyNowAndAddressPicker(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	b.post("/account/addresses", url.Values{
		"label": {"Kantor"}, "recipient_name": {"Siti Kantor"}, "phone": {"082112345678"},
		"province": {"DKI Jakarta"}, "city": {"Kota Jakarta Selatan"}, "district": {"Setiabudi"}, "village": {"Kuningan"},
		"postal_code": {"12950"}, "street_address": {"Gedung Ashanum Lt. 12"}, "shipping_area_id": {b.areaID("Cilandak")},
	})
	office := queryID(t, db, "SELECT id FROM user_addresses WHERE label = 'Kantor'")
	blkM := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	tunik := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'TNK-05'")
	b.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(tunik)}}, htmx)

	// Buy Now: checkout of one variant; the default address is the primary one.
	buy := fmt.Sprintf("/checkout?variant_id=%d&quantity=2", blkM)
	page := b.get(buy)
	expect(t, page, http.StatusOK, "Kaos Ashanum Basic", "× 2", "Rp258.000", "Siti Rahmawati", `name="variant_id"`)

	// Picking another address (HTMX) re-renders the checkout with it.
	frag := b.doWithTarget(buy+fmt.Sprintf("&address=%d", office), "checkout")
	expect(t, frag, http.StatusOK, `id="checkout"`, "Siti Kantor", "Gedung Ashanum Lt. 12", fmt.Sprintf(`name="address_id" value="%d"`, office))
	if strings.Contains(frag.body, "<html") {
		t.Error("address picker must return the checkout fragment only")
	}

	res := b.post("/checkout/order", url.Values{
		"variant_id": {fmt.Sprint(blkM)}, "quantity": {"2"}, "address_id": {fmt.Sprint(office)},
		"shipping_quote_id": {hiddenValue(t, frag.body, "shipping_quote_id")},
		"expected_total":    {"267000"}, "key": {hiddenValue(t, page.body, "key")},
	})
	m := successPath.FindStringSubmatch(res.location)
	if m == nil {
		t.Fatalf("buy now order: %d %q", res.status, res.location)
	}
	expect(t, b.get("/account/orders/"+m[1]), http.StatusOK, "Siti Kantor", "Kuningan")
	if stock := queryID(t, db, "SELECT stock FROM product_variants WHERE id = $1", blkM); stock != 8 {
		t.Errorf("stock = %d, want 8", stock)
	}
	expect(t, b.get("/cart"), http.StatusOK, "Tunik Polos 05") // Buy Now left the cart alone

	// Buy Now of a sold-out variant goes back with the reason.
	white := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-WHT-M'")
	res = b.get(fmt.Sprintf("/checkout?variant_id=%d&quantity=1&return_to=%%2Fproducts%%2Fkaos-ashanum-basic", white))
	if res.status != http.StatusSeeOther || res.location != "/products/kaos-ashanum-basic" {
		t.Errorf("sold-out buy now: %d %q", res.status, res.location)
	}
	expect(t, b.get("/products/kaos-ashanum-basic"), http.StatusOK, "Stok produk ini sedang habis.")

	// Stock changed between page and order: friendly 422 with fresh data.
	if _, err := db.Exec(context.Background(), "UPDATE product_variants SET stock = 1 WHERE id = $1", blkM); err != nil {
		t.Fatal(err)
	}
	expect(t, b.post("/checkout/order", url.Values{
		"variant_id": {fmt.Sprint(blkM)}, "quantity": {"2"}, "address_id": {fmt.Sprint(office)}, "key": {"fresh-key"},
	}), http.StatusUnprocessableEntity, "Stok produk berubah. Silakan periksa kembali pesanan Anda.", "Stok tersedia hanya 1")
}
