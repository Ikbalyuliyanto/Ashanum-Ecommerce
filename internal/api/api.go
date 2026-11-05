// Package api is the customer REST API (/api/v1) for mobile apps.
//
// It is a second transport over the same services the web uses — auth,
// user, address, category, product, cart, checkout, order, payment, review
// and returns — with no business rule of its own: handlers decode a strict
// JSON request, call one service method and encode an explicit DTO. Nothing
// here talks to the database directly.
//
// Authentication is a Bearer access token (auth.Service API sessions), never
// the web session cookie: the web session middleware skips /api/, and no
// cookie is read here. Because credentials are not sent automatically by
// browsers, the API is exempt from the web's same-origin (CSRF) check; CORS
// is closed unless API_CORS_ALLOWED_ORIGINS lists exact origins.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ecommerce/internal/address"
	"ecommerce/internal/auth"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/middleware"
	"ecommerce/internal/order"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/returns"
	"ecommerce/internal/review"
	"ecommerce/internal/user"
)

type Deps struct {
	Log        *slog.Logger
	Auth       *auth.Service
	Users      *user.Service
	Addresses  *address.Service
	Categories *category.Service
	Products   *product.Service
	Cart       *cart.Service
	Checkout   *checkout.Service
	Orders     *order.Service
	Payments   *payment.Service
	Reviews    *review.Service
	Returns    *returns.Service
	// CORSOrigins are exact browser origins allowed to call the API.
	CORSOrigins []string
}

type Handler struct{ Deps }

func NewHandler(d Deps) *Handler { return &Handler{Deps: d} }

// Prefix is where the router mounts Routes.
const Prefix = "/api/v1"

// Routes registers every /api/v1 endpoint on r (mounted at Prefix).
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.cors)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		failWith(w, http.StatusNotFound, "NOT_FOUND", "Endpoint tidak ditemukan.", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		failWith(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Metode tidak didukung untuk endpoint ini.", nil)
	})

	// Per client IP, in process memory (single instance; see docs/api-v1.md).
	tooMany := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failWith(w, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak percobaan. Coba lagi beberapa saat lagi.", nil)
	})
	r.With(middleware.RateLimit(10, 10*time.Minute, tooMany)).Post("/auth/register", h.register)
	r.With(middleware.RateLimit(20, 5*time.Minute, tooMany)).Post("/auth/login", h.login)
	r.With(middleware.RateLimit(60, 5*time.Minute, tooMany)).Post("/auth/refresh", h.refresh)
	r.With(middleware.RateLimit(60, 5*time.Minute, tooMany)).Post("/auth/logout", h.logout)

	r.Get("/categories", h.categories)
	r.Get("/home", h.home)
	r.Get("/products", h.products)
	r.Get("/products/{slug}", h.product)
	r.Get("/products/{slug}/reviews", h.productReviews)

	r.Group(func(r chi.Router) {
		r.Use(h.requireToken)
		r.Post("/auth/logout-all", h.logoutAll)

		r.Get("/me", h.me)
		r.Patch("/me/profile", h.updateProfile)
		r.Get("/me/addresses", h.addresses)
		r.Post("/me/addresses", h.createAddress)
		r.Patch("/me/addresses/{id}", h.updateAddress)
		r.Delete("/me/addresses/{id}", h.deleteAddress)
		r.Post("/me/addresses/{id}/primary", h.primaryAddress)

		r.Get("/cart", h.cart)
		r.Post("/cart/items", h.addCartItem)
		r.Patch("/cart/items/{id}", h.updateCartItem)
		r.Delete("/cart/items/{id}", h.deleteCartItem)
		r.Delete("/cart", h.clearCart)

		r.Post("/checkout/preview", h.checkoutPreview)
		r.Post("/orders", h.createOrder)
		r.Get("/orders", h.orders)
		r.Get("/orders/{number}", h.order)
		r.Post("/orders/{number}/cancel", h.cancelOrder)
		r.Post("/orders/{number}/payment", h.startPayment)
		r.Get("/orders/{number}/payment", h.paymentStatus)

		r.Get("/me/reviews", h.myReviews)
		r.Post("/reviews", h.createReview)
		r.Patch("/reviews/{id}", h.updateReview)

		r.Get("/me/returns", h.myReturns)
		r.Get("/me/returns/{number}", h.myReturn)
		r.Post("/returns", h.createReturn)
		r.Post("/me/returns/{number}/cancel", h.cancelReturn)
	})
}

type principalKey struct{}

// caller is the authenticated API user (set by requireToken only).
func caller(r *http.Request) auth.APIPrincipal {
	p, _ := r.Context().Value(principalKey{}).(auth.APIPrincipal)
	return p
}

func userID(r *http.Request) int64 { return caller(r).UserID }

// bearer returns the token of an "Authorization: Bearer <token>" header.
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// requireToken resolves the Bearer access token (one indexed query) and
// marks the response private. Any failure is 401 with a WWW-Authenticate
// challenge; TOKEN_EXPIRED tells the client to refresh.
func (h *Handler) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token := bearer(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			failWith(w, http.StatusUnauthorized, "UNAUTHORIZED", "Masuk terlebih dahulu (Authorization: Bearer).", nil)
			return
		}
		p, err := h.Auth.AuthenticateAPI(r.Context(), token)
		if err != nil {
			if isAuthError(err) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
			}
			h.fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// cors answers only listed origins (exact match, no wildcard, no
// credentials: the API uses Bearer tokens, not cookies). With no origins
// configured it adds nothing, and a browser on another origin is blocked.
func (h *Handler) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(h.CORSOrigins) > 0 {
			w.Header().Add("Vary", "Origin")
		}
		allowed := slices.Contains(h.CORSOrigins, origin)
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if !allowed {
			if preflight {
				failWith(w, http.StatusForbidden, "CORS_FORBIDDEN", "Origin ini tidak diizinkan.", nil)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "Retry-After, Location")
		if preflight {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsAPI reports whether a request path belongs to the API (the router uses
// it to answer JSON errors and to skip web-only middleware).
func IsAPI(path string) bool { return path == "/api" || strings.HasPrefix(path, "/api/") }

// InternalError writes the JSON 500 body (panic recovery for API paths).
func InternalError(w http.ResponseWriter) {
	failWith(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan pada server.", nil)
}

// NotFound writes the JSON 404 body (unknown /api paths outside v1).
func NotFound(w http.ResponseWriter) {
	failWith(w, http.StatusNotFound, "NOT_FOUND", "Endpoint tidak ditemukan.", nil)
}
