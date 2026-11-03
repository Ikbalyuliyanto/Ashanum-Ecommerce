package midtrans_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ecommerce/internal/payment"
	"ecommerce/internal/payment/midtrans"
)

// TestSandboxIntegration calls the real Midtrans sandbox. It is skipped
// unless a sandbox server key is given in its own variable (never the app
// configuration), so the normal test run needs no internet:
//
//	MIDTRANS_SANDBOX_SERVER_KEY=SB-Mid-server-... go test -run Sandbox -v ./internal/payment/midtrans/
func TestSandboxIntegration(t *testing.T) {
	key := os.Getenv("MIDTRANS_SANDBOX_SERVER_KEY")
	if key == "" {
		t.Skip("MIDTRANS_SANDBOX_SERVER_KEY not set; skipping the Midtrans sandbox test")
	}
	if !strings.HasPrefix(key, "SB-Mid-server-") {
		t.Fatal("MIDTRANS_SANDBOX_SERVER_KEY must be a sandbox key (SB-Mid-server-...)")
	}
	c, err := midtrans.New(midtrans.Config{Env: midtrans.EnvSandbox, ServerKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := fmt.Sprintf("ASH-TEST-%d-P1", time.Now().UnixNano())
	start := time.Now()
	co, err := c.CreateTransaction(ctx, payment.TransactionRequest{
		ProviderOrderID: id,
		Amount:          10000,
		Items:           []payment.LineItem{{ID: "TEST-1", Name: "Uji Sandbox Ashanum", Price: 10000, Quantity: 1}},
		Customer:        payment.Customer{Name: "Uji Sandbox", Email: "uji@example.com"},
		Start:           start,
		ExpiresAt:       start.Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create Snap transaction: %v", err)
	}
	if co.Token == "" || !strings.HasPrefix(co.RedirectURL, "https://app.sandbox.midtrans.com/") {
		t.Fatalf("unexpected checkout %+v", co)
	}

	// No method chosen yet: Midtrans has no transaction for the ID.
	st, err := c.GetStatus(ctx, id)
	if err != nil || st.Found {
		t.Errorf("status before payment: %+v %v", st, err)
	}
	if st, err = c.Cancel(ctx, id); err != nil || (st.Found && st.Status == payment.StatusPaid) {
		t.Errorf("cancel unused checkout: %+v %v", st, err)
	}
	t.Logf("sandbox checkout created for %s (page %s)", id, c.CheckoutOrigin())
}
