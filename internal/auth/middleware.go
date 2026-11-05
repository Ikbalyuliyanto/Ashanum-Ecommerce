package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// LoadSession attaches the signed-in user (if any) to the request context.
// Static assets and health checks are skipped so they never touch the
// database. A database error is logged and the request continues as a
// guest; protected routes then send the user to the login page.
func (h *Handler) LoadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The REST API never reads the cookie: its credential is a Bearer
		// token (see package api), so the two session surfaces stay apart.
		if p := r.URL.Path; strings.HasPrefix(p, "/static/") || p == "/healthz" || p == "/readyz" ||
			p == "/api" || strings.HasPrefix(p, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(h.cookie.Name)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		p, renewed, err := h.svc.Authenticate(r.Context(), c.Value)
		switch {
		case errors.Is(err, ErrSessionNotFound):
			h.clearSessionCookie(w)
		case err != nil:
			h.log.Error("load session", "error", err)
		default:
			if renewed != nil {
				h.setSessionCookie(w, *renewed)
			}
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser sends guests to the login page (remembering where they were
// going) and marks the response as private: account pages must never be
// stored by a shared cache or the browser's back-forward cache.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			target := r.URL.RequestURI()
			if r.Method != http.MethodGet {
				// A form post (e.g. add to cart) returns to the page it was
				// sent from. CrossOriginProtection already guarantees the
				// request is same-origin; safeNext re-checks at login.
				target = "/account"
				if u, err := url.Parse(r.Referer()); err == nil && u.Host == r.Host && u.Path != "" {
					target = u.RequestURI()
				}
			}
			if r.Header.Get("HX-Request") == "true" {
				if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && u.Path != "" {
					target = u.RequestURI()
				}
				w.Header().Set("HX-Redirect", "/login?next="+url.QueryEscape(target))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(target), http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next.ServeHTTP(w, r)
	})
}

// RedirectIfSignedIn keeps signed-in users away from login/register.
func RedirectIfSignedIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); ok {
			http.Redirect(w, r, "/account", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CurrentUser exposes the signed-in user to templates (view.Config hook).
func CurrentUser(r *http.Request) any {
	if p, ok := FromContext(r.Context()); ok {
		return p
	}
	return nil
}

// RequireAdmin lets only signed-in admins through. Guests are sent to the
// login page (as RequireUser does); any other signed-in user gets deny,
// which should answer as if the admin area did not exist. The role is read
// from the database on every request (ActiveSession), so a demoted admin
// loses access at once.
func RequireAdmin(deny http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, _ := FromContext(r.Context()); !p.IsAdmin() {
				deny.ServeHTTP(w, r)
				return
			}
			w.Header().Set("X-Robots-Tag", "noindex, nofollow")
			next.ServeHTTP(w, r)
		}))
	}
}
