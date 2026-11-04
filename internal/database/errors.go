package database

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// IsNoRows reports whether a query returned no row.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsUniqueViolation reports whether err violates the named unique
// constraint or index.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// IsForeignKeyViolation reports whether err violates the named foreign key.
func IsForeignKeyViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == constraint
}

// Like escapes q for an ILIKE '%' || q || '%' pattern, so % and _ typed in a
// search box match themselves (backslash is PostgreSQL's default escape).
func Like(q string) string {
	return strings.NewReplacer(`\`, `\`, `%`, `\%`, `_`, `\_`).Replace(q)
}
