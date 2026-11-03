// Package auth owns credentials and login sessions: register, login,
// logout, and resolving a session token to the signed-in user.
//
// The Service has no HTTP dependency. The web uses a server-side session
// cookie; a future mobile/API client may use its own credential transport
// (API token or mobile session, decided in the mobile API phase) while
// calling this same Service for registration, credential checks and user
// lookup. A Google sign-in would add a user_identities table and a Service
// method that ends in StartSession.
package auth

import (
	"context"
	"errors"
	"time"
)

type Role string

const (
	RoleUser  Role = "USER"
	RoleAdmin Role = "ADMIN"
)

type User struct {
	ID        int64
	Email     string
	Role      Role
	CreatedAt time.Time
}

// Principal is the signed-in identity attached to a request.
type Principal struct {
	UserID int64
	Email  string
	Role   Role
}

func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

// Session is what the client keeps: an opaque random token and its expiry.
type Session struct {
	Token     string
	ExpiresAt time.Time
}

type RegisterInput struct {
	Email           string
	Password        string
	PasswordConfirm string
}

var (
	ErrEmailTaken         = errors.New("auth: email already registered")
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	ErrSessionNotFound    = errors.New("auth: session not found or expired")
	errUserNotFound       = errors.New("auth: user not found")
)

type principalKey struct{}

// WithPrincipal returns a context carrying the signed-in user.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the signed-in user, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
