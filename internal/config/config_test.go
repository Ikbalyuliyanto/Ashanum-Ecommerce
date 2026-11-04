package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nCFG_TEST_A=from-file\nCFG_TEST_B=\"quoted value\"\nexport CFG_TEST_C=exported\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFG_TEST_A", "from-env")
	t.Setenv("CFG_TEST_B", "")
	os.Unsetenv("CFG_TEST_B")
	t.Setenv("CFG_TEST_C", "")
	os.Unsetenv("CFG_TEST_C")

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CFG_TEST_A"); got != "from-env" {
		t.Errorf("A = %q, want existing env to win", got)
	}
	if got := os.Getenv("CFG_TEST_B"); got != "quoted value" {
		t.Errorf("B = %q, want quotes stripped", got)
	}
	if got := os.Getenv("CFG_TEST_C"); got != "exported" {
		t.Errorf("C = %q, want export prefix handled", got)
	}
}

// TestEnvExampleIsValid keeps .env.example loadable as-is (no inline
// comments, which systemd's EnvironmentFile would also reject).
func TestEnvExampleIsValid(t *testing.T) {
	example, err := filepath.Abs("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"APP_ENV", "APP_HOST", "APP_PORT", "APP_NAME", "APP_BASE_URL", "DATABASE_URL", "DB_MAX_CONNS", "DB_MIN_CONNS", "SESSION_SECRET", "MIDTRANS_ENV", "MIDTRANS_SERVER_KEY", "MIDTRANS_CLIENT_KEY", "RETURN_WINDOW_DAYS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := loadDotEnv(example); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if _, err := Load(); err != nil {
		t.Fatalf(".env.example does not produce a valid config: %v", err)
	}
}

func TestLoadValidation(t *testing.T) {
	t.Chdir(t.TempDir()) // no .env here
	t.Setenv("APP_ENV", "staging")
	t.Setenv("APP_PORT", "abc")
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected validation error")
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_PORT", "9000")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/shop")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsProduction() || cfg.Addr() != "127.0.0.1:9000" || cfg.Database.MaxConns != 10 {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestSessionSecretRules(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/shop")

	t.Setenv("APP_ENV", "production")
	t.Setenv("SESSION_SECRET", "")
	if _, err := Load(); err == nil {
		t.Error("production without SESSION_SECRET must fail")
	}

	t.Setenv("SESSION_SECRET", "too-short")
	if _, err := Load(); err == nil {
		t.Error("short SESSION_SECRET must fail")
	}

	t.Setenv("APP_ENV", "development")
	t.Setenv("SESSION_SECRET", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Session.Ephemeral || len(cfg.Session.Secret) != minSessionSecret {
		t.Errorf("development without secret should get an ephemeral one: %+v", cfg.Session)
	}
}

func TestPaymentConfigRules(t *testing.T) {
	t.Chdir(t.TempDir())
	for k, v := range map[string]string{"APP_ENV": "development", "APP_HOST": "", "APP_PORT": "", "SESSION_SECRET": "",
		"DATABASE_URL": "postgres://u:p@db:5432/shop", "APP_BASE_URL": "", "MIDTRANS_ENV": "", "MIDTRANS_SERVER_KEY": ""} {
		t.Setenv(k, v)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Midtrans.Enabled() || cfg.Midtrans.Env != MidtransSandbox || cfg.BaseURL != "http://127.0.0.1:8080" {
		t.Errorf("defaults: payments off, sandbox, local base URL; got %+v %q", cfg.Midtrans, cfg.BaseURL)
	}

	t.Setenv("MIDTRANS_ENV", "production")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MIDTRANS_ENV") {
		t.Errorf("production Midtrans must be refused, got %v", err)
	}
	t.Setenv("MIDTRANS_ENV", "sandbox")

	t.Setenv("MIDTRANS_SERVER_KEY", "Mid-server-LIVEKEY123")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "LIVEKEY123") {
		t.Errorf("non-sandbox key must be refused without echoing it, got %v", err)
	}

	t.Setenv("MIDTRANS_SERVER_KEY", "SB-Mid-server-abc")
	t.Setenv("APP_BASE_URL", "https://toko.example/")
	if cfg, err := Load(); err != nil || !cfg.Midtrans.Enabled() || cfg.BaseURL != "https://toko.example" {
		t.Errorf("sandbox config: %v %+v", err, cfg)
	}
	for _, bad := range []string{"toko.example", "ftp://toko.example", "https://toko.example/shop", "https://toko.example?x=1"} {
		t.Setenv("APP_BASE_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("APP_BASE_URL %q must be refused", bad)
		}
	}

	t.Setenv("APP_ENV", "production")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("APP_BASE_URL", "")
	if _, err := Load(); err == nil {
		t.Error("production with payments needs APP_BASE_URL")
	}
	t.Setenv("APP_BASE_URL", "http://toko.example")
	if _, err := Load(); err == nil {
		t.Error("production APP_BASE_URL must be https")
	}
	t.Setenv("APP_BASE_URL", "https://toko.example")
	if _, err := Load(); err != nil {
		t.Errorf("production sandbox with https base URL: %v", err)
	}
}

func TestReturnWindowConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/shop")
	t.Setenv("RETURN_WINDOW_DAYS", "")
	if cfg, err := Load(); err != nil || cfg.Returns.WindowDays != 7 {
		t.Errorf("default return window: %v %+v", err, cfg)
	}
	t.Setenv("RETURN_WINDOW_DAYS", "14")
	if cfg, err := Load(); err != nil || cfg.Returns.WindowDays != 14 {
		t.Errorf("configured return window: %v %+v", err, cfg)
	}
	for _, bad := range []string{"0", "-3", "400", "tujuh"} {
		t.Setenv("RETURN_WINDOW_DAYS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("RETURN_WINDOW_DAYS=%s must be refused", bad)
		}
	}
}
