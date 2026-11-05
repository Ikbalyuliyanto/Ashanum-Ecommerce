package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ecommerce/internal/config"
	"ecommerce/internal/testutil"
)

// TestRoundTrips pins the database round trips of the two pages Phase 7
// made heavier. A completed order detail reads the order (with payment flag
// and shipment), its items (with review and returned quantity), its returns
// and its payment attempts; a product page with reviews reads one query for
// the review page and its star distribution. Session and cart badge are one
// query each on every signed-in page.
func TestRoundTrips(t *testing.T) {
	db, counter := testutil.CountingDB(t)
	srv := httptest.NewServer(buildRouter(t, db, config.EnvDevelopment))
	t.Cleanup(srv.Close)
	seedCatalog(t, db)
	b := newBrowser(t, srv.URL)
	b.register("siti@example.com")
	b.addAddress()
	number := buyNow(t, b, db)
	testutil.CompleteOrder(t, db, number, time.Now())
	item := queryID(t, db, "SELECT i.id FROM order_items i JOIN orders o ON o.id = i.order_id WHERE o.order_number = $1", number)
	if res := b.post("/account/reviews", url.Values{"item": {fmt.Sprint(item)}, "rating": {"5"}, "comment": {"Bagus"}}); res.status != http.StatusSeeOther {
		t.Fatalf("review: %d", res.status)
	}

	measure := func(path string) int64 {
		t.Helper()
		b.get(path) // warm caches (categories)
		counter.Reset()
		expect(t, b.get(path), http.StatusOK)
		return counter.Count()
	}
	const signedIn = 2 // session + cart badge
	if n := measure("/account/orders/" + number); n != signedIn+4 {
		t.Errorf("completed order detail: %d queries, want %d", n, signedIn+4)
	}
	if n := measure("/products/kaos-ashanum-basic"); n != signedIn+3 {
		t.Errorf("product page with reviews: %d queries, want %d", n, signedIn+3)
	}
}

// TestAPIRoundTrips pins the database round trips of the common API
// endpoints: resolving the Bearer token is one indexed query, and each
// resource below is one more.
func TestAPIRoundTrips(t *testing.T) {
	db, counter := testutil.CountingDB(t)
	srv := httptest.NewServer(buildRouter(t, db, config.EnvDevelopment))
	t.Cleanup(srv.Close)
	seedCatalog(t, db)
	a := newAPI(t, srv.URL)
	a.signIn("a@example.com")
	v := queryID(t, db, "SELECT id FROM product_variants WHERE sku = 'ASH-BLK-M'")
	a.post("/cart/items", map[string]any{"variant_id": v, "quantity": 1})

	measure := func(path string) int64 {
		t.Helper()
		a.get(path) // warm caches (categories)
		counter.Reset()
		if res := a.get(path); res.status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, res.status, res.raw)
		}
		return counter.Count()
	}
	const token = 1 // Bearer token → session + user
	for path, want := range map[string]int64{
		"/products":                    1, // public: no token lookup
		"/products/kaos-ashanum-basic": 1,
		"/categories":                  0, // in-memory cache
		"/cart":                        token + 1,
		"/orders":                      token + 1,
		"/me":                          token + 1,
	} {
		if strings.HasPrefix(path, "/products") || path == "/categories" {
			saved := a.token
			a.token = ""
			got := measure(path)
			a.token = saved
			if got != want {
				t.Errorf("%s: %d queries, want %d", path, got, want)
			}
			continue
		}
		if got := measure(path); got != want {
			t.Errorf("%s: %d queries, want %d", path, got, want)
		}
	}
}
