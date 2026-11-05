package auth

// API sessions: the credential transport of the REST API (mobile apps).
//
// The web keeps its session cookie. API clients get an opaque Bearer access
// token (short-lived) and a refresh token (long-lived, rotating). Both are
// 256-bit random values; only their HMAC is stored (tokenHash, keyed with
// SESSION_SECRET), so a copy of the table cannot be replayed. Registration,
// password checks and bcrypt are the same Service code the web uses.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultAccessTTL and DefaultRefreshTTL apply when ConfigureAPI was
	// not called (API_ACCESS_TOKEN_TTL / API_REFRESH_TOKEN_TTL).
	DefaultAccessTTL  = 15 * time.Minute
	DefaultRefreshTTL = 30 * 24 * time.Hour

	// Prefixes make the two kinds impossible to mix up (and easy for secret
	// scanners to spot); the whole string is hashed.
	accessPrefix  = "ashat_"
	refreshPrefix = "ashrt_"

	// touchEvery bounds last_used_at writes: at most one per session per
	// interval, so most authenticated requests cost one read.
	touchEvery = 10 * time.Minute

	maxDeviceName = 100
)

var (
	// ErrInvalidToken is an unknown, revoked or rotated token.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrTokenExpired is a valid access token past its lifetime: refresh.
	ErrTokenExpired = errors.New("auth: token expired")
	// ErrRefreshReused is a refresh token presented after it was rotated:
	// its whole family has just been revoked.
	ErrRefreshReused = errors.New("auth: refresh token reused")
)

// TokenPair is what an API client receives at login and on each refresh.
// The plaintext tokens exist only here; they are never stored or logged.
type TokenPair struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// APIPrincipal is the user behind a valid access token.
type APIPrincipal struct {
	Principal
	SessionID int64
	FamilyID  int64
}

// ConfigureAPI sets the token lifetimes (call while wiring, before serving).
func (s *Service) ConfigureAPI(accessTTL, refreshTTL time.Duration) {
	if accessTTL > 0 {
		s.accessTTL = accessTTL
	}
	if refreshTTL > 0 {
		s.refreshTTL = refreshTTL
	}
}

// AccessTTL is the configured access token lifetime.
func (s *Service) AccessTTL() time.Duration { return s.accessTTL }

// VerifyCredentials checks an email and password with bcrypt and returns the
// user, without starting any session. Wrong email and wrong password are
// both ErrInvalidCredentials and take the same time.
func (s *Service) VerifyCredentials(ctx context.Context, email, password string) (User, error) {
	email, ok := normalizeLogin(email, password)
	if !ok {
		return User{}, ErrInvalidCredentials
	}
	u, hash, err := s.repo.FindCredentials(ctx, email)
	if errors.Is(err, errUserNotFound) {
		s.comparePassword(ctx, s.dummyHash(), password)
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, err
	}
	if err := s.comparePassword(ctx, []byte(hash), password); err != nil {
		if isMismatch(err) {
			return User{}, ErrInvalidCredentials
		}
		return User{}, err
	}
	return u, nil
}

// LoginAPI checks credentials and issues a new token family.
func (s *Service) LoginAPI(ctx context.Context, email, password, device string) (User, TokenPair, error) {
	u, err := s.VerifyCredentials(ctx, email, password)
	if err != nil {
		return User{}, TokenPair{}, err
	}
	pair, err := s.IssueAPITokens(ctx, u.ID, device)
	return u, pair, err
}

// IssueAPITokens starts a new API session (token family) for a user, e.g.
// right after registration or login. Expired API sessions are purged here
// instead of by a background worker.
func (s *Service) IssueAPITokens(ctx context.Context, userID int64, device string) (TokenPair, error) {
	_ = s.repo.DeleteExpiredAPISessions(ctx) // best effort
	pair, err := s.newPair()
	if err != nil {
		return TokenPair{}, err
	}
	_, err = s.repo.CreateAPISession(ctx, apiSessionRow{UserID: userID, AccessHash: s.tokenHash(pair.AccessToken),
		RefreshHash: s.tokenHash(pair.RefreshToken), AccessExpires: pair.AccessExpiresAt,
		RefreshExpires: pair.RefreshExpiresAt, Device: cleanDevice(device)})
	return pair, err
}

// AuthenticateAPI resolves a Bearer access token in one indexed query.
// Unknown, rotated or revoked tokens are ErrInvalidToken; an expired one is
// ErrTokenExpired (the client should refresh).
func (s *Service) AuthenticateAPI(ctx context.Context, token string) (APIPrincipal, error) {
	if !strings.HasPrefix(token, accessPrefix) || len(token) > 128 {
		return APIPrincipal{}, ErrInvalidToken
	}
	row, err := s.repo.APISessionByAccess(ctx, s.tokenHash(token))
	if err != nil {
		return APIPrincipal{}, err
	}
	now := s.now()
	switch {
	case row.RevokedAt != nil || row.RotatedAt != nil:
		return APIPrincipal{}, ErrInvalidToken
	case !now.Before(row.AccessExpiresAt):
		return APIPrincipal{}, ErrTokenExpired
	}
	if now.Sub(row.LastUsedAt) > touchEvery {
		_ = s.repo.TouchAPISession(ctx, row.ID) // best effort
	}
	return APIPrincipal{Principal: row.Principal, SessionID: row.ID, FamilyID: row.FamilyID}, nil
}

