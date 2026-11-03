package midtrans_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ecommerce/internal/payment"
	"ecommerce/internal/payment/midtrans"
	"ecommerce/internal/payment/midtrans/midtranstest"
)

func newClient(t *testing.T, fake *midtranstest.Server) *midtrans.Client {
	t.Helper()
	c, err := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: midtranstest.ServerKey, SnapURL: fake.URL, APIURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewIsSandboxOnly(t *testing.T) {
	if _, err := midtrans.New(midtrans.Config{Env: "production", ServerKey: "Mid-server-x"}); err == nil {
		t.Error("production must be refused")
	}
	if _, err := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox}); err == nil {
		t.Error("an empty server key must be refused")
	}
	c, err := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: midtranstest.ServerKey})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.CheckoutOrigin(); got != "https://app.sandbox.midtrans.com" {
		t.Errorf("checkout origin = %q", got)
	}
}

func TestSnapRequestSerialization(t *testing.T) {
	var gotAuth, gotType string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/snap/v1/transactions" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"abc","redirect_url":"https://app.sandbox.midtrans.com/snap/v4/redirection/abc"}`))
	}))
	defer srv.Close()
	c, _ := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: midtranstest.ServerKey, SnapURL: srv.URL})

	wib := time.FixedZone("WIB", 7*3600)
	start := time.Date(2026, 11, 3, 10, 15, 0, 0, wib)
	co, err := c.CreateTransaction(context.Background(), payment.TransactionRequest{
		ProviderOrderID: "ASH-20261103-000123-P1",
		Amount:          259000,
		Items: []payment.LineItem{
			{ID: "ASH-BLK-M", Name: "Kaos Ashanum Basic Lengan Panjang Katun Combed Premium - Hitam / M", Price: 129000, Quantity: 1},
			{ID: "TNK-02", Name: "Tunik Polos 02", Price: 65000, Quantity: 2},
		},
		Customer:  payment.Customer{Name: "Siti Rahmawati", Email: "siti@example.com", Phone: "+6281298765432"},
		Start:     start,
		ExpiresAt: start.Add(1439 * time.Minute),
		FinishURL: "https://toko.example/account/orders/ASH-20261103-000123/payment/finish",
	})
	if err != nil {
		t.Fatal(err)
	}
	if co.Token != "abc" || !strings.HasSuffix(co.RedirectURL, "/abc") {
		t.Errorf("checkout = %+v", co)
	}
	if gotAuth != midtranstest.BasicAuth() || gotType != "application/json" {
		t.Errorf("headers: auth %q, content-type %q", gotAuth, gotType)
	}

	td := body["transaction_details"].(map[string]any)
	if td["order_id"] != "ASH-20261103-000123-P1" || td["gross_amount"] != float64(259000) {
		t.Errorf("transaction_details = %v", td)
	}
	items := body["item_details"].([]any)
	first := items[0].(map[string]any)
	if len(items) != 2 || len([]rune(first["name"].(string))) != 50 || first["price"] != float64(129000) || first["id"] != "ASH-BLK-M" {
		t.Errorf("item_details = %v", items)
	}
	exp := body["expiry"].(map[string]any)
	if exp["start_time"] != "2026-11-03 10:15:00 +0700" || exp["unit"] != "minute" || exp["duration"] != float64(1439) {
		t.Errorf("expiry = %v", exp)
	}
	if cb := body["callbacks"].(map[string]any); cb["finish"] != "https://toko.example/account/orders/ASH-20261103-000123/payment/finish" {
		t.Errorf("callbacks = %v", cb)
	}
	if cd := body["customer_details"].(map[string]any); cd["email"] != "siti@example.com" || cd["first_name"] != "Siti Rahmawati" {
		t.Errorf("customer_details = %v", cd)
	}
	if cc := body["credit_card"].(map[string]any); cc["secure"] != true {
		t.Errorf("credit_card = %v", cc)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), midtranstest.ServerKey) {
		t.Error("the server key must never be in a request body")
	}
}

