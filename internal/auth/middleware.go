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
		if p := r.URL.Path; strings.HasPrefix(p, "/static/") || p == "/healthz" || p == "/readyz" {
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
				target = "/account"
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
