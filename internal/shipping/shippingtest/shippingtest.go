// Package shippingtest gives tests a shipping service backed by the fake
// Biteship API (biteshiptest) through the real Biteship client, plus the
// matching configuration for routers built from config. Never used by the
// application binary.
package shippingtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/config"
	"ecommerce/internal/shipping"
	"ecommerce/internal/shipping/biteship"
	"ecommerce/internal/shipping/biteship/biteshiptest"
)

// Origin is the shop address used in tests.
var Origin = shipping.Origin{Name: "Toko Ashanum", Phone: "081200000001", Address: "Jl. Asia Afrika No. 8, Bandung",
	PostalCode: "40111", AreaID: "IDNP9IDNC22IDND313IDZ40115"}

// Couriers asked in tests: JNE only, so prices are predictable (REG Rp9.000
// per started kg, YES Rp18.000 + Rp15.000 per extra kg).
var Couriers = []string{"jne"}

// Env is a fake Biteship server, a client for it and a service using it.
type Env struct {
	Fake    *biteshiptest.Server
	Client  *biteship.Client
	Service *shipping.Service
}

// New starts a fake and builds a service on db.
func New(t testing.TB, db *pgxpool.Pool) *Env {
	t.Helper()
	fake := biteshiptest.New(t)
	c := Client(t, fake.URL)
	return &Env{Fake: fake, Client: c, Service: shipping.NewService(db, c, shipping.Options{Origin: Origin, Couriers: Couriers})}
}

// Client is a Biteship client for the fake at baseURL (short timeout).
func Client(t testing.TB, baseURL string) *biteship.Client {
	t.Helper()
	httpClient := biteship.NewHTTPClient()
	httpClient.Timeout = 2 * time.Second
	c, err := biteship.New(biteship.Config{Env: biteship.EnvTest, APIKey: biteshiptest.APIKey, BaseURL: baseURL,
		WebhookHeader: biteshiptest.WebhookHeader, WebhookSecret: biteshiptest.WebhookSecret, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Config is the shipping configuration matching Origin and Couriers.
func Config(baseURL string) config.Shipping {
	return config.Shipping{Provider: config.ShippingBiteship, Couriers: Couriers,
		RateCacheTTL: 10 * time.Minute, QuoteTTL: 30 * time.Minute,
		Biteship: config.Biteship{Env: config.BiteshipTest, APIKey: biteshiptest.APIKey, BaseURL: baseURL,
			WebhookHeader: biteshiptest.WebhookHeader, WebhookSecret: biteshiptest.WebhookSecret},
		Origin: config.ShipFrom{Name: Origin.Name, Phone: Origin.Phone, Address: Origin.Address,
			PostalCode: Origin.PostalCode, AreaID: Origin.AreaID}}
}

// Area searches the fake (as the address form does), which caches the
// areas in the database, and returns the first match.
func (e *Env) Area(t testing.TB, query string) shipping.Area {
	t.Helper()
	areas, err := e.Service.SearchAreas(context.Background(), query)
	if err != nil || len(areas) == 0 {
		t.Fatalf("search %q: %v (%d areas)", query, err, len(areas))
	}
	return areas[0]
}

// AreaID is the cached id of the Coblong (Bandung, 40132) area.
func (e *Env) AreaID(t testing.TB) string { return e.Area(t, "Coblong").ID }

var (
	mu   sync.Mutex
	envs = map[*pgxpool.Pool]*Env{}
)

// For returns the Env of db, creating it once (routers and the tests that
// drive them share one fake per database).
func For(t testing.TB, db *pgxpool.Pool) *Env {
	mu.Lock()
	defer mu.Unlock()
	if e, ok := envs[db]; ok {
		return e
	}
	e := New(t, db)
	envs[db] = e
	t.Cleanup(func() {
		mu.Lock()
		delete(envs, db)
		mu.Unlock()
	})
	return e
}
