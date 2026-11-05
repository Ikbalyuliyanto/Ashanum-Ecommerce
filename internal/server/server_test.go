package server

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/auth"
	"ecommerce/internal/config"
	"ecommerce/internal/payment"
	"ecommerce/internal/view"
	"ecommerce/web"
)

// newTestRouter builds the production router (embedded templates, production
// headers) without a database: only routes that never query it work.
func newTestRouter(t *testing.T) http.Handler {
	return buildRouter(t, nil, config.EnvProduction)
}

// buildRouter uses the embedded templates/static, exactly like production.
func buildRouter(t *testing.T, db *pgxpool.Pool, env string) http.Handler {
	return buildRouterWith(t, db, env, nil, "")
}

// buildRouterWith also sets the payment provider (nil: payments disabled)
// and its checkout origin.
func buildRouterWith(t *testing.T, db *pgxpool.Pool, env string, provider payment.Provider, checkoutOrigin string) http.Handler {
	t.Helper()
	return buildRouterCfg(t, db, env, provider, checkoutOrigin, nil)
}

// buildRouterCfg is buildRouterWith with a hook to adjust the configuration
// (API settings in the API tests).
func buildRouterCfg(t *testing.T, db *pgxpool.Pool, env string, provider payment.Provider, checkoutOrigin string, mod func(*config.Config)) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tpl, _ := fs.Sub(web.FS, "templates")
	static, _ := fs.Sub(web.FS, "static")

	assets, err := view.NewAssets(static, "/static/", false)
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := view.New(view.Config{
		FS:          tpl,
		Assets:      assets,
		App:         view.App{Name: "Toko Uji", Dev: env != config.EnvProduction},
		Log:         log,
		CurrentUser: auth.CurrentUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Env: env, Session: config.Session{Secret: []byte("0123456789abcdef0123456789abcdef")},
		Midtrans: config.Midtrans{Env: config.MidtransSandbox}}
	if mod != nil {
		mod(cfg)
	}
	return NewRouter(Deps{
		Config:          cfg,
		Log:             log,
		DB:              db,
		View:            renderer,
		Assets:          assets,
		PaymentProvider: provider,
		CheckoutOrigin:  checkoutOrigin,
	})
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestBaseLayoutAndHeaders checks the shared layout and security headers on a
// page that needs no database (the home page is covered with a database in
// TestStorefrontPages).
func TestBaseLayoutAndHeaders(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodGet, "/login", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "ASHANUM", `action="/search"`, "/static/css/app.css?v=", "/static/js/htmx.min.js?v="} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Strict-Transport-Security"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestNotFoundRendersErrorPage(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodGet, "/tidak-ada", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Halaman tidak ditemukan") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestStaticAssets(t *testing.T) {
	h := newTestRouter(t)

	rec := do(h, httptest.NewRequest(http.MethodGet, "/static/css/app.css?v=x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("css status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("cache-control = %q", cc)
	}

	etag := rec.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "/static/css/app.css?v=x", nil)
	req.Header.Set("If-None-Match", etag)
	if rec := do(h, req); rec.Code != http.StatusNotModified {
		t.Errorf("conditional request status = %d, want 304", rec.Code)
	}

	for _, p := range []string{"/static/css/", "/static/", "/static/../go.mod", "/static/nope.css"} {
		if rec := do(h, httptest.NewRequest(http.MethodGet, p, nil)); rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", p, rec.Code)
		}
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(newTestRouter(t), req); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
