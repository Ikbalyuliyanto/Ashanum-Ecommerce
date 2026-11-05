package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAPISessions(t *testing.T) {
	s, ctx := newTestService(t)
	s.ConfigureAPI(15*time.Minute, 24*time.Hour)
	u, err := s.Register(ctx, RegisterInput{Email: "budi@example.com", Password: "rahasia123", PasswordConfirm: "rahasia123"})
	if err != nil {
		t.Fatal(err)
	}
	count := func(sql string, args ...any) (n int) {
		if err := s.repo.db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}

	if _, _, err := s.LoginAPI(ctx, "budi@example.com", "salah-sekali", "Pixel"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := s.LoginAPI(ctx, "tidak-ada@example.com", "rahasia123", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email: %v", err)
	}
	_, pair, err := s.LoginAPI(ctx, "BUDI@example.com", "rahasia123", "  Pixel   8 ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pair.AccessToken, accessPrefix) || !strings.HasPrefix(pair.RefreshToken, refreshPrefix) ||
		len(pair.AccessToken) != len(accessPrefix)+43 || pair.AccessToken == pair.RefreshToken {
		t.Fatalf("token format: %q %q", pair.AccessToken, pair.RefreshToken)
	}
	if got := pair.AccessExpiresAt.Sub(s.now()); got > 15*time.Minute || got < 14*time.Minute {
		t.Errorf("access TTL = %s", got)
	}

	// Only HMACs are stored; the device label is normalized.
	var access, refresh []byte
	var device string
	s.repo.db.QueryRow(ctx, "SELECT access_token_hash, refresh_token_hash, device_name FROM api_sessions").Scan(&access, &refresh, &device)
	if bytes.Contains(access, []byte(pair.AccessToken)) || !bytes.Equal(access, s.tokenHash(pair.AccessToken)) ||
		!bytes.Equal(refresh, s.tokenHash(pair.RefreshToken)) || device != "Pixel 8" {
		t.Errorf("stored hashes/device: %x %q", access, device)
	}

	p, err := s.AuthenticateAPI(ctx, pair.AccessToken)
	if err != nil || p.UserID != u.ID || p.Role != RoleCustomer {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	for _, bad := range []string{"", "ashat_tidak-ada", pair.RefreshToken, "Bearer " + pair.AccessToken, strings.Repeat("a", 200)} {
		if _, err := s.AuthenticateAPI(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("token %.20q: %v", bad, err)
		}
	}

	// Expiry: an expired access token asks for a refresh.
	real := s.now
	s.now = func() time.Time { return real().Add(16 * time.Minute) }
	if _, err := s.AuthenticateAPI(ctx, pair.AccessToken); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expired access token: %v", err)
	}
	s.now = real

	// Rotation: new pair works, the old pair does not.
	next, err := s.RefreshAPI(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPI(ctx, next.AccessToken); err != nil {
		t.Errorf("rotated access token: %v", err)
	}
	if _, err := s.AuthenticateAPI(ctx, pair.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("old access token after rotation: %v", err)
	}
	if got := count("SELECT count(DISTINCT family_id) FROM api_sessions"); got != 1 {
		t.Errorf("rotation must stay in one family, got %d", got)
	}

	// Reuse: replaying the rotated refresh token revokes the whole family.
	if _, err := s.RefreshAPI(ctx, pair.RefreshToken); !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("refresh reuse: %v", err)
	}
	if _, err := s.AuthenticateAPI(ctx, next.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("family not revoked after reuse: %v", err)
	}
	if _, err := s.RefreshAPI(ctx, next.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("refresh in a revoked family: %v", err)
	}
	if got := count("SELECT count(*) FROM api_sessions WHERE revoked_reason = 'REUSE_DETECTED'"); got != 2 {
		t.Errorf("revoked rows = %d, want 2", got)
	}

	// An expired refresh token is refused.
	_, old, _ := s.LoginAPI(ctx, "budi@example.com", "rahasia123", "")
	s.now = func() time.Time { return real().Add(25 * time.Hour) }
	if _, err := s.RefreshAPI(ctx, old.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired refresh token: %v", err)
	}
	s.now = real

	// Logout ends one device; logout-all ends every device; web untouched.
	_, a, _ := s.LoginAPI(ctx, "budi@example.com", "rahasia123", "A")
	_, b, _ := s.LoginAPI(ctx, "budi@example.com", "rahasia123", "B")
	web, err := s.StartSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := s.AuthenticateAPI(ctx, a.AccessToken)
	if err := s.LogoutAPI(ctx, pa.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPI(ctx, a.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("after logout: %v", err)
	}
	if _, err := s.RefreshAPI(ctx, a.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("refresh after logout: %v", err)
	}
	if _, err := s.AuthenticateAPI(ctx, b.AccessToken); err != nil {
		t.Errorf("other device after logout: %v", err)
	}
	if err := s.LogoutAPIByRefresh(ctx, b.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPI(ctx, b.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("after logout by refresh token: %v", err)
	}
	_, c, _ := s.LoginAPI(ctx, "budi@example.com", "rahasia123", "C")
	_, d, _ := s.LoginAPI(ctx, "budi@example.com", "rahasia123", "D")
	if n, err := s.LogoutAllAPI(ctx, u.ID); err != nil || n < 2 {
		t.Fatalf("logout all: %d %v", n, err)
	}
	for _, tok := range []string{c.AccessToken, d.AccessToken} {
		if _, err := s.AuthenticateAPI(ctx, tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("after logout-all: %v", err)
		}
	}
	if _, _, err := s.Authenticate(ctx, web.Token); err != nil {
		t.Errorf("API logout must not end the web session: %v", err)
	}
	if err := s.Logout(ctx, web.Token); err != nil {
		t.Fatal(err)
	}

	// Cleanup removes expired and long-revoked rows; logins purge expired ones.
	s.repo.db.Exec(ctx, "UPDATE api_sessions SET revoked_at = now() - interval '2 days' WHERE revoked_at IS NOT NULL")
	if n, err := s.CleanupAPISessions(ctx); err != nil || n == 0 {
		t.Errorf("cleanup: %d %v", n, err)
	}
	if got := count("SELECT count(*) FROM api_sessions WHERE revoked_at IS NOT NULL"); got != 0 {
		t.Errorf("revoked rows left: %d", got)
	}
	if _, _, err := s.LoginAPI(ctx, "budi@example.com", "rahasia123", "E"); err != nil {
		t.Fatal(err)
	}
	s.repo.db.Exec(ctx, "UPDATE api_sessions SET access_expires_at = now() - interval '2 hours', refresh_expires_at = now() - interval '1 hour'")
	before := count("SELECT count(*) FROM api_sessions")
	if _, _, err := s.LoginAPI(ctx, "budi@example.com", "rahasia123", ""); err != nil {
		t.Fatal(err)
	}
	if after := count("SELECT count(*) FROM api_sessions"); after != 1 || before == 0 {
		t.Errorf("lazy cleanup at login: %d → %d", before, after)
	}
}

func TestConcurrentRefreshRotatesOnce(t *testing.T) {
	s, ctx := newTestService(t)
	if _, err := s.Register(ctx, RegisterInput{Email: "a@example.com", Password: "rahasia123", PasswordConfirm: "rahasia123"}); err != nil {
		t.Fatal(err)
	}
	_, pair, err := s.LoginAPI(ctx, "a@example.com", "rahasia123", "")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 4)
	for range 4 {
		go func() { _, err := s.RefreshAPI(ctx, pair.RefreshToken); results <- err }()
	}
	okCount, reused := 0, 0
	for range 4 {
		switch err := <-results; {
		case err == nil:
			okCount++
		case errors.Is(err, ErrRefreshReused), errors.Is(err, ErrInvalidToken):
			reused++
		default:
			t.Errorf("concurrent refresh: %v", err)
		}
	}
	// At most one rotation wins; the others are treated as reuse, which
	// (strictly) revokes the family — the app must serialize refreshes.
	if okCount > 1 || okCount+reused != 4 {
		t.Errorf("concurrent refresh: %d ok, %d refused", okCount, reused)
	}
}
