package middleware

import (
	"net"
	"net/http"
)

// contentSecurityPolicy allows only same-origin scripts and styles, so
// templates must not use inline <script>, inline style="" or hx-on.
// Later phases extend it for external services (e.g. Midtrans Snap).
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data: https://res.cloudinary.com; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// SecureHeaders sets browser security headers on every response. HSTS is only
// sent in production, where the site is always reached over HTTPS.
func SecureHeaders(production bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", contentSecurityPolicy)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			if production {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RealIP replaces r.RemoteAddr with the X-Real-IP header set by Nginx.
//
// The header is trusted only when the TCP peer is loopback, i.e. the request
// came through the local Nginx. The app listens on 127.0.0.1 in production,
// so clients on the internet cannot spoof it.
func RealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if peer := net.ParseIP(hostOnly(r.RemoteAddr)); peer != nil && peer.IsLoopback() {
			if ip := net.ParseIP(r.Header.Get("X-Real-IP")); ip != nil {
				r.RemoteAddr = ip.String()
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the client address without the port.
func ClientIP(r *http.Request) string { return hostOnly(r.RemoteAddr) }

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
