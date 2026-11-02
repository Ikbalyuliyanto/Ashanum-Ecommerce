package middleware

import (
	"net/http"
	"net/http/httptest"
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
