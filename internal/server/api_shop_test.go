package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/config"
	"ecommerce/internal/payment/midtrans"
	"ecommerce/internal/payment/midtrans/midtranstest"
	"ecommerce/internal/testutil"
)

// newPaymentServer is newPaymentApp with a configuration hook.
func newPaymentServer(t *testing.T, db *pgxpool.Pool, mod func(*config.Config)) (string, *midtranstest.Server) {
	fake := midtranstest.New(t)
	mt, err := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: midtranstest.ServerKey, SnapURL: fake.URL, APIURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(buildRouterCfg(t, db, config.EnvDevelopment, mt, mt.CheckoutOrigin(), mod))
	t.Cleanup(srv.Close)
	return srv.URL, fake
}

func TestAPICatalog(t *testing.T) {
	base, db := newAPIApp(t, nil)
	seedCatalog(t, db)
	exec(t, db, "UPDATE product_images SET url = 'https://res.cloudinary.com/demo/image/upload/kaos.jpg' WHERE product_id = (SELECT id FROM products WHERE slug = 'kaos-ashanum-basic')")
	c := newAPI(t, base) // catalog is public

	res := c.get("/products?per_page=5")
	want(t, res, http.StatusOK, "")
	if len(res.list()) != 5 || num(res.meta()["total"]) != 23 || res.meta()["has_more"] != true || num(res.meta()["per_page"]) != 5 {
		t.Errorf("list meta: %v (%d items)", res.meta(), len(res.list()))
	}
	if strings.Contains(res.raw, "Produk Nonaktif") || strings.Contains(res.raw, "Produk Kategori Arsip") {
		t.Error("hidden products listed")
	}
	last := c.get("/products?per_page=5&page=5")
	if len(last.list()) != 3 || last.meta()["has_more"] != false {
		t.Errorf("last page: %d %v", len(last.list()), last.meta())
	}
	if c.get("/products?per_page=500").meta()["per_page"] != float64(60) {
		t.Error("per_page must be capped at 60")
	}

	res = c.get("/products?q=kaos")
	if len(res.list()) != 1 || res.meta()["sort"] != "relevance" {
		t.Fatalf("search: %s", res.raw)
	}
	k := res.list()[0].(map[string]any)
	if k["price"] != float64(129000) || k["compare_at_price"] != float64(159000) || k["discount_percent"] != float64(19) ||
		!strings.Contains(k["image_url"].(string), "/image/upload/c_limit,w_400,f_auto,q_auto/") {
		t.Errorf("card: %v", k)
	}
	if !strings.Contains(res.raw, `"price":129000,`) {
		t.Error("money must be a JSON integer")
	}
	res = c.get("/products?category=atasan&min_price=60000&max_price=70000&sort=price_desc")
	prices := []float64{}
	for _, p := range res.list() {
		prices = append(prices, p.(map[string]any)["price"].(float64))
	}
	if len(prices) == 0 || prices[0] != 70000 || prices[len(prices)-1] < 60000 {
		t.Errorf("filter + sort: %v", prices)
	}
	res = c.get("/products?sort=termahal&page=x&category=tidak-ada")
	want(t, res, http.StatusBadRequest, "INVALID_REQUEST")
	if res.field("sort") == "" || res.field("page") == "" || res.field("category") == "" {
		t.Errorf("param errors: %s", res.raw)
	}

	res = c.get("/products/kaos-ashanum-basic")
	want(t, res, http.StatusOK, "")
	d := res.data()
	vs := d["variants"].([]any)
	if len(vs) != 2 || d["in_stock"] != true || d["rating"].(map[string]any)["count"] != float64(56) {
		t.Fatalf("detail: %s", res.raw)
	}
	for _, v := range vs {
		v := v.(map[string]any)
		attrs := v["attributes"].(map[string]any)
		switch v["sku"] {
		case "ASH-BLK-M":
			if attrs["Warna"] != "Hitam" || attrs["Ukuran"] != "M" || v["available"] != true || v["stock"] != float64(10) {
				t.Errorf("black variant: %v", v)
			}
		case "ASH-WHT-M":
			if v["available"] != false || v["compare_at_price"] != nil {
				t.Errorf("white variant (no stock): %v", v)
			}
		}
	}
	if p := d["price"].(map[string]any); p["min"] != float64(129000) || p["max"] != float64(129000) {
		t.Errorf("price summary: %v", p)
	}
	img := d["images"].([]any)[0].(map[string]any)
	if !strings.Contains(img["url"].(string), "w_900") || !strings.Contains(img["thumbnail_url"].(string), "w_200") {
		t.Errorf("images: %v", img)
	}
	for _, leak := range []string{"sold_count_internal", "category_active", "updated_at", "featured"} {
		if strings.Contains(res.raw, leak) {
			t.Errorf("detail leaks %q", leak)
		}
	}
	want(t, c.get("/products/produk-nonaktif"), http.StatusNotFound, "NOT_FOUND")
	want(t, c.get("/products/tidak-ada"), http.StatusNotFound, "NOT_FOUND")

	res = c.get("/categories")
	if len(res.list()) != 1 || res.list()[0].(map[string]any)["slug"] != "atasan" {
		t.Errorf("categories (active only): %s", res.raw)
	}
	res = c.get("/home")
	if len(res.data()["flash_sale"].([]any)) != 1 || len(res.data()["categories"].([]any)) != 1 {
		t.Errorf("home: %s", res.raw)
	}
	res = c.get("/products/kaos-ashanum-basic/reviews")
	want(t, res, http.StatusOK, "")
	if res.meta()["rating"] == nil || len(res.list()) != 0 {
		t.Errorf("reviews: %s", res.raw)
	}
	want(t, c.get("/products/tidak-ada/reviews"), http.StatusNotFound, "NOT_FOUND")
}

