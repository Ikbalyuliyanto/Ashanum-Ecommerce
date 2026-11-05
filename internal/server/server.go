// Package server wires middleware and module routes into one HTTP handler
// and runs it behind Nginx.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/admin"
	"ecommerce/internal/api"
	"ecommerce/internal/audit"
	"ecommerce/internal/auth"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/config"
	"ecommerce/internal/fulfillment"
	"ecommerce/internal/home"
	"ecommerce/internal/inventory"
	"ecommerce/internal/middleware"
	"ecommerce/internal/order"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
	"ecommerce/internal/report"
	"ecommerce/internal/returns"
	"ecommerce/internal/review"
	"ecommerce/internal/shipping"
	"ecommerce/internal/user"
	"ecommerce/internal/view"
)

type Deps struct {
	Config *config.Config
	Log    *slog.Logger
	DB     *pgxpool.Pool
	View   *view.Renderer
	Assets *view.Assets

	// PaymentProvider is nil when online payment is not configured.
	PaymentProvider payment.Provider
	// CheckoutOrigin is the provider's hosted checkout origin, allowed in
	// the CSP form-action (the pay form redirects there).
	CheckoutOrigin string

	// ShippingProvider is nil when no shipping provider is configured
	// (checkout then cannot place orders: no shipping cost 0 fallback).
	ShippingProvider shipping.Provider
}

// ShippingOptions maps the configuration to the shipping service options.
func ShippingOptions(cfg *config.Config, log *slog.Logger) shipping.Options {
	o := cfg.Shipping.Origin
	origin := shipping.Origin{Name: o.Name, Phone: o.Phone, Address: o.Address, PostalCode: o.PostalCode, AreaID: o.AreaID}
	if lat, err := strconv.ParseFloat(o.Latitude, 64); err == nil {
		if lng, err := strconv.ParseFloat(o.Longitude, 64); err == nil {
			origin.Latitude, origin.Longitude = &lat, &lng
		}
	}
	return shipping.Options{Origin: origin, Couriers: cfg.Shipping.Couriers, RateTTL: cfg.Shipping.RateCacheTTL,
		QuoteTTL: cfg.Shipping.QuoteTTL, Log: log}
}

