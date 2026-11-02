// Package database owns the PostgreSQL connection pool and schema migrations.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/config"
)

// Open creates a small connection pool sized for a low-memory host and
// verifies the remote database is reachable.
//
// Extra session settings (sslmode, statement_timeout, ...) can be passed as
// query parameters in DATABASE_URL.
func Open(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("database: parse DATABASE_URL: %w", err)
	}

	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = time.Hour
	pc.MaxConnIdleTime = 15 * time.Minute
	pc.HealthCheckPeriod = time.Minute
	if pc.ConnConfig.ConnectTimeout == 0 {
		pc.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	if _, ok := pc.ConnConfig.RuntimeParams["application_name"]; !ok {
		pc.ConnConfig.RuntimeParams["application_name"] = "ecommerce"
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping %s: %w", pc.ConnConfig.Host, err)
	}
	return pool, nil
}
