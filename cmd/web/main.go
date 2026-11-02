// Command ecommerce is the single production binary.
//
//	ecommerce                 start the web server (same as "serve")
//	ecommerce serve           start the web server
//	ecommerce migrate         apply pending database migrations
//	ecommerce migrate status  list migrations and when they were applied
//	ecommerce version         print the build version
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ecommerce/internal/config"
	"ecommerce/internal/database"
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
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, version)", cmd)
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
	renderer, err := view.New(templatesFS, assets, view.App{Name: cfg.Name, Dev: !cfg.IsProduction()}, log)
	if err != nil {
		return err
	}

	handler := server.NewRouter(server.Deps{
		Config: cfg,
		Log:    log,
		DB:     db,
		View:   renderer,
		Assets: assets,
	})

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
