// Package testutil provides an isolated PostgreSQL schema per test.
//
// Database tests run only when TEST_DATABASE_URL is set, e.g.
//
//	TEST_DATABASE_URL=postgres://postgres@127.0.0.1:5432/ecommerce_test?sslmode=disable go test ./...
//
// Each test gets a fresh schema with all migrations applied; it is dropped
// when the test ends. Never point this at a production database.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/database"
)

// DB returns a pool whose search_path is a new, migrated schema.
func DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()

	suffix := make([]byte, 6)
	rand.Read(suffix)
	schema := "test_" + hex.EncodeToString(suffix)

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := database.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// CreateUser inserts a user directly (no bcrypt) and returns its id.
func CreateUser(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id", email).Scan(&id)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return id
}

// CompleteOrder turns an order into a paid, COMPLETED order, as the admin
// and shipping phases will (test fixture only; nothing in the app does this
// yet): a PAID payment, paid_at and completed_at.
func CompleteOrder(t *testing.T, pool *pgxpool.Pool, orderNumber string, completedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO payments (order_id, attempt_no, provider, provider_order_id, amount, status, paid_at, expires_at)
		SELECT o.id, coalesce((SELECT max(p.attempt_no) FROM payments p WHERE p.order_id = o.id), 0) + 1,
		       'test', o.order_number || '-T', o.total, 'PAID', $2, $2
		FROM orders o
		WHERE o.order_number = $1
		  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.order_id = o.id AND p.status = 'PAID')`,
		orderNumber, completedAt); err != nil {
		t.Fatalf("complete order %s: payment: %v", orderNumber, err)
	}
	tag, err := pool.Exec(ctx,
		"UPDATE orders SET status = 'COMPLETED', paid_at = coalesce(paid_at, $2), completed_at = $2 WHERE order_number = $1",
		orderNumber, completedAt)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("complete order %s: %v", orderNumber, err)
	}
}
