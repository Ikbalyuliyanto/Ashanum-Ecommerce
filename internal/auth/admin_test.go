package auth

import (
	"errors"
	"testing"

	"ecommerce/internal/validate"
)

func TestCreateAdmin(t *testing.T) {
	s, ctx := newTestService(t)
	isField := func(err error, key string) bool {
		var v validate.Errors
		return errors.As(err, &v) && v[key] != ""
	}

	// Stricter password than customers (12+), a name is required.
	if _, err := s.CreateAdmin(ctx, AdminInput{Email: "admin@ashanum.id", Password: "rahasia123", FirstName: "Rina"}); !isField(err, "password") {
		t.Errorf("short admin password: %v", err)
	}
	if _, err := s.CreateAdmin(ctx, AdminInput{Email: "admin@ashanum.id", Password: "kata-sandi-panjang-1"}); !isField(err, "first_name") {
		t.Errorf("admin without a name: %v", err)
	}
	res, err := s.CreateAdmin(ctx, AdminInput{Email: "Admin@Ashanum.id", Password: "kata-sandi-panjang-1", FirstName: "Rina", LastName: "Wati"})
	if err != nil || !res.Created || res.User.Role != RoleAdmin || res.User.Email != "admin@ashanum.id" {
		t.Fatalf("create admin: %+v %v", res, err)
	}
	u, sess, err := s.Login(ctx, "admin@ashanum.id", "kata-sandi-panjang-1")
	if err != nil || u.Role != RoleAdmin {
		t.Fatalf("admin login: %+v %v", u, err)
	}
	if p, _, err := s.Authenticate(ctx, sess.Token); err != nil || !p.IsAdmin() {
		t.Errorf("admin session: %+v %v", p, err)
	}

	// An existing account is never promoted silently.
	cust, err := s.Register(ctx, RegisterInput{Email: "budi@example.com", Password: "rahasia123", PasswordConfirm: "rahasia123"})
	if err != nil || cust.Role != RoleCustomer {
		t.Fatalf("register: %+v %v", cust, err)
	}
	if _, err := s.CreateAdmin(ctx, AdminInput{Email: "budi@example.com", Password: "kata-sandi-panjang-1", FirstName: "Budi"}); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("existing email without promote: %v", err)
	}
	if _, err := s.CreateAdmin(ctx, AdminInput{Email: "tidak-ada@example.com", Promote: true}); !errors.Is(err, ErrUnknownEmail) {
		t.Errorf("promote an unknown email: %v", err)
	}
	res, err = s.CreateAdmin(ctx, AdminInput{Email: "budi@example.com", Promote: true})
	if err != nil || !res.Promoted {
		t.Fatalf("promote: %+v %v", res, err)
	}
	if u, _, err := s.Login(ctx, "budi@example.com", "rahasia123"); err != nil || u.Role != RoleAdmin {
		t.Errorf("promoted account keeps its password and is admin: %+v %v", u, err)
	}
	if res, err := s.CreateAdmin(ctx, AdminInput{Email: "budi@example.com", Promote: true}); err != nil || res.Promoted || res.Created {
		t.Errorf("promote twice: %+v %v", res, err)
	}

	var n int
	s.repo.db.QueryRow(ctx, "SELECT count(*) FROM admin_audit_logs WHERE action IN ('user.create_admin', 'user.grant_admin')").Scan(&n)
	if n != 2 {
		t.Errorf("audit entries = %d, want 2", n)
	}
	var hash string
	s.repo.db.QueryRow(ctx, "SELECT password_hash FROM users WHERE email = 'admin@ashanum.id'").Scan(&hash)
	if hash == "" || hash == "kata-sandi-panjang-1" {
		t.Error("the admin password must be stored hashed")
	}
}
