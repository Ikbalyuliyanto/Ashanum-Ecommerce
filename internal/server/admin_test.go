package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/testutil"
)

// newAdmin registers an account and grants it ADMIN in the database, as
// "ecommerce create-admin -promote" does. The role is read on every request,
// so the open session is an admin session at once.
func newAdmin(t *testing.T, base string, db *pgxpool.Pool) *browser {
	t.Helper()
	b := newBrowser(t, base)
	b.register("admin@ashanum.id")
	if _, err := db.Exec(context.Background(), "UPDATE users SET role = 'ADMIN' WHERE email = 'admin@ashanum.id'"); err != nil {
		t.Fatal(err)
	}
	return b
}

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestAdminAccessControl(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	variant := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	stock := func() int64 { return queryID(t, db, "SELECT stock FROM product_variants WHERE id = $1", variant) }
	stockPath := fmt.Sprintf("/admin/stock/%d", variant)
	adjust := url.Values{"mode": {"add"}, "quantity": {"5"}, "reason": {"Barang masuk"}}

	// Guests are sent to the login page.
	guest := newBrowser(t, base)
	if res := guest.get("/admin/orders"); res.status != http.StatusSeeOther || res.location != "/login?next=%2Fadmin%2Forders" {
		t.Errorf("guest /admin/orders: %d %q", res.status, res.location)
	}
	if res := guest.do(http.MethodGet, "/admin", nil, true); res.status != http.StatusUnauthorized || !strings.HasPrefix(res.header.Get("HX-Redirect"), "/login") {
		t.Errorf("guest HTMX /admin: %d %q", res.status, res.header.Get("HX-Redirect"))
	}
	if res := guest.post(stockPath, adjust); res.status != http.StatusSeeOther || stock() != 10 {
		t.Errorf("guest POST: %d, stock %d", res.status, stock())
	}

	// A customer never sees the admin area, and cannot post to it.
	cust := newBrowser(t, base)
	cust.register("budi@example.com")
	for _, p := range []string{"/admin", "/admin/orders", "/admin/products/new", stockPath, "/admin/returns", "/admin/reviews", "/admin/reports", "/admin/audit"} {
		expect(t, cust.get(p), http.StatusNotFound)
	}
	for _, p := range []string{stockPath, "/admin/orders/ASH-X/confirm", "/admin/reviews/1/hide", "/admin/products", "/admin/categories"} {
		expect(t, cust.post(p, adjust), http.StatusNotFound)
	}
	if stock() != 10 {
		t.Fatalf("a customer changed stock: %d", stock())
	}
	if strings.Contains(cust.get("/account").body, "Panel Admin") {
		t.Error("a customer is offered the admin panel")
	}

	// Registration never takes a role from the form.
	evil := newBrowser(t, base)
	evil.post("/register", url.Values{"email": {"evil@example.com"}, "password": {"rahasia123"}, "password_confirm": {"rahasia123"}, "role": {"ADMIN"}})
	if role := queryString(t, db, "SELECT role FROM users WHERE email = 'evil@example.com'"); role != "CUSTOMER" {
		t.Errorf("registered role = %q", role)
	}
	expect(t, evil.get("/admin"), http.StatusNotFound)

	// An admin gets every page, private and not indexed.
	admin := newAdmin(t, base, db)
	for _, p := range []string{"/admin", "/admin/orders", "/admin/orders?status=confirm&q=ASH", "/admin/products", "/admin/products/new",
		"/admin/stock", "/admin/stock?status=low", stockPath, "/admin/categories", "/admin/returns", "/admin/refunds",
		"/admin/reviews", "/admin/reports", "/admin/reports?range=custom&from=2026-01-01&to=2026-01-31", "/admin/audit"} {
		res := admin.get(p)
		expect(t, res, http.StatusOK, "ASHANUM <small>Admin</small>")
		if res.header.Get("Cache-Control") != "private, no-store" || !strings.Contains(res.header.Get("X-Robots-Tag"), "noindex") {
			t.Errorf("%s headers: %q %q", p, res.header.Get("Cache-Control"), res.header.Get("X-Robots-Tag"))
		}
		if strings.Contains(res.body, "style=") || strings.Contains(res.body, "hx-on") || strings.Contains(res.body, "<script>") {
			t.Errorf("%s: inline style/script/hx-on breaks the CSP", p)
		}
	}
	expect(t, admin.get("/account"), http.StatusOK, "Panel Admin")
	expect(t, admin.get("/admin/orders/ASH-TIDAK-ADA"), http.StatusNotFound)
	expect(t, admin.get("/admin/products/999999/edit"), http.StatusNotFound)

	// Cross-site POST from an admin's browser is refused (CSRF).
	res := admin.send(stockPath, adjust, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if res.status != http.StatusForbidden || stock() != 10 {
		t.Errorf("cross-site admin POST: %d, stock %d", res.status, stock())
	}
	if res := admin.post(stockPath, adjust); res.status != http.StatusSeeOther || stock() != 15 {
		t.Fatalf("admin stock adjustment: %d %q, stock %d", res.status, res.location, stock())
	}
	expect(t, admin.post(stockPath, url.Values{"mode": {"remove"}, "quantity": {"99"}, "reason": {"Rusak"}}),
		http.StatusUnprocessableEntity, "Stok tidak boleh negatif")
	if stock() != 15 {
		t.Errorf("a refused adjustment changed stock: %d", stock())
	}

	// Taking the role away ends access at the next request.
	exec(t, db, "UPDATE users SET role = 'CUSTOMER' WHERE email = 'admin@ashanum.id'")
	expect(t, admin.get("/admin"), http.StatusNotFound)
}

func TestAdminOrderToReturnFlow(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	buyer := newBrowser(t, base)
	buyer.register("siti@example.com")
	buyer.addAddress()
	number := buyNow(t, buyer, db)
	orderPath := "/admin/orders/" + number
	admin := newAdmin(t, base, db)
	status := func() string { return queryString(t, db, "SELECT status FROM orders WHERE order_number = $1", number) }

	// Unpaid: the admin cannot process it.
	res := admin.post(orderPath+"/confirm", nil)
	if res.status != http.StatusSeeOther || status() != "WAITING_PAYMENT" {
		t.Fatalf("confirm unpaid: %d %s", res.status, status())
	}
	expect(t, admin.get(orderPath), http.StatusOK, "Pesanan belum dibayar", "Menunggu pembayaran pembeli")

	testutil.PayOrder(t, db, number) // the provider confirmed the payment
	expect(t, admin.get(orderPath), http.StatusOK, "Konfirmasi &amp; Proses", "siti@example.com", "Siti Rahmawati")
	if res := admin.post(orderPath+"/confirm", nil); res.status != http.StatusSeeOther || status() != "PROCESSING" {
		t.Fatalf("confirm: %d %s", res.status, status())
	}
	expect(t, admin.post(orderPath+"/ship", url.Values{"courier": {""}}), http.StatusUnprocessableEntity, "Kurir")
	// Only whitelisted fields are read: status, total and paid_at are ignored.
	total := queryID(t, db, "SELECT total FROM orders WHERE order_number = $1", number)
	if res := admin.post(orderPath+"/ship", url.Values{"courier": {"JNE"}, "service": {"REG"}, "tracking": {"JNE00012345"},
		"status": {"COMPLETED"}, "total": {"1"}, "paid_at": {"2020-01-01"}, "user_id": {"1"}}); res.status != http.StatusSeeOther {
		t.Fatalf("ship: %d", res.status)
	}
	if status() != "SHIPPED" || queryID(t, db, "SELECT total FROM orders WHERE order_number = $1", number) != total {
		t.Errorf("after ship: %s", status())
	}
	expect(t, buyer.get("/account/orders/"+number), http.StatusOK, "Pesanan sudah dikirim", "JNE", "JNE00012345", "Data pengiriman diisi oleh penjual")
	if res := admin.post(orderPath+"/complete", nil); res.status != http.StatusSeeOther || status() != "COMPLETED" {
		t.Fatalf("complete: %d %s", res.status, status())
	}

	// The normal flow (no fixture) opens reviews and returns to the buyer.
	expect(t, buyer.get("/account/orders/"+number), http.StatusOK, "Pesanan selesai", "Beri Ulasan", "Ajukan Pengembalian")
	item := queryID(t, db, "SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1", number)
	res = buyer.post("/account/returns", url.Values{"order": {number}, "key": {"k-1"}, "item": {fmt.Sprint(item)},
		fmt.Sprintf("qty_%d", item): {"1"}, "type": {"REFUND"}, "reason": {"DAMAGED"}})
	if res.status != http.StatusSeeOther {
		t.Fatalf("buyer return: %d\n%s", res.status, res.body)
	}
	retNumber := strings.TrimPrefix(res.location, "/account/returns/")
	retPath := "/admin/returns/" + retNumber
	line := queryID(t, db, "SELECT ri.id FROM return_items ri JOIN returns r ON r.id = ri.return_id WHERE r.return_number = $1", retNumber)
	qty := fmt.Sprintf("qty_%d", line)

	expect(t, admin.get("/admin/returns?status=review"), http.StatusOK, retNumber)
	expect(t, admin.get(retPath), http.StatusOK, "Setujui (boleh sebagian)", "estimasi refund", "Rp129.000")
	frag := admin.doWithTarget(retPath+"/estimate?"+qty+"=0", "approve-estimate")
	if frag.status != http.StatusOK || strings.Contains(frag.body, "<html") || !strings.Contains(frag.body, "Disetujui 0 dari 1") {
		t.Errorf("HTMX estimate: %d %q", frag.status, frag.body)
	}
	expect(t, admin.post(retPath+"/approve", url.Values{qty: {"0"}}), http.StatusSeeOther)
	expect(t, admin.get(retPath), http.StatusOK, "Setujui minimal satu barang")
	stock := func() int64 { return queryID(t, db, "SELECT stock FROM product_variants WHERE sku = 'ASH-BLK-M'") }
	steps := []struct {
		path string
		form url.Values
	}{
		{"/approve", url.Values{qty: {"1"}, "note": {"Silakan kirim"}, "refund_amount": {"999999999"}}},
		{"/waiting-return", url.Values{"note": {"Kirim ke gudang Bandung"}}},
		{"/received", url.Values{fmt.Sprintf("condition_%d", line): {"RESTOCKABLE"}}},
	}
	for _, s := range steps {
		if res := admin.post(retPath+s.path, s.form); res.status != http.StatusSeeOther || res.location != retPath {
			t.Fatalf("%s: %d %q", s.path, res.status, res.location)
		}
	}
	if amount := queryID(t, db, "SELECT refund_amount FROM returns WHERE return_number = $1", retNumber); amount != 129000 {
		t.Errorf("refund amount = %d: it must come from the price paid, never the form", amount)
	}
	before := stock()
	expect(t, admin.post(retPath+"/restock", nil), http.StatusSeeOther)
	expect(t, admin.post(retPath+"/restock", nil), http.StatusSeeOther)
	if stock() != before+1 {
		t.Errorf("restock (twice) moved stock %d → %d, want +1", before, stock())
	}
	expect(t, admin.post(retPath+"/prepare-refund", nil), http.StatusSeeOther)
	expect(t, admin.get(retPath), http.StatusOK, "Catat Refund Berhasil", "Rp129.000")
	expect(t, admin.post(retPath+"/complete-refund", url.Values{"method": {"manual"}, "reference": {"BCA-123456"}}), http.StatusSeeOther)
	expect(t, admin.post(retPath+"/complete", nil), http.StatusSeeOther)
	expect(t, buyer.get("/account/returns/"+retNumber), http.StatusOK, "Selesai", "Berhasil")
	expect(t, admin.get("/admin/refunds?status=succeeded"), http.StatusOK, retNumber, "Berhasil")
	expect(t, admin.get("/admin/audit?entity=RETURN"), http.StatusOK, "Setujui pengembalian", "Kembalikan barang ke stok")
	expect(t, admin.get("/admin/reports?range=today"), http.StatusOK, "Rp129.000")

	// Review moderation through the admin; customers cannot moderate.
	res = buyer.post("/account/reviews", url.Values{"item": {fmt.Sprint(item)}, "rating": {"1"}, "comment": {"Promo situs lain www.spam.example"}})
	if res.status != http.StatusSeeOther {
		t.Fatalf("review: %d", res.status)
	}
	reviewID := queryID(t, db, "SELECT id FROM product_reviews")
	expect(t, buyer.post(fmt.Sprintf("/admin/reviews/%d/hide", reviewID), url.Values{"reason": {"spam"}}), http.StatusNotFound)
	expect(t, buyer.get("/products/kaos-ashanum-basic"), http.StatusOK, "spam.example")
	expect(t, admin.post(fmt.Sprintf("/admin/reviews/%d/hide", reviewID), url.Values{"reason": {""}}), http.StatusSeeOther)
	if queryID(t, db, "SELECT count(*) FROM product_reviews WHERE hidden_at IS NOT NULL") != 0 {
		t.Error("hidden without a reason")
	}
	res = admin.post(fmt.Sprintf("/admin/reviews/%d/hide", reviewID), url.Values{"reason": {"Spam / tautan promosi"}, "return_to": {"/admin/reviews?status=hidden"}})
	if res.status != http.StatusSeeOther || res.location != "/admin/reviews?status=hidden" {
		t.Fatalf("hide: %d %q", res.status, res.location)
	}
	page := buyer.get("/products/kaos-ashanum-basic")
	if strings.Contains(page.body, "spam.example") {
		t.Error("a hidden review is still public")
	}
	expect(t, buyer.get("/account/reviews"), http.StatusOK, "Tidak ditampilkan publik")
	expect(t, admin.get("/admin/reviews?status=hidden"), http.StatusOK, "Spam / tautan promosi", "Tampilkan Lagi")
	if res := admin.post(fmt.Sprintf("/admin/reviews/%d/hide", reviewID), url.Values{"return_to": {"https://evil.example/"}}); res.location != "/admin/reviews" {
		t.Errorf("return_to must stay in the admin: %q", res.location)
	}
}

func TestAdminCatalogForms(t *testing.T) {
	base, db := newApp(t)
	seedCatalog(t, db)
	admin := newAdmin(t, base, db)
	cat := queryID(t, db, "SELECT id FROM categories WHERE slug = 'atasan'")

	expect(t, admin.post("/admin/products", url.Values{"name": {"X"}}), http.StatusUnprocessableEntity, "Periksa kembali")
	form := url.Values{"name": {"Celana Chino"}, "category_id": {fmt.Sprint(cat)}, "brand": {"Ashanum"}, "active": {"1"},
		"attr_name_1": {"Ukuran"}, "attr_values_1": {"30, 32, 34"}, "sku_prefix": {"CHINO"}, "price": {"Rp 249.000"}, "stock": {"3"},
		"weight_grams": {"450"}, "sold_count": {"999"}, "rating_average": {"5"}}
	noWeight := url.Values{}
	for k, v := range form {
		noWeight[k] = v
	}
	noWeight.Del("weight_grams")
	expect(t, admin.post("/admin/products", noWeight), http.StatusUnprocessableEntity, "Isi berat produk (gram)")
	res := admin.post("/admin/products", form)
	if res.status != http.StatusSeeOther || !strings.HasSuffix(res.location, "/edit") {
		t.Fatalf("create product: %d %q\n%s", res.status, res.location, res.body)
	}
	id := queryID(t, db, "SELECT id FROM products WHERE slug = 'celana-chino'")
	if queryID(t, db, "SELECT count(*) FROM product_variants WHERE product_id = $1 AND price = 249000 AND stock = 3", id) != 3 ||
		queryID(t, db, "SELECT sold_count FROM products WHERE id = $1", id) != 0 {
		t.Error("generated variants / ignored fields")
	}
	edit := admin.get(res.location)
	expect(t, edit, http.StatusOK, "CHINO-30", "CHINO-34", "Atur Stok", "Tambah varian", "Tambah atribut", "Stok awal")

	variant := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'CHINO-32'")
	vPath := fmt.Sprintf("/admin/products/%d/variants/%d", id, variant)
	expect(t, admin.post(vPath, url.Values{"sku": {"CHINO-32"}, "price": {"259000"}, "active": {"1"}, "stock": {"500"}}), http.StatusSeeOther)
	if queryID(t, db, "SELECT price FROM product_variants WHERE id = $1", variant) != 259000 || queryID(t, db, "SELECT stock FROM product_variants WHERE id = $1", variant) != 3 {
		t.Error("variant edit: price changes, stock never from this form")
	}
	if res := admin.post(vPath, url.Values{"sku": {"CHINO-30"}, "price": {"259000"}, "active": {"1"}}); res.status != http.StatusSeeOther {
		t.Fatalf("duplicate SKU: %d", res.status)
	}
	expect(t, admin.get(fmt.Sprintf("/admin/products/%d/edit", id)), http.StatusOK, "SKU sudah dipakai")
	expect(t, admin.post(fmt.Sprintf("/admin/products/%d/variants", id), url.Values{"opt:Ukuran": {"36"}, "sku": {"CHINO-36"}, "price": {"249000"}, "stock": {"1"}}), http.StatusSeeOther)
	expect(t, admin.post(fmt.Sprintf("/admin/products/%d/deactivate", id), nil), http.StatusSeeOther)
	expect(t, admin.get("/products/celana-chino"), http.StatusNotFound)
	expect(t, admin.get("/admin/products?status=inactive"), http.StatusOK, "Celana Chino")
	expect(t, admin.get("/admin/products?q=CHINO-36"), http.StatusOK, "Celana Chino")

	expect(t, admin.post("/admin/categories", url.Values{"name": {"Bawahan"}, "active": {"1"}, "display_order": {"2"}}), http.StatusSeeOther)
	expect(t, admin.post("/admin/categories", url.Values{"name": {"Bawahan"}}), http.StatusUnprocessableEntity, "Slug sudah dipakai")
	expect(t, admin.get("/admin/categories"), http.StatusOK, "Bawahan", "Arsip")
	if queryID(t, db, "SELECT count(*) FROM admin_audit_logs WHERE action IN ('product.create', 'variant.update', 'variant.create', 'product.deactivate', 'category.create')") != 5 {
		t.Error("catalog audit entries")
	}
}
