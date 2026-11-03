package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"ecommerce/internal/testutil"
	"ecommerce/internal/validate"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func newTestService(t *testing.T) (*Service, context.Context) {
	db := testutil.DB(t)
	return NewService(NewRepository(db), testSecret), context.Background()
}

func TestRegister(t *testing.T) {
	svc, ctx := newTestService(t)

	u, err := svc.Register(ctx, RegisterInput{Email: " Budi@Example.com ", Password: "rahasia123", PasswordConfirm: "rahasia123"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.Email != "budi@example.com" || u.Role != RoleUser {
		t.Errorf("unexpected user: %+v", u)
	}

	t.Run("duplicate email, any case", func(t *testing.T) {
		_, err := svc.Register(ctx, RegisterInput{Email: "BUDI@example.com", Password: "lainnya123", PasswordConfirm: "lainnya123"})
		if !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("err = %v, want ErrEmailTaken", err)
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		_, err := svc.Register(ctx, RegisterInput{Email: "bukan-email", Password: "pendek", PasswordConfirm: "beda"})
		var ve validate.Errors
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want validate.Errors", err)
		}
		for _, f := range []string{"email", "password", "password_confirm"} {
			if ve[f] == "" {
				t.Errorf("missing error for %s", f)
			}
		}
	})
}

func TestLoginLogout(t *testing.T) {
	svc, ctx := newTestService(t)
	if _, err := svc.Register(ctx, RegisterInput{Email: "siti@example.com", Password: "rahasia123", PasswordConfirm: "rahasia123"}); err != nil {
		t.Fatal(err)
	}

	for name, pw := range map[string][2]string{
		"wrong password": {"siti@example.com", "salah12345"},
		"unknown email":  {"tidakada@example.com", "rahasia123"},
		"empty password": {"siti@example.com", ""},
	} {
		if _, _, err := svc.Login(ctx, pw[0], pw[1]); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", name, err)
		}
	}

	u, sess, err := svc.Login(ctx, "SITI@example.com", "rahasia123")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Token) < 40 || time.Until(sess.ExpiresAt) < SessionTTL-time.Minute {
		t.Errorf("unexpected session: %+v", sess)
	}

	p, renewed, err := svc.Authenticate(ctx, sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != u.ID || p.Email != "siti@example.com" || renewed != nil {
		t.Errorf("principal = %+v, renewed = %v", p, renewed)
	}

	if err := svc.Logout(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authenticate(ctx, sess.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("after logout: err = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionStorageAndLifetime(t *testing.T) {
	db := testutil.DB(t)
	svc, ctx := NewService(NewRepository(db), testSecret), context.Background()
	uid := testutil.CreateUser(t, db, "andi@example.com")

	sess, err := svc.StartSession(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}

	// Only the HMAC of the token is stored, never the token itself.
	var raw int
	db.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE token_hash = $1", []byte(sess.Token)).Scan(&raw)
	if raw != 0 {
		t.Error("raw token must not be stored")
	}

	// Past half of its lifetime the session is extended.
	svc.now = func() time.Time { return time.Now().Add(20 * 24 * time.Hour) }
	_, renewed, err := svc.Authenticate(ctx, sess.Token)
	if err != nil || renewed == nil || !renewed.ExpiresAt.After(sess.ExpiresAt) {
		t.Fatalf("expected renewal, got %v, %v", renewed, err)
	}
	svc.now = time.Now

	// An expired session is rejected.
	db.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second'")
	if _, _, err := svc.Authenticate(ctx, sess.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expired: err = %v", err)
	}

	// A different secret cannot resolve the same token.
	other := NewService(NewRepository(db), []byte("ffffffffffffffffffffffffffffffff"))
	sess2, _ := svc.StartSession(ctx, uid)
	if _, _, err := other.Authenticate(ctx, sess2.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("other secret: err = %v", err)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/account":               "/account",
		"/account/addresses?x=1": "/account/addresses?x=1",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"https://evil.example/":  "/",
		"javascript:alert(1)":    "/",
		"/ok\r\nSet-Cookie: x=1": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
