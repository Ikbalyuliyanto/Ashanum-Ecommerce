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
//	ecommerce create-admin -email E -first-name N [-last-name N] [-promote]
//	                          create an admin account (password read from
//	                          stdin, never from a flag); -promote makes an
//	                          existing account admin instead
//	ecommerce cleanup-api-sessions
//	                          delete expired / revoked mobile API sessions
//	ecommerce reconcile-shipments [-limit N]
//	                          refresh provider shipments still moving, list
//	                          bookings without an answer, delete stale quotes
//	ecommerce version         print the build version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"ecommerce/internal/auth"
	"ecommerce/internal/config"
	"ecommerce/internal/database"
	"ecommerce/internal/payment"
	"ecommerce/internal/payment/midtrans"
	"ecommerce/internal/review"
	"ecommerce/internal/server"
	"ecommerce/internal/shipping"
	"ecommerce/internal/shipping/biteship"
	"ecommerce/internal/validate"
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
	case "create-admin":
		return createAdmin(ctx, cfg, args)
	case "cleanup-api-sessions":
		return cleanupAPISessions(ctx, cfg)
	case "reconcile-shipments":
		return reconcileShipments(ctx, cfg, log, args)
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, reconcile-payments, reconcile-shipments, recalculate-ratings, create-admin, cleanup-api-sessions, version)", cmd)
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
	if bs, err := shippingProvider(cfg, log); err != nil {
		return err
	} else if bs != nil {
		deps.ShippingProvider = bs
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
	log.Warn("payments use the Midtrans SANDBOX: never accept real payments yet")
	return midtrans.New(midtrans.Config{Env: cfg.Midtrans.Env, ServerKey: cfg.Midtrans.ServerKey})
}

// shippingProvider returns the Biteship client, or nil when
// BITESHIP_API_KEY is not set (shipping off: orders cannot be placed).
func shippingProvider(cfg *config.Config, log *slog.Logger) (*biteship.Client, error) {
	if !cfg.Shipping.Enabled() {
		log.Warn("BITESHIP_API_KEY not set: shipping rates are unavailable, checkout cannot place orders")
		return nil, nil
	}
	b := cfg.Shipping.Biteship
	if b.Env == biteship.EnvTest {
		log.Warn("shipping uses the Biteship TESTING environment: no real pickup")
	}
	if b.WebhookSecret == "" {
		log.Warn("BITESHIP_WEBHOOK_SECRET not set: shipping webhooks are refused; tracking updates via admin refresh or reconcile-shipments")
	}
	return biteship.New(biteship.Config{Env: b.Env, APIKey: b.APIKey, LiveEnabled: cfg.Shipping.LiveEnabled, BaseURL: b.BaseURL,
		WebhookHeader: b.WebhookHeader, WebhookSecret: b.WebhookSecret})
}

// reconcileShipments runs one shipping reconciliation pass (see
// shipping.Service.Reconcile). Stateless: call it from a systemd timer or
// by hand.
func reconcileShipments(ctx context.Context, cfg *config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("reconcile-shipments", flag.ContinueOnError)
	limit := fs.Int("limit", 100, "maximum shipments per run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bs, err := shippingProvider(cfg, log)
	if err != nil {
		return err
	}
	if bs == nil {
		return errors.New("reconcile-shipments: shipping is not configured (BITESHIP_API_KEY)")
	}
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := shipping.NewService(db, bs, server.ShippingOptions(cfg, log)).Reconcile(ctx, max(*limit, 1))
	if err != nil {
		return err
	}
	fmt.Printf("shipments checked: %d, errors: %d, bookings without answer: %d, stale quotes deleted: %d\n",
		rep.Checked, rep.Errors, len(rep.StalePending), rep.QuotesDeleted)
	for _, n := range rep.StalePending {
		fmt.Printf("REVIEW %s: booking still PENDING; retry \"Buat Pengiriman\" in admin\n", n)
	}
	if rep.Errors > 0 {
		return fmt.Errorf("reconcile-shipments: %d error(s), see log", rep.Errors)
	}
	return nil
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

// cleanupAPISessions deletes API sessions past their refresh expiry and
// revoked ones older than a day. Logins already purge expired sessions
// lazily; this is for a systemd timer or cron, not a resident worker.
func cleanupAPISessions(ctx context.Context, cfg *config.Config) error {
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	n, err := auth.NewService(auth.NewRepository(db), cfg.Session.Secret).CleanupAPISessions(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("api sessions deleted: %d\n", n)
	return nil
}

// createAdmin creates the first (or another) admin account. There is no
// built-in admin and no default password: the password is read from stdin
// (typed twice on a terminal, echo off where stty exists, or piped as one
// line) and never accepted as a flag, printed or logged. An existing email
// is only made admin with -promote (its password stays unchanged).
func createAdmin(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	email := fs.String("email", "", "admin email (required)")
	first := fs.String("first-name", "", "first name (required for a new account)")
	last := fs.String("last-name", "", "last name")
	promote := fs.Bool("promote", false, "make an existing account admin (password unchanged)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("create-admin: -email is required")
	}
	in := auth.AdminInput{Email: *email, FirstName: *first, LastName: *last, Promote: *promote}
	if !*promote {
		pw, err := readPassword()
		if err != nil {
			return err
		}
		in.Password = pw
	}

	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := auth.NewService(auth.NewRepository(db), cfg.Session.Secret).CreateAdmin(ctx, in)
	var verr validate.Errors
	switch {
	case errors.As(err, &verr):
		return fmt.Errorf("create-admin: %s", verr.Error())
	case errors.Is(err, auth.ErrEmailTaken):
		return errors.New("create-admin: this email already has an account; rerun with -promote to make it admin")
	case errors.Is(err, auth.ErrUnknownEmail):
		return errors.New("create-admin: no account with this email to promote")
	case err != nil:
		return err
	}
	switch {
	case res.Created:
		fmt.Printf("admin account created: %s (id %d)\n", res.User.Email, res.User.ID)
	case res.Promoted:
		fmt.Printf("account promoted to admin: %s (id %d)\n", res.User.Email, res.User.ID)
	default:
		fmt.Printf("already an admin, nothing changed: %s (id %d)\n", res.User.Email, res.User.ID)
	}
	return nil
}

// readPassword reads the new admin password from stdin. On a terminal it
// asks twice with echo turned off when stty is available (Linux, the
// HG680P); piped input is one line.
func readPassword() (string, error) {
	st, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	in := bufio.NewReader(os.Stdin)
	if st.Mode()&os.ModeCharDevice == 0 { // piped
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("create-admin: no password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if stty("-echo") == nil {
		defer func() { stty("echo"); fmt.Fprintln(os.Stderr) }()
	}
	ask := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		line, err := in.ReadString('\n')
		fmt.Fprintln(os.Stderr)
		return strings.TrimRight(line, "\r\n"), err
	}
	pw, err := ask("Password admin (min. 12 karakter): ")
	if err != nil {
		return "", err
	}
	again, err := ask("Ulangi password: ")
	if err != nil {
		return "", err
	}
	if pw != again {
		return "", errors.New("create-admin: passwords do not match")
	}
	return pw, nil
}

func stty(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
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