func TestAPICartCheckoutAndOrders(t *testing.T) {
	base, db := newAPIApp(t, nil)
	seedCatalog(t, db)
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	white := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-WHT-M'")
	off := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'OFF-1'")
	a, b := newAPI(t, base), newAPI(t, base)
	a.signIn("a@example.com")
	b.signIn("b@example.com")

	// Cart: one query, lines ready to render.
	want(t, a.get("/cart"), http.StatusOK, "")
	res := a.post("/cart/items", map[string]any{"variant_id": v, "quantity": 2})
	want(t, res, http.StatusCreated, "")
	res = a.post("/cart/items", map[string]any{"variant_id": v, "quantity": 1})
	items := res.data()["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("same variant must merge: %s", res.raw)
	}
	it := items[0].(map[string]any)
	if it["quantity"] != float64(3) || it["product_slug"] != "kaos-ashanum-basic" || it["product_id"] == nil ||
		it["variant_label"] != "Hitam / M" || it["unit_price"] != float64(129000) || it["subtotal"] != float64(387000) ||
		it["stock"] != float64(10) || it["available"] != true {
		t.Errorf("cart item: %v", it)
	}
	if s := res.data()["summary"].(map[string]any); s["subtotal"] != float64(387000) || s["quantity"] != float64(3) {
		t.Errorf("cart summary: %v", s)
	}
	itemID := num(it["id"])
	res = a.post("/cart/items", map[string]any{"variant_id": v, "quantity": 20})
	want(t, res, http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	if res.field("quantity") == "" {
		t.Errorf("stock error field: %s", res.raw)
	}
	want(t, a.post("/cart/items", map[string]any{"variant_id": white, "quantity": 1}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	want(t, a.post("/cart/items", map[string]any{"variant_id": off, "quantity": 1}), http.StatusUnprocessableEntity, "PRODUCT_UNAVAILABLE")
	want(t, a.post("/cart/items", map[string]any{"variant_id": v, "quantity": "2"}), http.StatusBadRequest, "INVALID_REQUEST")
	res = a.call(http.MethodPatch, fmt.Sprintf("/cart/items/%d", itemID), map[string]any{"quantity": 2})
	if res.status != http.StatusOK || res.data()["items"].([]any)[0].(map[string]any)["quantity"] != float64(2) {
		t.Errorf("update quantity: %s", res.raw)
	}
	want(t, b.call(http.MethodPatch, fmt.Sprintf("/cart/items/%d", itemID), map[string]any{"quantity": 1}), http.StatusNotFound, "NOT_FOUND")
	want(t, b.call(http.MethodDelete, fmt.Sprintf("/cart/items/%d", itemID), nil), http.StatusNotFound, "NOT_FOUND")

	// Concurrent adds never exceed stock (10).
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); b.post("/cart/items", map[string]any{"variant_id": v, "quantity": 2}) }()
	}
	wg.Wait()
	if q := b.get("/cart").data()["summary"].(map[string]any)["quantity"].(float64); q > 10 || q < 2 {
		t.Errorf("concurrent adds: quantity %v", q)
	}
	if r := b.call(http.MethodDelete, "/cart", nil); r.status != http.StatusNoContent || len(b.get("/cart").data()["items"].([]any)) != 0 {
		t.Errorf("clear cart: %d", r.status)
	}

	// Addresses: ownership enforced.
	res = a.post("/me/addresses", map[string]any{"label": "Rumah", "recipient_name": "Siti Rahmawati", "phone": "081298765432",
		"province": "Jawa Barat", "city": "Kota Bandung", "district": "Coblong", "village": "Dago", "postal_code": "40135",
		"street_address": "Jl. Ir. H. Juanda No. 100", "latitude": -6.89, "longitude": 107.61, "shipping_area_id": a.area("Coblong")})
	want(t, res, http.StatusCreated, "")
	addr := num(res.data()["id"])
	if res.data()["is_primary"] != true || res.data()["phone"] != "+6281298765432" || res.header.Get("Location") == "" {
		t.Errorf("address: %s", res.raw)
	}
	want(t, a.post("/me/addresses", map[string]any{"label": "X"}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	res = b.post("/me/addresses", map[string]any{"label": "Kantor", "recipient_name": "Budi", "phone": "081234567890",
		"province": "DKI Jakarta", "city": "Jakarta Selatan", "district": "Setiabudi", "village": "Kuningan", "postal_code": "12950",
		"street_address": "Jl. Rasuna Said 1", "shipping_area_id": b.area("Cilandak")})
	bAddr := num(res.data()["id"])
	want(t, b.call(http.MethodPatch, fmt.Sprintf("/me/addresses/%d", addr), map[string]any{"label": "Curian"}), http.StatusNotFound, "NOT_FOUND")
	want(t, b.call(http.MethodDelete, fmt.Sprintf("/me/addresses/%d", addr), nil), http.StatusNotFound, "NOT_FOUND")
	want(t, b.post(fmt.Sprintf("/me/addresses/%d/primary", addr), nil), http.StatusNotFound, "NOT_FOUND")
	res = a.call(http.MethodPatch, fmt.Sprintf("/me/addresses/%d", addr), map[string]any{"label": "Rumah Utama"})
	if res.status != http.StatusOK || res.data()["label"] != "Rumah Utama" || res.data()["city"] != "Bandung" || res.data()["shipping_area_label"] == nil {
		t.Errorf("patch keeps other fields: %s", res.raw)
	}
	if l := a.get("/me/addresses").list(); len(l) != 1 {
		t.Errorf("addresses: %d", len(l))
	}

	// Preview: server prices and stock, honest shipping, ownership.
	res = a.post("/checkout/preview", map[string]any{"cart_item_ids": []int64{itemID}})
	want(t, res, http.StatusOK, "")
	p := res.data()
	if p["total"] != float64(258000+9000) || p["shipping_cost"] != float64(9000) || p["ready"] != true || p["address"].(map[string]any)["id"] != float64(addr) {
		t.Errorf("preview: %s", res.raw)
	}
	if sh := p["shipping"].(map[string]any); sh["cost"] != float64(9000) || sh["status"] != "READY" || sh["weight_grams"] != float64(200) ||
		sh["selected"].(map[string]any)["courier_code"] != "jne" || sh["selected"].(map[string]any)["quote_id"] == "" || len(sh["options"].([]any)) != 2 {
		t.Errorf("shipping options: %v", sh)
	}
	want(t, a.post("/checkout/preview", map[string]any{"cart_item_ids": []int64{itemID}, "address_id": bAddr}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	want(t, b.post("/checkout/preview", map[string]any{"cart_item_ids": []int64{itemID}}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	want(t, a.post("/checkout/preview", map[string]any{"cart_item_ids": []int64{itemID}, "buy_now": map[string]any{"variant_id": v, "quantity": 1}}),
		http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	res = a.post("/checkout/preview", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}})
	if res.data()["total"] != float64(129000+9000) || res.data()["is_buy_now"] != true {
		t.Errorf("buy now preview: %s", res.raw)
	}
	exec(t, db, "UPDATE product_variants SET price = 139000 WHERE id = $1", v)
	if res = a.post("/checkout/preview", map[string]any{"cart_item_ids": []int64{itemID}}); res.data()["total"] != float64(278000+9000) {
		t.Errorf("price recalculated: %v", res.data()["total"])
	}

	// Create order: idempotent, stale total refused, stock revalidated, the
	// shipping cost only from a stored quote (no client cost field).
	quote, _ := a.quote(map[string]any{"cart_item_ids": []int64{itemID}, "address_id": addr})
	order := map[string]any{"cart_item_ids": []int64{itemID}, "address_id": addr, "shipping_quote_id": quote,
		"idempotency_key": "order-key-0001", "expected_total": 258000 + 9000}
	want(t, a.post("/orders", order), http.StatusConflict, "PRICE_CHANGED")
	forged := map[string]any{"cart_item_ids": []int64{itemID}, "address_id": addr, "shipping_quote_id": quote,
		"idempotency_key": "order-key-0003", "shipping_cost": 0}
	want(t, a.post("/orders", forged), http.StatusBadRequest, "UNKNOWN_FIELD") // no client shipping cost
	want(t, b.post("/orders", map[string]any{"cart_item_ids": []int64{itemID}, "address_id": bAddr, "shipping_quote_id": quote,
		"idempotency_key": "order-key-0004"}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	order["expected_total"] = 278000 + 9000
	want(t, a.post("/orders", map[string]any{"cart_item_ids": []int64{itemID}, "address_id": addr}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	want(t, a.post("/orders", map[string]any{"cart_item_ids": []int64{itemID}, "address_id": bAddr, "idempotency_key": "order-key-0002"}),
		http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	res = a.post("/orders", order)
	want(t, res, http.StatusCreated, "")
	o := res.data()
	number := o["order_number"].(string)
	if o["status"] != "WAITING_PAYMENT" || o["total"] != float64(278000+9000) || o["shipping_cost"] != float64(9000) ||
		o["shipping"].(map[string]any)["courier_name"] != "JNE" || res.header.Get("Location") != "/api/v1/orders/"+number ||
		!rfc3339WIB.MatchString(o["created_at"].(string)) || o["actions"].(map[string]any)["can_cancel"] != true {
		t.Errorf("order: %s", res.raw)
	}
	again := a.post("/orders", order)
	if again.status != http.StatusOK || again.data()["order_number"] != number {
		t.Errorf("idempotent create: %d %v", again.status, again.data()["order_number"])
	}
	if len(a.get("/cart").data()["items"].([]any)) != 0 {
		t.Error("ordered cart lines must leave the cart")
	}

	// Snapshot preserved; another user cannot see the order.
	exec(t, db, "UPDATE products SET name = 'Nama Baru' WHERE slug = 'kaos-ashanum-basic'")
	res = a.get("/orders/" + number)
	if res.data()["items"].([]any)[0].(map[string]any)["product_name"] != "Kaos Ashanum Basic" {
		t.Errorf("snapshot: %s", res.raw)
	}
	want(t, b.get("/orders/"+number), http.StatusNotFound, "NOT_FOUND")
	want(t, b.post("/orders/"+number+"/cancel", nil), http.StatusNotFound, "NOT_FOUND")

	// Last unit: two buyers at once, one order.
	exec(t, db, "UPDATE product_variants SET stock = 1, price = 129000 WHERE id = $1", v)
	var mu sync.Mutex
	codes := []int{}
	for i, cl := range []*apiClient{a, b} {
		wg.Add(1)
		addrID := addr
		if i == 1 {
			addrID = bAddr
		}
		q, _ := cl.quote(map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addrID})
		go func() {
			defer wg.Done()
			r := cl.post("/orders", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addrID,
				"shipping_quote_id": q, "idempotency_key": fmt.Sprintf("last-unit-%d", i)})
			mu.Lock()
			codes = append(codes, r.status)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !(contains(codes, 201) && contains(codes, 409)) {
		t.Errorf("last unit: %v", codes)
	}
	want(t, b.post("/orders", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": bAddr,
		"shipping_quote_id": "stale-quote", "idempotency_key": "after-sold-out"}), http.StatusConflict, "STOCK_CHANGED")

	// Lists, filters, cancel.
	res = a.get("/orders?status=unpaid")
	if res.status != http.StatusOK || len(res.list()) < 1 || res.meta()["per_page"] != float64(10) {
		t.Errorf("orders list: %s", res.raw)
	}
	want(t, a.get("/orders?status=semua"), http.StatusBadRequest, "INVALID_REQUEST")
	res = a.post("/orders/"+number+"/cancel", nil)
	if res.status != http.StatusOK || res.data()["status"] != "CANCELLED" || res.data()["cancel_reason"] != "USER_CANCELLED" {
		t.Errorf("cancel: %s", res.raw)
	}
	want(t, a.post("/orders/"+number+"/cancel", nil), http.StatusOK, "") // already cancelled: no change
	if len(a.get("/orders?status=cancelled").list()) != 1 {
		t.Error("cancelled filter")
	}

	// Payments disabled in this app: honest 503, no state change.
	unpaid := a.get("/orders?status=unpaid").list()
	if len(unpaid) > 0 {
		n := unpaid[0].(map[string]any)["order_number"].(string)
		want(t, a.post("/orders/"+n+"/payment", nil), http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE")
		res = a.get("/orders/" + n + "/payment")
		if res.status != http.StatusOK || res.data()["paid"] != false || res.data()["order_status"] != "WAITING_PAYMENT" {
			t.Errorf("payment status: %s", res.raw)
		}
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func TestAPIPaymentReturnsToAppLink(t *testing.T) {
	db := testutil.DB(t)
	srv, fake := newPaymentServer(t, db, func(c *config.Config) {
		c.API.PaymentFinishURL = "ashanum://payment/finish?order={order_number}"
	})
	seedCatalog(t, db)
	a := newAPI(t, srv)
	a.signIn("a@example.com")
	res := a.post("/me/addresses", map[string]any{"label": "Rumah", "recipient_name": "Siti", "phone": "081298765432",
		"province": "Jawa Barat", "city": "Kota Bandung", "district": "Coblong", "village": "Dago", "postal_code": "40135",
		"street_address": "Jl. Ir. H. Juanda No. 100", "shipping_area_id": a.area("Coblong")})
	addr := num(res.data()["id"])
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	q, _ := a.quote(map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addr})
	res = a.post("/orders", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addr,
		"shipping_quote_id": q, "idempotency_key": "pay-key-001"})
	number := res.data()["order_number"].(string)

	res = a.post("/orders/"+number+"/payment", nil)
	want(t, res, http.StatusOK, "")
	if !strings.HasPrefix(res.data()["redirect_url"].(string), "http") || res.data()["sandbox"] != true ||
		res.data()["payment"].(map[string]any)["status"] != "PENDING" {
		t.Fatalf("payment start: %s", res.raw)
	}
	snap := fake.SnapRequest(number + "-P1")
	raw, _ := json.Marshal(snap)
	if !strings.Contains(string(raw), "ashanum://payment/finish?order="+number) {
		t.Errorf("finish URL must be the configured app link: %s", raw)
	}
	// Coming back from the provider changes nothing by itself; the status
	// check asks the provider.
	if res = a.get("/orders/" + number + "/payment"); res.data()["paid"] != false {
		t.Errorf("unpaid after redirect: %s", res.raw)
	}
	fake.SetStatus(number+"-P1", "settlement", "bank_transfer", 129000+9000) // items + shipping = orders.total
	res = a.get("/orders/" + number + "/payment")
	if res.data()["paid"] != true || res.data()["order_status"] != "WAITING_CONFIRMATION" {
		t.Errorf("after the provider confirms: %s", res.raw)
	}
}

func TestAPIReviewsAndReturns(t *testing.T) {
	base, db := newAPIApp(t, nil)
	seedCatalog(t, db)
	a, b := newAPI(t, base), newAPI(t, base)
	a.signIn("a@example.com")
	b.signIn("b@example.com")
	res := a.post("/me/addresses", map[string]any{"label": "Rumah", "recipient_name": "Siti Rahmawati", "phone": "081298765432",
		"province": "Jawa Barat", "city": "Kota Bandung", "district": "Coblong", "village": "Dago", "postal_code": "40135",
		"street_address": "Jl. Ir. H. Juanda No. 100", "shipping_area_id": a.area("Coblong")})
	addr := num(res.data()["id"])
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	q, _ := a.quote(map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 2}, "address_id": addr})
	res = a.post("/orders", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 2}, "address_id": addr,
		"shipping_quote_id": q, "idempotency_key": "rv-order-01"})
	number := res.data()["order_number"].(string)
	item := num(res.data()["items"].([]any)[0].(map[string]any)["id"])

	// Not completed yet: no review, no return.
	want(t, a.post("/reviews", map[string]any{"order_item_id": item, "rating": 5}), http.StatusConflict, "REVIEW_NOT_ALLOWED")
	want(t, a.post("/returns", map[string]any{"order_number": number, "items": []any{map[string]any{"order_item_id": item, "quantity": 1}},
		"type": "REFUND", "reason": "DAMAGED"}), http.StatusConflict, "RETURN_NOT_ALLOWED")
	testutil.CompleteOrder(t, db, number, time.Now())
	o := a.get("/orders/" + number).data()
	if act := o["actions"].(map[string]any); act["can_review"] != true || act["can_return"] != true || o["return_deadline"] == nil {
		t.Errorf("completed order actions: %v", o)
	}

	want(t, a.post("/reviews", map[string]any{"order_item_id": item}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	want(t, b.post("/reviews", map[string]any{"order_item_id": item, "rating": 5}), http.StatusNotFound, "NOT_FOUND")
	res = a.post("/reviews", map[string]any{"order_item_id": item, "rating": 4, "comment": "Bagus"})
	want(t, res, http.StatusCreated, "")
	rid := num(res.data()["id"])
	want(t, a.post("/reviews", map[string]any{"order_item_id": item, "rating": 5}), http.StatusConflict, "ALREADY_REVIEWED")
	res = a.call(http.MethodPatch, fmt.Sprintf("/reviews/%d", rid), map[string]any{"rating": 5})
	if res.status != http.StatusOK || res.data()["rating"] != float64(5) || res.data()["comment"] != "Bagus" || res.data()["edited_at"] == nil {
		t.Errorf("edit review: %s", res.raw)
	}
	want(t, b.call(http.MethodPatch, fmt.Sprintf("/reviews/%d", rid), map[string]any{"rating": 1}), http.StatusNotFound, "NOT_FOUND")
	want(t, a.call(http.MethodPatch, fmt.Sprintf("/reviews/%d", rid), map[string]any{"rating": 9}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	res = a.get("/me/reviews")
	if len(res.list()) != 1 || res.meta()["per_page"] != float64(10) || res.meta()["has_more"] != false {
		t.Errorf("my reviews: %s", res.raw)
	}
	if res = newAPI(t, base).get("/products/kaos-ashanum-basic/reviews"); len(res.list()) != 1 || strings.Contains(res.raw, "a@example.com") {
		t.Errorf("public reviews (masked): %s", res.raw)
	}

	// Returns.
	ret := map[string]any{"order_number": number, "items": []any{map[string]any{"order_item_id": item, "quantity": 1}},
		"type": "REFUND", "reason": "DAMAGED", "description": "Jahitan lepas", "evidence": []string{"https://drive.google.com/file/d/abc/view"},
		"idempotency_key": "ret-key-1"}
	want(t, b.post("/returns", ret), http.StatusNotFound, "NOT_FOUND")
	res = a.post("/returns", ret)
	want(t, res, http.StatusCreated, "")
	rn := res.data()["return_number"].(string)
	if res.data()["refund_amount"] != float64(129000) || res.data()["status"] != "REQUESTED" || res.data()["can_cancel"] != true ||
		res.data()["timeline"].([]any)[0].(map[string]any)["actor"] != "BUYER" {
		t.Errorf("return: %s", res.raw)
	}
	if again := a.post("/returns", ret); again.data()["return_number"] != rn {
		t.Errorf("idempotent return: %s", again.raw)
	}
	bad := map[string]any{"order_number": number, "items": []any{map[string]any{"order_item_id": item, "quantity": 5}}, "type": "REFUND", "reason": "OTHER"}
	res = a.post("/returns", bad)
	want(t, res, http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	if res.field("description") == "" {
		t.Errorf("return validation: %s", res.raw)
	}
	want(t, b.get("/me/returns/"+rn), http.StatusNotFound, "NOT_FOUND")
	want(t, b.post("/me/returns/"+rn+"/cancel", nil), http.StatusNotFound, "NOT_FOUND")
	if l := a.get("/me/returns"); len(l.list()) != 1 || l.meta()["per_page"] != float64(10) {
		t.Errorf("my returns: %s", l.raw)
	}
	res = a.post("/me/returns/"+rn+"/cancel", nil)
	if res.status != http.StatusOK || res.data()["status"] != "CANCELLED" {
		t.Errorf("cancel return: %s", res.raw)
	}
}

// TestAPIContract pins the shape of the main responses: no secret or
// internal field, integer money, RFC 3339 (+07:00) times.
func TestAPIContract(t *testing.T) {
	base, db := newAPIApp(t, nil)
	seedCatalog(t, db)
	a := newAPI(t, base)
	a.signIn("a@example.com")
	res := a.post("/me/addresses", map[string]any{"label": "Rumah", "recipient_name": "Siti", "phone": "081298765432",
		"province": "Jawa Barat", "city": "Kota Bandung", "district": "Coblong", "village": "Dago", "postal_code": "40135",
		"street_address": "Jl. Ir. H. Juanda No. 100", "shipping_area_id": a.area("Coblong")})
	addr := num(res.data()["id"])
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	a.post("/cart/items", map[string]any{"variant_id": v, "quantity": 1})
	q, _ := a.quote(map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addr})
	order := a.post("/orders", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addr,
		"shipping_quote_id": q, "idempotency_key": "contract-1"})

	bodies := map[string]apiRes{
		"me": a.get("/me"), "cart": a.get("/cart"), "order": order, "orders": a.get("/orders"),
		"products": a.get("/products"), "product": a.get("/products/kaos-ashanum-basic"), "addresses": a.get("/me/addresses"),
		"tracking": a.get("/orders/" + order.data()["order_number"].(string) + "/tracking"),
		"areas":    a.get("/shipping/areas?q=Coblong"),
		"rates":    a.post("/shipping/rates", map[string]any{"buy_now": map[string]any{"variant_id": v, "quantity": 1}, "address_id": addr}),
	}
	forbidden := []string{"password", "_hash", "token", "role", "admin", "user_id", "provider_order_id", "snap", "audit", "idempotency"}
	timeKey := regexp.MustCompile(`"[a-z_]*_at":"([^"]*)"`)
	for name, r := range bodies {
		if r.status >= 300 {
			t.Fatalf("%s: %d %s", name, r.status, r.raw)
		}
		low := strings.ToLower(r.raw)
		for _, f := range forbidden {
			if strings.Contains(low, f) {
				t.Errorf("%s leaks %q: %s", name, f, r.raw)
			}
		}
		for _, m := range timeKey.FindAllStringSubmatch(r.raw, -1) {
			if !rfc3339WIB.MatchString(m[1]) {
				t.Errorf("%s: time %q is not RFC 3339 +07:00", name, m[1])
			}
		}
		if strings.Contains(r.raw, ".00,") || strings.Contains(r.raw, `"Rp`) {
			t.Errorf("%s: money must be integer rupiah: %s", name, r.raw)
		}
	}
}
