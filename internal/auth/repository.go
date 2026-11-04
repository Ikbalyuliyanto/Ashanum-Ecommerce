package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"ecommerce/internal/audit"
	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	db dbgen.DBTX
	q  *dbgen.Queries
}

func NewRepository(db dbgen.DBTX) *Repository { return &Repository{db: db, q: dbgen.New(db)} }

func (r *Repository) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	row, err := r.q.CreateUser(ctx, dbgen.CreateUserParams{Email: email, PasswordHash: passwordHash})
	if database.IsUniqueViolation(err, "users_email_key") {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	return User{ID: row.ID, Email: row.Email, Role: Role(row.Role), CreatedAt: row.CreatedAt}, nil
}

// FindCredentials returns the user and stored password hash for an email.
func (r *Repository) FindCredentials(ctx context.Context, email string) (User, string, error) {
	row, err := r.q.GetUserCredentialsByEmail(ctx, email)
	if database.IsNoRows(err) {
		return User{}, "", errUserNotFound
	}
	if err != nil {
		return User{}, "", err
	}
	return User{ID: row.ID, Email: row.Email, Role: Role(row.Role), CreatedAt: row.CreatedAt}, row.PasswordHash, nil
}

func (r *Repository) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error {
	return r.q.CreateSession(ctx, dbgen.CreateSessionParams{TokenHash: tokenHash, UserID: userID, ExpiresAt: expiresAt})
}

// ActiveSession resolves an unexpired session to its user in one query.
func (r *Repository) ActiveSession(ctx context.Context, tokenHash []byte) (Principal, time.Time, error) {
	row, err := r.q.GetActiveSession(ctx, tokenHash)
	if database.IsNoRows(err) {
		return Principal{}, time.Time{}, ErrSessionNotFound
	}
	if err != nil {
		return Principal{}, time.Time{}, err
	}
	return Principal{UserID: row.UserID, Email: row.Email, Role: Role(row.Role)}, row.ExpiresAt, nil
}

func (r *Repository) ExtendSession(ctx context.Context, tokenHash []byte, expiresAt time.Time) error {
	return r.q.ExtendSession(ctx, dbgen.ExtendSessionParams{TokenHash: tokenHash, ExpiresAt: expiresAt})
}

func (r *Repository) DeleteSession(ctx context.Context, tokenHash []byte) error {
	return r.q.DeleteSession(ctx, tokenHash)
}

func (r *Repository) DeleteExpiredSessions(ctx context.Context) error {
	_, err := r.q.DeleteExpiredSessions(ctx)
	return err
}

// CreateAdmin inserts an admin (hash set) or, with promote, makes the
// existing account of email ADMIN — in one transaction with its profile
// name and the audit entry.
func (r *Repository) CreateAdmin(ctx context.Context, email, hash, first, last string, promote bool) (AdminResult, error) {
	b, ok := r.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return AdminResult{}, errors.New("auth: CreateAdmin needs a connection pool")
	}
	var res AdminResult
	err := pgx.BeginFunc(ctx, b, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.UserByEmail(ctx, email)
		switch {
		case database.IsNoRows(err) && promote:
			return ErrUnknownEmail
		case database.IsNoRows(err):
			created, err := q.CreateAdminUser(ctx, dbgen.CreateAdminUserParams{Email: email, PasswordHash: hash})
			if database.IsUniqueViolation(err, "users_email_key") {
				return ErrEmailTaken
			}
			if err != nil {
				return err
			}
			res = AdminResult{User: User{ID: created.ID, Email: created.Email, Role: Role(created.Role), CreatedAt: created.CreatedAt}, Created: true}
			if err := q.SetProfileName(ctx, dbgen.SetProfileNameParams{UserID: created.ID, FirstName: first, LastName: last}); err != nil {
				return err
			}
			return audit.Record(ctx, q, audit.Entry{AdminID: created.ID, Action: "user.create_admin", EntityType: audit.EntityUser,
				EntityID: created.ID, Label: created.Email, Meta: map[string]any{"via": "cli"}})
		case err != nil:
			return err
		case !promote:
			return ErrEmailTaken
		}
		res = AdminResult{User: User{ID: row.ID, Email: row.Email, Role: RoleAdmin, CreatedAt: row.CreatedAt}}
		n, err := q.PromoteToAdmin(ctx, row.ID)
		if err != nil || n == 0 {
			return err // n == 0: already an admin, nothing to do
		}
		res.Promoted = true
		return audit.Record(ctx, q, audit.Entry{AdminID: row.ID, Action: "user.grant_admin", EntityType: audit.EntityUser,
			EntityID: row.ID, Label: row.Email, Meta: map[string]any{"via": "cli", "from": row.Role}})
	})
	return res, err
}