func TestCreateTransactionErrors(t *testing.T) {
	fake := midtranstest.New(t)
	c := newClient(t, fake)
	start := time.Now()
	req := payment.TransactionRequest{ProviderOrderID: "ASH-1-P1", Amount: 10000, Start: start, ExpiresAt: start.Add(time.Hour),
		Items: []payment.LineItem{{ID: "A", Name: "A", Price: 10000, Quantity: 1}}}

	fake.FailNext("/snap", 1)
	if _, err := c.CreateTransaction(context.Background(), req); !errors.Is(err, payment.ErrProviderUnavailable) {
		t.Errorf("5xx: %v", err)
	}
	if _, err := c.CreateTransaction(context.Background(), req); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if _, err := c.CreateTransaction(context.Background(), req); !errors.Is(err, payment.ErrProviderRejected) {
		t.Errorf("reused order_id must be rejected: %v", err)
	}

	bad := req
	bad.ProviderOrderID, bad.Amount = "ASH-2-P1", 99999
	calls := fake.Count("POST /snap")
	if _, err := c.CreateTransaction(context.Background(), bad); err == nil || fake.Count("POST /snap") != calls {
		t.Errorf("items not adding up to the amount must fail before calling Midtrans: %v", err)
	}

	wrongKey, _ := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: "SB-Mid-server-WRONG", SnapURL: fake.URL, APIURL: fake.URL})
	req.ProviderOrderID = "ASH-3-P1"
	_, err := wrongKey.CreateTransaction(context.Background(), req)
	if !errors.Is(err, payment.ErrProviderRejected) || strings.Contains(err.Error(), "WRONG") {
		t.Errorf("wrong key: %v (must not contain the key)", err)
	}

	fake.Close()
	if _, err := c.GetStatus(context.Background(), "ASH-1-P1"); !errors.Is(err, payment.ErrProviderUnavailable) {
		t.Errorf("unreachable provider: %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	fake := midtranstest.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	if st, err := c.GetStatus(ctx, "ASH-NONE-P1"); err != nil || st.Found {
		t.Errorf("unknown transaction must be Found=false: %+v %v", st, err)
	}

	cases := []struct {
		status, fraud string
		want          payment.Status
	}{
		{"settlement", "", payment.StatusPaid},
		{"capture", "accept", payment.StatusPaid},
		{"capture", "challenge", payment.StatusPending},
		{"capture", "deny", payment.StatusDenied},
		{"pending", "", payment.StatusPending},
		{"deny", "", payment.StatusDenied},
		{"cancel", "", payment.StatusCancelled},
		{"expire", "", payment.StatusExpired},
		{"failure", "", payment.StatusFailed},
		{"refund", "", ""},
		{"chargeback", "", ""},
	}
	for _, tc := range cases {
		id := "ASH-" + tc.status + tc.fraud + "-P1"
		fake.SetStatus(id, tc.status, "bank_transfer", 649000)
		if tc.fraud != "" {
			fake.SetFraud(id, tc.fraud)
		}
		st, err := c.GetStatus(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if !st.Found || st.Status != tc.want || st.Raw != tc.status || st.Amount != 649000 || st.TransactionID != "trx-"+id {
			t.Errorf("%s/%s → %+v, want %q", tc.status, tc.fraud, st, tc.want)
		}
		if tc.want == payment.StatusPaid && st.PaidAt == nil {
			t.Errorf("%s: paid time missing", tc.status)
		}
	}
	if st, _ := c.GetStatus(ctx, "ASH-settlement-P1"); st.PaidAt.Format(time.RFC3339) != "2026-11-03T10:21:30+07:00" {
		t.Errorf("settlement time = %v", st.PaidAt)
	}
	if got := c.MethodLabel("bank_transfer"); got != "Transfer Bank (Virtual Account)" {
		t.Errorf("method label = %q", got)
	}
}

func TestNotificationSignature(t *testing.T) {
	fake := midtranstest.New(t)
	c := newClient(t, fake)
	fake.SetStatus("ASH-1-P1", "settlement", "qris", 120000)

	body := fake.Notification("ASH-1-P1")
	n, err := c.ParseNotification(body)
	if err != nil || n.ProviderOrderID != "ASH-1-P1" || n.Raw != "settlement" {
		t.Fatalf("valid notification: %+v %v", n, err)
	}

	var m map[string]any
	json.Unmarshal(body, &m)
	m["signature_key"] = strings.ToUpper(m["signature_key"].(string))
	upper, _ := json.Marshal(m)
	if _, err := c.ParseNotification(upper); err != nil {
		t.Errorf("hex case must not matter: %v", err)
	}

	m["gross_amount"] = "1.00" // amount changed after signing
	tampered, _ := json.Marshal(m)
	if _, err := c.ParseNotification(tampered); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("tampered amount: %v", err)
	}
	m["gross_amount"], m["signature_key"] = "120000.00", strings.Repeat("0", 128)
	forged, _ := json.Marshal(m)
	if _, err := c.ParseNotification(forged); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("forged signature: %v", err)
	}
	for _, bad := range []string{`not json`, `{}`, `{"order_id":"x","status_code":"200","gross_amount":"1.00"}`} {
		if _, err := c.ParseNotification([]byte(bad)); !errors.Is(err, payment.ErrInvalidNotification) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestCancelFallsBackToExpire(t *testing.T) {
	fake := midtranstest.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	// A pending bank transfer cannot be cancelled, only expired.
	fake.SetStatus("ASH-VA-P1", "pending", "bank_transfer", 50000)
	st, err := c.Cancel(ctx, "ASH-VA-P1")
	if err != nil || st.Status != payment.StatusExpired || fake.Count("POST /v2/ASH-VA-P1/expire") != 1 {
		t.Errorf("pending VA: %+v %v", st, err)
	}

	// A pending card payment is cancelled directly.
	fake.SetStatus("ASH-CC-P1", "pending", "credit_card", 50000)
	if st, err := c.Cancel(ctx, "ASH-CC-P1"); err != nil || st.Status != payment.StatusCancelled || fake.Count("POST /v2/ASH-CC-P1/expire") != 0 {
		t.Errorf("pending card: %+v %v", st, err)
	}

	// A settled payment stays paid: the caller must not cancel the order.
	fake.SetStatus("ASH-PAID-P1", "settlement", "gopay", 50000)
	if st, err := c.Cancel(ctx, "ASH-PAID-P1"); err != nil || st.Status != payment.StatusPaid {
		t.Errorf("settled: %+v %v", st, err)
	}

	// Checkout opened but never used: no transaction at Midtrans.
	if st, err := c.Cancel(ctx, "ASH-UNUSED-P1"); err != nil || st.Found {
		t.Errorf("unused checkout: %+v %v", st, err)
	}
}
