package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func queryID(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// send posts a form with extra headers (HTMX, Referer).
func (b *browser) send(path string, form url.Values, headers map[string]string) response {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, location: res.Header.Get("Location"), header: res.Header, body: string(raw)}
}

var htmx = map[string]string{"HX-Request": "true"}

func TestCartRequiresLogin(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	blkM := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")

	if res := b.get("/cart"); res.status != http.StatusSeeOther || res.location != "/login?next=%2Fcart" {
		t.Errorf("guest /cart: %d %q", res.status, res.location)
	}
	// A guest's add-to-cart post returns to the product page after login.
	res := b.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(blkM)}},
		map[string]string{"Referer": base + "/products/kaos-ashanum-basic?o=1"})
	if res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "/login?next=%2Fproducts%2Fkaos-ashanum-basic") {
		t.Errorf("guest add: %d %q", res.status, res.location)
	}
	// The product page offers a login link that keeps the choice.
	hitam := queryID(t, db, "SELECT id FROM product_attribute_values WHERE value = 'Hitam'")
	m := queryID(t, db, "SELECT id FROM product_attribute_values WHERE value = 'M'")
	page := b.get(fmt.Sprintf("/products/kaos-ashanum-basic?o=%d,%d&q=2", hitam, m))
	expect(t, page, http.StatusOK, "/login?next=%2Fproducts%2Fkaos-ashanum-basic%3Fo%3D")
	if strings.Contains(page.body, `id="purchase"`) {
		t.Error("guests must not get the add-to-cart form")
	}
}

func TestCartFlow(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	alice := newBrowser(t, base)
	alice.register("alice@example.com")

	blkM := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	tunik := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'TNK-01'")
	hitam := queryID(t, db, "SELECT id FROM product_attribute_values WHERE value = 'Hitam'")
	m := queryID(t, db, "SELECT id FROM product_attribute_values WHERE value = 'M'")

	// Product page: with a full choice the add-to-cart form appears.
	page := alice.get(fmt.Sprintf("/products/kaos-ashanum-basic?o=%d,%d&q=2", hitam, m))
	expect(t, page, http.StatusOK, `id="purchase"`, fmt.Sprintf(`name="variant_id" value="%d"`, blkM),
		`name="quantity" value="2"`, `formaction="/checkout" formmethod="get"`)
	if strings.Contains(alice.get("/products/kaos-ashanum-basic").body, `id="purchase"`) {
		t.Error("without a full choice there must be no add-to-cart form")
	}

	// HTMX add: toast + out-of-band header badge.
	res := alice.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(blkM)}, "quantity": {"2"}}, htmx)
	expect(t, res, http.StatusOK, "Berhasil ditambahkan ke keranjang", "Lihat Keranjang",
		`id="cart-badge" class="cart-badge" hx-swap-oob="true">2<`)

	// Plain form post: redirect back to the product with a flash message.
	back := "/products/kaos-ashanum-basic"
	res = alice.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(blkM)}, "quantity": {"1"}, "return_to": {back}}, nil)
	if res.status != http.StatusSeeOther || res.location != back {
		t.Fatalf("plain add: %d %q", res.status, res.location)
	}
	expect(t, alice.get(back), http.StatusOK, "Berhasil ditambahkan ke keranjang.", `class="cart-badge">3<`)

	// Over stock (10, already 3 in cart) is refused with a reason.
	res = alice.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(blkM)}, "quantity": {"8"}}, htmx)
	expect(t, res, http.StatusUnprocessableEntity, "Sudah ada 3 di keranjang", "toast--error")
	// An open redirect in return_to is ignored.
	res = alice.send("/cart/items", url.Values{"variant_id": {fmt.Sprint(tunik)}, "return_to": {"//evil.example"}}, nil)
	if res.location != "/cart" {
		t.Errorf("return_to must be local, got %q", res.location)
	}

	// Cart page: lines, variant label, total and badge without an extra query.
	cart := alice.get("/cart")
	expect(t, cart, http.StatusOK, "Kaos Ashanum Basic", "Varian: Hitam / M", "Tunik Polos 01",
		"Total (4 produk)", "Rp438.000", `class="cart-badge">4<`, `formaction="/checkout"`, "Checkout (2)")
	if cc := cart.header.Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("cart Cache-Control = %q", cc)
	}

	kaosItem := queryID(t, db, "SELECT id FROM cart_items WHERE variant_id = $1", blkM)
	tunikItem := queryID(t, db, "SELECT id FROM cart_items WHERE variant_id = $1", tunik)

	// Selection is request state: only the chosen line counts.
	sel := fmt.Sprintf("/cart?sel=1&item=%d", kaosItem)
	expect(t, alice.get(sel), http.StatusOK, "Total (3 produk)", "Rp387.000")
	frag := alice.doWithTarget(sel, "cart")
	expect(t, frag, http.StatusOK, `id="cart"`, "Rp387.000", `hx-swap-oob="true"`)
	if strings.Contains(frag.body, "<html") {
		t.Error("HTMX selection must return the cart fragment only")
	}

	// Update quantity (HTMX) keeps the selection and refreshes the badge.
	form := url.Values{"quantity": {"1"}, "sel": {"1"}, "item": {fmt.Sprint(kaosItem)}}
	res = alice.send(fmt.Sprintf("/cart/items/%d", kaosItem), form, htmx)
	expect(t, res, http.StatusOK, "Total (1 produk)", "Rp129.000", `id="cart-badge" class="cart-badge" hx-swap-oob="true">2<`)
	res = alice.send(fmt.Sprintf("/cart/items/%d", kaosItem), url.Values{"quantity": {"11"}}, htmx)
	expect(t, res, http.StatusUnprocessableEntity, "Stok tersedia hanya 10.")

	// Plain-form update redirects back keeping the selection.
	res = alice.send(fmt.Sprintf("/cart/items/%d", kaosItem), form, nil)
	if res.status != http.StatusSeeOther || res.location != fmt.Sprintf("/cart?item=%d&sel=1", kaosItem) {
		t.Errorf("plain update: %d %q", res.status, res.location)
	}

	// Bob cannot change or remove Alice's lines.
	bob := newBrowser(t, base)
	bob.register("bob@example.com")
	expect(t, bob.send(fmt.Sprintf("/cart/items/%d", kaosItem), url.Values{"quantity": {"5"}}, nil), http.StatusNotFound)
	expect(t, bob.send(fmt.Sprintf("/cart/items/%d/delete", kaosItem), url.Values{}, nil), http.StatusNotFound)
	expect(t, bob.get("/cart"), http.StatusOK, "Keranjang masih kosong")

	// Delete (HTMX).
	res = alice.send(fmt.Sprintf("/cart/items/%d/delete", tunikItem), url.Values{}, htmx)
	expect(t, res, http.StatusOK, "Produk dihapus dari keranjang.")
	if strings.Contains(res.body, "Tunik Polos 01") {
		t.Error("deleted line still shown")
	}

	// A product deactivated after adding: shown as unavailable, not counted.
	if _, err := db.Exec(context.Background(), "UPDATE products SET active = false WHERE slug = 'kaos-ashanum-basic'"); err != nil {
		t.Fatal(err)
	}
	cart = alice.get("/cart")
	expect(t, cart, http.StatusOK, "Produk tidak tersedia", "Total (0 produk)", "Rp0", "Hapus")
	if !strings.Contains(cart.body, fmt.Sprintf(`value="%d" disabled`, kaosItem)) {
		t.Error("an unavailable line must not be selectable")
	}
}
