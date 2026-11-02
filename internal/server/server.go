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

	"ecommerce/internal/config"
	"ecommerce/internal/home"
	"ecommerce/internal/middleware"
	"ecommerce/internal/view"
)

type Deps struct {
	Config *config.Config
	Log    *slog.Logger
	DB     *pgxpool.Pool
	View   *view.Renderer
	Assets *view.Assets
}

// NewRouter builds the application handler. New modules register their
// routes here, each through its own Routes method.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(
		middleware.RealIP,
		middleware.Logger(d.Log),
		middleware.Recover(d.Log, func(w http.ResponseWriter, req *http.Request) {
			d.View.Error(w, req, http.StatusInternalServerError)
		}),
		middleware.SecureHeaders(d.Config.IsProduction()),
		middleware.CSRF(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			d.View.Error(w, req, http.StatusForbidden)
		})),
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

	home.NewHandler(d.View).Routes(r)

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
