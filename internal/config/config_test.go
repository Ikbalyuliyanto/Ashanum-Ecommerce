package config

import (
	"os"
	"path/filepath"
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
	for _, k := range []string{"APP_ENV", "APP_HOST", "APP_PORT", "APP_NAME", "DATABASE_URL", "DB_MAX_CONNS", "DB_MIN_CONNS"} {
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
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsProduction() || cfg.Addr() != "127.0.0.1:9000" || cfg.Database.MaxConns != 10 {
		t.Errorf("unexpected config: %+v", cfg)
	}
}
