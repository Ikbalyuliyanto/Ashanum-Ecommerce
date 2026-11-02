package server

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ecommerce/internal/config"
	"ecommerce/internal/view"
	"ecommerce/web"
)

// newTestRouter uses the embedded templates/static, exactly like production.
// The DB is nil, so /readyz is not exercised here.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tpl, _ := fs.Sub(web.FS, "templates")
	static, _ := fs.Sub(web.FS, "static")

	assets, err := view.NewAssets(static, "/static/", false)
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := view.New(tpl, assets, view.App{Name: "Toko Uji"}, log)
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(Deps{
		Config: &config.Config{Env: config.EnvProduction},
		Log:    log,
		View:   renderer,
		Assets: assets,
	})
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHomePage(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "Toko Uji", "/static/css/app.css?v=", "/static/js/htmx.min.js?v="} {
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