// NewRouter is the composition root: it builds repositories, services and
// handlers, then registers each module's routes through its Routes method.
func NewRouter(d Deps) http.Handler {
	authSvc := auth.NewService(auth.NewRepository(d.DB), d.Config.Session.Secret)
	authSvc.ConfigureAPI(d.Config.API.AccessTTL, d.Config.API.RefreshTTL)
	userSvc := user.NewService(user.NewRepository(d.DB))
	shippingSvc := shipping.NewService(d.DB, d.ShippingProvider, ShippingOptions(d.Config, d.Log))
	addressSvc := address.NewService(address.NewRepository(d.DB), shippingSvc)
	categorySvc := category.NewService(category.NewRepository(d.DB))
	productSvc := product.NewService(product.NewRepository(d.DB))
	cartSvc := cart.NewService(cart.NewRepository(d.DB))
	paymentSvc := payment.NewService(payment.NewRepository(d.DB), d.PaymentProvider, payment.Options{
		BaseURL:      d.Config.BaseURL,
		AppFinishURL: d.Config.API.PaymentFinishURL,
		Sandbox:      d.Config.Midtrans.Env == config.MidtransSandbox,
		Log:          d.Log,
	})
	reviewSvc := review.NewService(review.NewRepository(d.DB))
	returnSvc := returns.NewService(returns.NewRepository(d.DB), returns.Options{WindowDays: d.Config.Returns.WindowDays})
	orderSvc := order.NewService(order.NewRepository(d.DB), order.Deps{Payments: paymentSvc, Returns: returnSvc})
	checkoutSvc := checkout.NewService(checkout.NewRepository(d.DB), addressSvc, shippingSvc)
	inventorySvc := inventory.NewService(d.DB, d.Config.Inventory.LowStockThreshold)

	// Header cart badge on full pages for signed-in users (one small query);
	// admin pages have no storefront header, so no query there.
	d.View.SetCartCounter(func(r *http.Request) int {
		p, ok := auth.FromContext(r.Context())
		if !ok || strings.HasPrefix(r.URL.Path, "/admin") {
			return 0
		}
		n, err := cartSvc.Count(r.Context(), p.UserID)
		if err != nil {
			d.Log.Error("cart badge", "error", err)
		}
		return n
	})

	paymentH := payment.NewHandler(paymentSvc, d.View, d.Log)
	reviewH := review.NewHandler(reviewSvc, d.View)
	authH := auth.NewHandler(authSvc, d.View, auth.NewCookieConfig(d.Config.IsProduction()), d.Log)

	r := chi.NewRouter()

	r.Use(
		middleware.RealIP,
		middleware.Logger(d.Log),
		middleware.Recover(d.Log, func(w http.ResponseWriter, req *http.Request) {
			if api.IsAPI(req.URL.Path) {
				api.InternalError(w)
				return
			}
			d.View.Error(w, req, http.StatusInternalServerError)
		}),
		middleware.SecureHeaders(d.Config.IsProduction(), d.CheckoutOrigin),
		// The web keeps the same-origin check; /api/v1 is exempt because it
		// authenticates with Bearer tokens only, never cookies.
		middleware.CSRF(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			d.View.Error(w, req, http.StatusForbidden)
		}), api.Prefix+"/"),
		authH.LoadSession,
	)

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if api.IsAPI(req.URL.Path) {
			api.NotFound(w)
			return
		}
		d.View.Error(w, req, http.StatusNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		d.View.Error(w, req, http.StatusMethodNotAllowed)
	})

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.DB))
	r.Method(http.MethodGet, "/static/*", d.Assets)
	r.Method(http.MethodHead, "/static/*", d.Assets)

	home.NewHandler(productSvc, categorySvc, d.View).Routes(r)
	category.NewHandler(categorySvc, d.View).Routes(r)
	product.NewHandler(productSvc, categorySvc, reviewSvc, d.View).Routes(r)
	reviewH.PublicRoutes(r)
	authH.Routes(r)
	paymentH.WebhookRoutes(r)
	shipping.NewWebhookHandler(shippingSvc, d.Log).Routes(r)

	// REST API for mobile apps: the same services, a JSON transport and
	// Bearer-token API sessions (no cookies).
	r.Route(api.Prefix, api.NewHandler(api.Deps{
		Log: d.Log, Auth: authSvc, Users: userSvc, Addresses: addressSvc, Categories: categorySvc,
		Products: productSvc, Cart: cartSvc, Checkout: checkoutSvc, Orders: orderSvc, Payments: paymentSvc,
		Reviews: reviewSvc, Returns: returnSvc, Shipping: shippingSvc, CORSOrigins: d.Config.API.CORSOrigins,
	}).Routes)

	// Back office: signed-in ADMIN only. Guests go to the login page; any
	// other account gets the ordinary 404, as if /admin did not exist.
	adminH := admin.NewHandler(admin.Deps{
		View:       d.View,
		Log:        d.Log,
		Orders:     fulfillment.NewService(d.DB, paymentSvc),
		Products:   productSvc,
		Categories: categorySvc,
		Inventory:  inventorySvc,
		Returns:    returnSvc,
		Reviews:    reviewSvc,
		Reports:    report.NewService(d.DB, inventorySvc),
		Audit:      audit.NewService(d.DB),
		Shipping:   shippingSvc,
	})
	r.Route("/admin", func(r chi.Router) {
		r.Use(auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			d.View.Error(w, req, http.StatusNotFound)
		})))
		adminH.Routes(r)
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireUser)
		user.NewHandler(userSvc, d.View).Routes(r)
		address.NewHandler(addressSvc, shippingSvc, d.View).Routes(r)
		cart.NewHandler(cartSvc, d.View).Routes(r)
		checkout.NewHandler(checkoutSvc, cartSvc, orderSvc, d.View).Routes(r)
		order.NewHandler(orderSvc, d.View).Routes(r)
		paymentH.Routes(r)
		reviewH.Routes(r)
		returns.NewHandler(returnSvc, d.View).Routes(r)
	})

	return r
}

// Run serves until ctx is cancelled (SIGINT/SIGTERM), then drains in-flight
// requests for up to 15 seconds.
func Run(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
