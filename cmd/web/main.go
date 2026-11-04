// Command ecommerce is the single production binary.
//
//	ecommerce                 start the web server (same as "serve")
//	ecommerce serve           start the web server
//	ecommerce migrate         apply pending database migrations
//	ecommerce migrate status  list migrations and when they were applied
//	ecommerce reconcile-payments [-limit N]
//	                          sync pending payments with the provider, expire
//	                          overdue unpaid orders, list payments to review
//	ecommerce recalculate-ratings
//	                          rebuild cached product ratings from product_reviews
//	ecommerce version         print the build version
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ecommerce/internal/auth"
	"ecommerce/internal/config"
	"ecommerce/internal/database"
	"ecommerce/internal/payment"
	"ecommerce/internal/payment/midtrans"
	"ecommerce/internal/review"
	"ecommerce/internal/server"
	"ecommerce/internal/view"
	"ecommerce/web"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "version" {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, log)
	case "migrate":
		return migrate(ctx, cfg, log, args)
	case "reconcile-payments":
		return reconcilePayments(ctx, cfg, log, args)
	case "recalculate-ratings":
		return recalculateRatings(ctx, cfg)
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, reconcile-payments, recalculate-ratings, version)", cmd)
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	templatesFS, staticFS, err := webFS(cfg)
	if err != nil {
		return err
	}
	assets, err := view.NewAssets(staticFS, "/static/", !cfg.IsProduction())
	if err != nil {
		return fmt.Errorf("static assets: %w", err)
	}
	renderer, err := view.New(view.Config{
		FS:          templatesFS,
		Assets:      assets,
		App:         view.App{Name: cfg.Name, Dev: !cfg.IsProduction()},
		Log:         log,
		CurrentUser: auth.CurrentUser,
	})
	if err != nil {
		return err
	}
	if cfg.Session.Ephemeral {
		log.Warn("SESSION_SECRET not set: using a random one, sessions end on restart")
	}

	deps := server.Deps{
		Config: cfg,
		Log:    log,
		DB:     db,
		View:   renderer,
		Assets: assets,
	}
	if mt, err := paymentProvider(cfg, log); err != nil {
		return err
	} else if mt != nil {
		deps.PaymentProvider, deps.CheckoutOrigin = mt, mt.CheckoutOrigin()
	}
	handler := server.NewRouter(deps)

	log.Info("starting", "version", version, "env", cfg.Env)
	return server.Run(ctx, cfg.Addr(), handler, log)
}

func migrate(ctx context.Context, cfg *config.Config, log *slog.Logger, args []string) error {
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "up":
		return database.Migrate(ctx, db, log)
	case "status":
		list, err := database.Status(ctx, db)
		if err != nil {
			return err
		}
		for _, m := range list {
			state := "pending"
			if m.AppliedAt != nil {
				state = "applied " + m.AppliedAt.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Printf("%-40s %s\n", m.File, state)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate command %q (want up, status)", sub)
	}
}

// paymentProvider returns the Midtrans sandbox client, or nil when
// MIDTRANS_SERVER_KEY is not set (online payment disabled).
func paymentProvider(cfg *config.Config, log *slog.Logger) (*midtrans.Client, error) {
	if !cfg.Midtrans.Enabled() {
		log.Warn("MIDTRANS_SERVER_KEY not set: online payment disabled")
		return nil, nil
	}
	log.Warn("payments use the Midtrans SANDBOX: order totals exclude shipping cost; never accept real payments yet")
	return midtrans.New(midtrans.Config{Env: cfg.Midtrans.Env, ServerKey: cfg.Midtrans.ServerKey})
}

// reconcilePayments runs one reconciliation pass (see payment.Service.Reconcile).
// It holds no state between runs: call it from a systemd timer or by hand.
func reconcilePayments(ctx context.Context, cfg *config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("reconcile-payments", flag.ContinueOnError)
	limit := fs.Int("limit", 100, "maximum rows per step")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	var provider payment.Provider
	if mt, err := paymentProvider(cfg, log); err != nil {
		return err
	} else if mt != nil {
		provider = mt
	}
	svc := payment.NewService(payment.NewRepository(db), provider, payment.Options{BaseURL: cfg.BaseURL, Log: log})
	rep, err := svc.Reconcile(ctx, max(*limit, 1))
	if err != nil {
		return err
	}
	fmt.Printf("pending checked: %d, updated: %d, orders expired: %d, errors: %d, needs review: %d\n",
		rep.Checked, rep.Updated, rep.Expired, rep.Errors, len(rep.Review))
	for _, it := range rep.Review {
		fmt.Printf("REVIEW %s %s %s (%s): %s\n", it.OrderNumber, it.ProviderOrderID, it.Status, it.ProviderStatus, it.Reason)
	}
	if rep.Errors > 0 {
		return fmt.Errorf("reconcile-payments: %d error(s), see log", rep.Errors)
	}
	return nil
}

// recalculateRatings repairs products whose cached rating differs from
// product_reviews (reviews already keep it in sync; this is a repair tool).
func recalculateRatings(ctx context.Context, cfg *config.Config) error {
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	n, err := review.NewService(review.NewRepository(db)).RecalculateAll(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("products repaired: %d\n", n)
	return nil
}

// webFS returns the embedded templates/static in production. In development
// it reads web/ from disk (when present) so edits apply without rebuilding.
func webFS(cfg *config.Config) (templates, static fs.FS, err error) {
	if !cfg.IsProduction() {
		if st, statErr := os.Stat("web/templates"); statErr == nil && st.IsDir() {
			return os.DirFS("web/templates"), os.DirFS("web/static"), nil
		}
	}
	if templates, err = fs.Sub(web.FS, "templates"); err != nil {
		return nil, nil, err
	}
	if static, err = fs.Sub(web.FS, "static"); err != nil {
		return nil, nil, err
	}
	return templates, static, nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	if cfg.IsProduction() {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
