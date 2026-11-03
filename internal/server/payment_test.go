package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/config"
	"ecommerce/internal/payment/midtrans"
	"ecommerce/internal/payment/midtrans/midtranstest"
	"ecommerce/internal/testutil"
)

// newPaymentApp runs the app with the real Midtrans client talking to an
// in-memory Midtrans (no internet).
func newPaymentApp(t *testing.T) (string, *pgxpool.Pool, *midtranstest.Server) {
	db := testutil.DB(t)
	fake := midtranstest.New(t)
	mt, err := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: midtranstest.ServerKey, SnapURL: fake.URL, APIURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(buildRouterWith(t, db, config.EnvDevelopment, mt, mt.CheckoutOrigin()))
	t.Cleanup(srv.Close)
	return srv.URL, db, fake
}

// buyNow places a Buy Now order of one ASH-BLK-M (Rp129.000) and returns
// its number.
func buyNow(t *testing.T, b *browser, db *pgxpool.Pool) string {
	t.Helper()
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	page := b.get(fmt.Sprintf("/checkout?variant_id=%d&quantity=1", v))
	res := b.post("/checkout/order", url.Values{
		"variant_id": {fmt.Sprint(v)}, "quantity": {"1"},
		"address_id":     {hiddenValue(t, page.body, "address_id")},
		"expected_total": {hiddenValue(t, page.body, "expected_total")},
		"key":            {hiddenValue(t, page.body, "key")},
	})
	m := successPath.FindStringSubmatch(res.location)
	if m == nil {
		t.Fatalf("buy now: %d %q", res.status, res.location)
	}
	return m[1]
}

