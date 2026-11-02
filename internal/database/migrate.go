package database

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrations are forward-only SQL files embedded in the binary, so production
// needs no extra tool: run `ecommerce migrate`.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey is an arbitrary constant for pg_advisory_lock so two
// processes never migrate concurrently.
const migrationLockKey int64 = 7_351_202_611

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type Migration struct {
	Version  string
	File     string
	SQL      string
	Checksum string
}

type MigrationStatus struct {
	Migration
	AppliedAt *time.Time
}

// LoadMigrations returns the embedded migrations ordered by version.
func LoadMigrations() ([]Migration, error) {
	return loadMigrations(migrationFiles, "migrations")
}

func loadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir: %w", err)
	}

	seen := make(map[string]string)
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrate: invalid file name %q (want NNNN_name.sql)", e.Name())
		}
		if prev, dup := seen[m[1]]; dup {
			return nil, fmt.Errorf("migrate: duplicate version %s: %s and %s", m[1], prev, e.Name())
		}
		seen[m[1]] = e.Name()

		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  m[1],
			File:     e.Name(),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies all pending migrations, each in its own transaction.
// An applied migration whose file was edited afterwards is an error: write a
// new migration instead of changing an old one.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	migrations, err := LoadMigrations()
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			file       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied, err := appliedChecksums(ctx, conn)
	if err != nil {
		return err
	}

	pending := 0
	for _, m := range migrations {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return fmt.Errorf("migrate: %s was modified after it was applied", m.File)
			}
			continue
		}

		start := time.Now()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migrate: begin %s: %w", m.File, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migrate: apply %s: %w", m.File, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version, file, checksum) VALUES ($1, $2, $3)",
			m.Version, m.File, m.Checksum); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migrate: record %s: %w", m.File, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migrate: commit %s: %w", m.File, err)
		}
		pending++
		log.Info("migration applied", "file", m.File, "duration", time.Since(start).Round(time.Millisecond))
	}

	log.Info("migrations up to date", "applied_now", pending, "total", len(migrations))
	return nil
}

// Status reports every embedded migration and when it was applied.
func Status(ctx context.Context, pool *pgxpool.Pool) ([]MigrationStatus, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}

	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return nil, fmt.Errorf("migrate: status: %w", err)
	}
	appliedAt := make(map[string]time.Time)
	if exists {
		rows, err := pool.Query(ctx, "SELECT version, applied_at FROM schema_migrations")
		if err != nil {
			return nil, fmt.Errorf("migrate: status: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			var t time.Time
			if err := rows.Scan(&v, &t); err != nil {
				return nil, err
			}
			appliedAt[v] = t
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	out := make([]MigrationStatus, len(migrations))
	for i, m := range migrations {
		out[i].Migration = m
		if t, ok := appliedAt[m.Version]; ok {
			out[i].AppliedAt = &t
		}
	}
	return out, nil
}

// appliedChecksums reads on the lock-holding connection so migrating also
// works with DB_MAX_CONNS=1.
func appliedChecksums(ctx context.Context, conn *pgxpool.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var v, sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		out[v] = sum
	}
	return out, rows.Err()
}
