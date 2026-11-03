package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRealIPTrustsOnlyLoopback(t *testing.T) {
	var seen string
	probe := RealIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))

	for _, tc := range []struct{ peer, header, want string }{
		{"127.0.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{"[::1]:5000", "2001:db8::1", "2001:db8::1"},
		{"198.51.100.7:5000", "203.0.113.9", "198.51.100.7:5000"}, // not via Nginx: ignore header
		{"127.0.0.1:5000", "not-an-ip", "127.0.0.1:5000"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tc.peer
		req.Header.Set("X-Real-IP", tc.header)
		probe.ServeHTTP(httptest.NewRecorder(), req)
		if seen != tc.want {
			t.Errorf("peer %s header %s: got %s, want %s", tc.peer, tc.header, seen, tc.want)
		}
	}
}

func TestCSPFormActionOrigins(t *testing.T) {
	csp := func(origins ...string) string {
		rec := httptest.NewRecorder()
		SecureHeaders(false, origins...)(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Header().Get("Content-Security-Policy")
	}
	base := csp()
	if !strings.HasSuffix(base, "form-action 'self'") {
		t.Fatalf("default form-action must be 'self' only: %q", base)
	}
	got := csp("https://app.sandbox.midtrans.com", "*", "https://evil.example; script-src *", "")
	if got != base+" https://app.sandbox.midtrans.com" {
		t.Errorf("only the checkout origin may be added to form-action: %q", got)
	}
	for _, d := range []string{"script-src 'self';", "connect-src 'self';", "default-src 'self';"} {
		if !strings.Contains(got, d) {
			t.Errorf("%s must stay unchanged: %q", d, got)
		}
	}
	if strings.Contains(got, "unsafe-") || strings.Contains(got, "frame-src") {
		t.Errorf("CSP must not be relaxed: %q", got)
	}
}