// RefreshAPI rotates a refresh token in one transaction: the presented
// token's session is marked rotated (its access token stops working) and a
// new pair in the same family is issued. A refresh token that was already
// rotated is reuse — a stolen copy or a replay — and revokes the whole
// family (the device must log in again): ErrRefreshReused.
func (s *Service) RefreshAPI(ctx context.Context, refreshToken string) (TokenPair, error) {
	if !strings.HasPrefix(refreshToken, refreshPrefix) || len(refreshToken) > 128 {
		return TokenPair{}, ErrInvalidToken
	}
	pair, err := s.newPair()
	if err != nil {
		return TokenPair{}, err
	}
	reused := false
	err = s.repo.InTx(ctx, func(tx *Repository) error {
		row, err := tx.LockAPISessionByRefresh(ctx, s.tokenHash(refreshToken))
		if err != nil {
			return err
		}
		switch {
		case row.RevokedAt != nil:
			return ErrInvalidToken
		case row.RotatedAt != nil:
			reused = true
			return tx.RevokeAPIFamily(ctx, row.FamilyID, "REUSE_DETECTED") // commit the revocation
		case !s.now().Before(row.RefreshExpiresAt):
			return ErrInvalidToken
		}
		ok, err := tx.RotateAPISession(ctx, row.ID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidToken
		}
		_, err = tx.CreateAPISession(ctx, apiSessionRow{UserID: row.UserID, FamilyID: row.FamilyID,
			AccessHash: s.tokenHash(pair.AccessToken), RefreshHash: s.tokenHash(pair.RefreshToken),
			AccessExpires: pair.AccessExpiresAt, RefreshExpires: pair.RefreshExpiresAt, Device: row.Device})
		return err
	})
	if err != nil {
		return TokenPair{}, err
	}
	if reused {
		return TokenPair{}, ErrRefreshReused
	}
	return pair, nil
}

// LogoutAPI ends one API session (its whole family: current and rotated
// tokens). The user's web sessions and other devices are not affected.
func (s *Service) LogoutAPI(ctx context.Context, familyID int64) error {
	return s.repo.RevokeAPIFamily(ctx, familyID, "LOGOUT")
}

// LogoutAPIByRefresh is LogoutAPI for a client that only has its refresh
// token (e.g. its access token already expired). Unknown tokens are no-ops.
func (s *Service) LogoutAPIByRefresh(ctx context.Context, refreshToken string) error {
	if !strings.HasPrefix(refreshToken, refreshPrefix) || len(refreshToken) > 128 {
		return nil
	}
	return s.repo.InTx(ctx, func(tx *Repository) error {
		row, err := tx.LockAPISessionByRefresh(ctx, s.tokenHash(refreshToken))
		if errors.Is(err, ErrInvalidToken) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.RevokeAPIFamily(ctx, row.FamilyID, "LOGOUT")
	})
}

// LogoutAllAPI revokes every API session of the user (all devices) and
// returns how many were active. Web sessions are not affected.
func (s *Service) LogoutAllAPI(ctx context.Context, userID int64) (int64, error) {
	return s.repo.RevokeUserAPISessions(ctx, userID)
}

// CleanupAPISessions deletes expired API sessions and revoked ones older
// than a day (cleanup-api-sessions command, e.g. from a systemd timer).
func (s *Service) CleanupAPISessions(ctx context.Context) (int64, error) {
	return s.repo.DeleteStaleAPISessions(ctx)
}

// UserByID returns an account (API responses after registration).
func (s *Service) UserByID(ctx context.Context, id int64) (User, error) {
	return s.repo.UserByID(ctx, id)
}

func (s *Service) newPair() (TokenPair, error) {
	access, err := randomToken(accessPrefix)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, err := randomToken(refreshPrefix)
	if err != nil {
		return TokenPair{}, err
	}
	now := s.now().Truncate(time.Second)
	return TokenPair{AccessToken: access, RefreshToken: refresh,
		AccessExpiresAt: now.Add(s.accessTTL), RefreshExpiresAt: now.Add(max(s.refreshTTL, s.accessTTL))}, nil
}

// randomToken is prefix + 256 random bits, base64url.
func randomToken(prefix string) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func cleanDevice(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxDeviceName {
		s = string([]rune(s)[:maxDeviceName])
	}
	return s
}
