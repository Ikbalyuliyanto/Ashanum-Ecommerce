package middleware

import "net/http"

// CSRF rejects cross-origin state-changing requests (POST, PUT, PATCH,
// DELETE) using the browser's Sec-Fetch-Site / Origin headers, via the
// standard library's http.CrossOriginProtection. No tokens or session storage
// are needed, and it works unchanged for HTMX requests.
//
// Server-to-server calls such as payment webhooks send neither header and are
// therefore allowed; they must authenticate themselves (e.g. signature check).
//
// bypass lists ServeMux patterns exempt from the check. Only routes that
// never authenticate with cookies may be listed: the REST API, whose
// credential is an explicit Bearer header a browser never attaches by
// itself, so a cross-site page cannot ride on it.
func CSRF(onDeny http.Handler, bypass ...string) func(http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	if onDeny != nil {
		cop.SetDenyHandler(onDeny)
	}
	for _, p := range bypass {
		cop.AddInsecureBypassPattern(p)
	}
	return cop.Handler
}