// webhook posts a notification like Midtrans does: no cookie, no Origin.
func webhook(t *testing.T, base string, body []byte) int {
	t.Helper()
	res, err := http.Post(base+"/webhooks/midtrans", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestPaymentFlow(t *testing.T) {
	base, db, fake := newPaymentApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	number := buyNow(t, b, db)
	p1 := number + "-P1"
	detailPath := "/account/orders/" + number

	expect(t, b.get("/checkout/success/"+number), http.StatusOK, "Bayar Sekarang", "Midtrans Sandbox", "Selesaikan pembayaran sebelum")
	detail := b.get(detailPath)
	expect(t, detail, http.StatusOK, "Belum Bayar", "Bayar Sekarang", "Batas Pembayaran", "Belum ada pembayaran",
		"Batalkan Pesanan", "Mode uji", "Total belum termasuk ongkos kirim")
	if csp := detail.header.Get("Content-Security-Policy"); !strings.HasSuffix(csp, "form-action 'self' "+fake.URL) ||
		!strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "connect-src 'self';") {
		t.Errorf("CSP must only add the checkout origin to form-action: %q", csp)
	}

	// Bayar Sekarang → 303 to the hosted checkout; a double click reuses it.
	pay := b.post(detailPath+"/pay", nil)
	if pay.status != http.StatusSeeOther || pay.location != fake.URL+"/snap/v4/redirection/tok-"+p1 {
		t.Fatalf("pay: %d %q", pay.status, pay.location)
	}
	if again := b.post(detailPath+"/pay", nil); again.location != pay.location || fake.Count("POST /snap") != 1 {
		t.Errorf("double click: %q, snap calls %d", again.location, fake.Count("POST /snap"))
	}
	td := fake.SnapRequest(p1)["transaction_details"].(map[string]any)
	if td["gross_amount"] != float64(129000) || fake.BadAuth() != 0 {
		t.Errorf("snap amount %v, bad auth %d", td["gross_amount"], fake.BadAuth())
	}

	// Coming back with "settlement" in the URL proves nothing.
	fin := b.get(detailPath + "/payment/finish?order_id=" + p1 + "&status_code=200&transaction_status=settlement")
	expect(t, fin, http.StatusOK, "Menunggu Pembayaran", number)
	if s := queryString(t, db, "SELECT status FROM payments WHERE provider_order_id = $1", p1); s != "PENDING" {
		t.Errorf("payment after redirect = %s, want PENDING", s)
	}

	// A forged notification is rejected and changes nothing.
	fake.SetStatus(p1, "settlement", "bank_transfer", 129000)
	var forged map[string]any
	json.Unmarshal(fake.Notification(p1), &forged)
	forged["signature_key"] = strings.Repeat("a", 128)
	raw, _ := json.Marshal(forged)
	if code := webhook(t, base, raw); code != http.StatusUnauthorized {
		t.Errorf("forged webhook = %d", code)
	}
	if s := queryString(t, db, "SELECT status FROM orders WHERE order_number = $1", number); s != "WAITING_PAYMENT" {
		t.Errorf("forged webhook changed the order: %s", s)
	}

	// The real notification, delivered three times.
	for range 3 {
		if code := webhook(t, base, fake.Notification(p1)); code != http.StatusOK {
			t.Fatalf("webhook = %d", code)
		}
	}
	if n := queryID(t, db, "SELECT count(*) FROM payment_events WHERE kind = 'paid'"); n != 1 {
		t.Errorf("paid transitions = %d", n)
	}
	detail = b.get(detailPath)
	expect(t, detail, http.StatusOK, "Menunggu Konfirmasi", "Berhasil", "Transfer Bank (Virtual Account)", "Waktu Pembayaran")
	if strings.Contains(detail.body, "Bayar Sekarang") || strings.Contains(detail.body, "Batalkan Pesanan") {
		t.Error("a paid order offers no pay or cancel action")
	}
	expect(t, b.get(detailPath+"/payment/finish"), http.StatusOK, "Pembayaran Berhasil", number, "Menunggu Konfirmasi", "Lihat Pesanan", "Kembali Belanja")
	for _, body := range []string{detail.body, fin.body} {
		if strings.Contains(body, midtranstest.ServerKey) || strings.Contains(body, "tok-") {
			t.Error("pages must not contain the server key or the Snap token")
		}
	}
}

func TestPaymentFinishSyncsWithoutWebhook(t *testing.T) {
	base, db, fake := newPaymentApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	number := buyNow(t, b, db)
	b.post("/account/orders/"+number+"/pay", nil)

	// The notification never arrives (e.g. local development): the finish
	// page asks Midtrans and applies what Midtrans says.
	fake.SetStatus(number+"-P1", "expire", "bank_transfer", 129000)
	expect(t, b.get("/account/orders/"+number+"/payment/finish"), http.StatusOK, "Pembayaran Belum Berhasil", "Silakan coba kembali", "Bayar Lagi")
	pay := b.post("/account/orders/"+number+"/pay", nil)
	if pay.location != fake.URL+"/snap/v4/redirection/tok-"+number+"-P2" {
		t.Errorf("pay again must open attempt 2: %q", pay.location)
	}
	fake.SetStatus(number+"-P2", "settlement", "qris", 129000)
	expect(t, b.get("/account/orders/"+number+"/payment/finish"), http.StatusOK, "Pembayaran Berhasil", "QRIS")
}

func TestCancelAndPaymentSecurity(t *testing.T) {
	base, db, fake := newPaymentApp(t)
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	number := buyNow(t, b, db)
	path := "/account/orders/" + number
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	stockAfterOrder := queryID(t, db, "SELECT stock FROM product_variants WHERE id = $1", v)

	other := newBrowser(t, base)
	other.register("other@example.com")
	for _, res := range []response{other.post(path+"/pay", nil), other.post(path+"/cancel", nil), other.get(path + "/payment/finish"), other.get(path)} {
		if res.status != http.StatusNotFound {
			t.Errorf("another user's order: %d", res.status)
		}
	}
	if fake.Count("POST /snap") != 0 {
		t.Error("another user must not open a payment")
	}

	// Webhook input validation.
	fake.SetStatus("ASH-19990101-000001-P1", "settlement", "gopay", 1000)
	if code := webhook(t, base, fake.Notification("ASH-19990101-000001-P1")); code != http.StatusNotFound {
		t.Errorf("unknown transaction = %d", code)
	}
	if code := webhook(t, base, []byte("{not json")); code != http.StatusBadRequest {
		t.Errorf("malformed = %d", code)
	}
	if res := b.get("/webhooks/midtrans"); res.status != http.StatusMethodNotAllowed {
		t.Errorf("GET webhook = %d", res.status)
	}

	// Cancel: stock back, order closed; a second cancel is harmless.
	res := b.post(path+"/cancel", nil)
	if res.status != http.StatusSeeOther || res.location != path {
		t.Fatalf("cancel: %d %q", res.status, res.location)
	}
	expect(t, b.get(path), http.StatusOK, "Pesanan berhasil dibatalkan.", "Dibatalkan", "Pesanan dibatalkan oleh Anda.")
	if s := queryID(t, db, "SELECT stock FROM product_variants WHERE id = $1", v); s != stockAfterOrder+1 {
		t.Errorf("stock = %d, want %d", s, stockAfterOrder+1)
	}
	b.post(path+"/cancel", nil)
	if s := queryID(t, db, "SELECT stock FROM product_variants WHERE id = $1", v); s != stockAfterOrder+1 {
		t.Errorf("double cancel restored twice: %d", s)
	}

	// A cancelled order cannot be paid any more.
	res = b.post(path+"/pay", nil)
	if res.status != http.StatusSeeOther || res.location != path {
		t.Fatalf("pay cancelled: %d %q", res.status, res.location)
	}
	expect(t, b.get(path), http.StatusOK, "Pesanan ini tidak lagi menunggu pembayaran.")
}

func TestPaymentsDisabled(t *testing.T) {
	base, db := newApp(t) // no MIDTRANS_SERVER_KEY
	seedCatalog(t, db)
	b := newBrowser(t, base)
	b.register("siti@example.com")
	b.addAddress()
	number := buyNow(t, b, db)
	path := "/account/orders/" + number

	detail := b.get(path)
	expect(t, detail, http.StatusOK, "Pembayaran online belum tersedia.", "Batalkan Pesanan")
	if csp := detail.header.Get("Content-Security-Policy"); !strings.HasSuffix(csp, "form-action 'self'") {
		t.Errorf("CSP without payments: %q", csp)
	}
	b.post(path+"/pay", nil)
	expect(t, b.get(path), http.StatusOK, "Pembayaran online belum tersedia.")
	if code := webhook(t, base, []byte(`{"order_id":"x"}`)); code != http.StatusNotFound {
		t.Errorf("webhook without provider = %d", code)
	}
}

func queryString(t *testing.T, db *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}
