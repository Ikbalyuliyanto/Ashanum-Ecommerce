// Package server wires middleware and module routes into one HTTP handler
// and runs it behind Nginx.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/address"
	"ecommerce/internal/auth"
	"ecommerce/internal/cart"
	"ecommerce/internal/category"
	"ecommerce/internal/checkout"
	"ecommerce/internal/config"
	"ecommerce/internal/home"
	"ecommerce/internal/middleware"
	"ecommerce/internal/order"
	"ecommerce/internal/payment"
	"ecommerce/internal/product"
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
}

// NewRouter is the composition root: it builds repositories, services and
// handlers, then registers each module's routes through its Routes method.
func NewRouter(d Deps) http.Handler {
	authSvc := auth.NewService(auth.NewRepository(d.DB), d.Config.Session.Secret)
	userSvc := user.NewService(user.NewRepository(d.DB))
	addressSvc := address.NewService(address.NewRepository(d.DB))
	categorySvc := category.NewService(category.NewRepository(d.DB))
	productSvc := product.NewService(product.NewRepository(d.DB))
	cartSvc := cart.NewService(cart.NewRepository(d.DB))
	paymentSvc := payment.NewService(payment.NewRepository(d.DB), d.PaymentProvider, payment.Options{
		BaseURL: d.Config.BaseURL,
		Sandbox: d.Config.Midtrans.Env == config.MidtransSandbox,
		Log:     d.Log,
	})
	orderSvc := order.NewService(order.NewRepository(d.DB), paymentSvc)
	checkoutSvc := checkout.NewService(checkout.NewRepository(d.DB), addressSvc)

	// Header cart badge on full pages for signed-in users (one small query).
	d.View.SetCartCounter(func(r *http.Request) int {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			return 0
		}
		n, err := cartSvc.Count(r.Context(), p.UserID)
		if err != nil {
			d.Log.Error("cart badge", "error", err)
		}
		return n
	})

	paymentH := payment.NewHandler(paymentSvc, d.View, d.Log)
	authH := auth.NewHandler(authSvc, d.View, auth.NewCookieConfig(d.Config.IsProduction()), d.Log)

	r := chi.NewRouter()

	r.Use(
		middleware.RealIP,
		middleware.Logger(d.Log),
		middleware.Recover(d.Log, func(w http.ResponseWriter, req *http.Request) {
			d.View.Error(w, req, http.StatusInternalServerError)
		}),
		middleware.SecureHeaders(d.Config.IsProduction(), d.CheckoutOrigin),
		middleware.CSRF(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			d.View.Error(w, req, http.StatusForbidden)
		})),
		authH.LoadSession,
	)

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
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
	product.NewHandler(productSvc, categorySvc, d.View).Routes(r)
	authH.Routes(r)
	paymentH.WebhookRoutes(r)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireUser)
		user.NewHandler(userSvc, d.View).Routes(r)
		address.NewHandler(addressSvc, d.View).Routes(r)
		cart.NewHandler(cartSvc, d.View).Routes(r)
		checkout.NewHandler(checkoutSvc, cartSvc, orderSvc, d.View).Routes(r)
		order.NewHandler(orderSvc, d.View).Routes(r)
		paymentH.Routes(r)
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
