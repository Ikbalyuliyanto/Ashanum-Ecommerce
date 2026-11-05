package biteship_test

import (
	"os"
	"strings"
	"testing"

	"ecommerce/internal/shipping"
	"ecommerce/internal/shipping/biteship"
)

// TestRealBiteshipTesting talks to the real Biteship API with a testing
// key, read-only: area search and rates. It never books a shipment (no
// order is created, nothing is paid). Skipped without
// BITESHIP_TEST_API_KEY (biteship_test.*); BITESHIP_TEST_ORIGIN_AREA_ID
// can set the origin (default: the first result for "Bandung Wetan").
func TestRealBiteshipTesting(t *testing.T) {
	key := os.Getenv("BITESHIP_TEST_API_KEY")
	if key == "" {
		t.Skip("BITESHIP_TEST_API_KEY not set: real Biteship testing API not called")
	}
	if !strings.HasPrefix(key, "biteship_test.") {
		t.Fatal("BITESHIP_TEST_API_KEY must be a testing key (biteship_test.…); live keys are never used by tests")
	}
	c, err := biteship.New(biteship.Config{Env: biteship.EnvTest, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := c.SearchAreas(ctx, "Coblong")
	if err != nil || len(dest) == 0 {
		t.Fatalf("area search: %v (%d areas)", err, len(dest))
	}
	t.Logf("area: %s (%s)", dest[0].Label, dest[0].ID)
	origin := os.Getenv("BITESHIP_TEST_ORIGIN_AREA_ID")
	if origin == "" {
		o, err := c.SearchAreas(ctx, "Bandung Wetan")
		if err != nil || len(o) == 0 {
			t.Fatalf("origin search: %v", err)
		}
		origin = o[0].ID
	}
	rates, err := c.Rates(ctx, shipping.RateRequest{Origin: shipping.Location{AreaID: origin},
		Destination: shipping.Location{AreaID: dest[0].ID}, Package: pkg(500, 1), Couriers: []string{"jne", "sicepat", "jnt"}})
	if err != nil {
		t.Fatalf("rates: %v", err)
	}
	for _, r := range rates {
		if r.Price <= 0 || r.CourierCode == "" || r.ServiceCode == "" {
			t.Errorf("incomplete rate: %+v", r)
		}
		t.Logf("%s %s: Rp%d, %s", r.CourierName, r.ServiceName, r.Price, r.EstimateLabel())
	}
}
