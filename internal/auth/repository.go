package auth

import (
	"context"
	"time"

	"ecommerce/internal/database"
	"ecommerce/internal/database/dbgen"
)

type Repository struct {
	q *dbgen.Queries
}

func NewRepository(db dbgen.DBTX) *Repository { return &Repository{q: dbgen.New(db)} }

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
