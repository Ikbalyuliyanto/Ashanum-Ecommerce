package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLowStockConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/shop")
	t.Setenv("LOW_STOCK_THRESHOLD", "")
	cfg, err := Load()
	if err != nil || cfg.Inventory.LowStockThreshold != 5 {
		t.Fatalf("default LOW_STOCK_THRESHOLD: %v %+v", err, cfg)
	}
	t.Setenv("LOW_STOCK_THRESHOLD", "12")
	if cfg, err := Load(); err != nil || cfg.Inventory.LowStockThreshold != 12 {
		t.Fatalf("LOW_STOCK_THRESHOLD=12: %v", err)
	}
	for _, bad := range []string{"-1", "10001", "lima"} {
		t.Setenv("LOW_STOCK_THRESHOLD", bad)
		if _, err := Load(); err == nil {
			t.Errorf("LOW_STOCK_THRESHOLD=%s accepted", bad)
		}
	}
}

func TestAPIConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/shop")
	for _, k := range []string{"API_ACCESS_TOKEN_TTL", "API_REFRESH_TOKEN_TTL", "API_CORS_ALLOWED_ORIGINS", "API_PAYMENT_FINISH_URL"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil || cfg.API.AccessTTL != 15*time.Minute || cfg.API.RefreshTTL != 30*24*time.Hour || len(cfg.API.CORSOrigins) != 0 {
		t.Fatalf("defaults: %v %+v", err, cfg.API)
	}
	t.Setenv("API_ACCESS_TOKEN_TTL", "10m")
	t.Setenv("API_REFRESH_TOKEN_TTL", "14d")
	t.Setenv("API_CORS_ALLOWED_ORIGINS", "http://localhost:5173, https://admin.example/")
	t.Setenv("API_PAYMENT_FINISH_URL", "ashanum://payment/finish?order={order_number}")
	cfg, err = Load()
	if err != nil || cfg.API.AccessTTL != 10*time.Minute || cfg.API.RefreshTTL != 14*24*time.Hour ||
		len(cfg.API.CORSOrigins) != 2 || cfg.API.CORSOrigins[1] != "https://admin.example" {
		t.Fatalf("custom: %v %+v", err, cfg.API)
	}
	bad := map[string]string{
		"API_ACCESS_TOKEN_TTL":     "10s",
		"API_REFRESH_TOKEN_TTL":    "5m",
		"API_CORS_ALLOWED_ORIGINS": "*",
		"API_PAYMENT_FINISH_URL":   "javascript:alert(1)",
	}
	for k, v := range bad {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := Load(); err == nil {
				t.Errorf("%s=%s accepted", k, v)
			}
		})
	}
	t.Setenv("API_CORS_ALLOWED_ORIGINS", "https://a.example/path")
	if _, err := Load(); err == nil {
		t.Error("an origin with a path accepted")
	}
}

func TestShippingConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/shop")
	for _, k := range []string{"SHIPPING_PROVIDER", "BITESHIP_API_KEY", "BITESHIP_ENV", "SHIPPING_LIVE_ENABLED", "BITESHIP_BASE_URL",
		"BITESHIP_WEBHOOK_HEADER", "BITESHIP_WEBHOOK_SECRET", "SHIP_FROM_NAME", "SHIP_FROM_PHONE", "SHIP_FROM_ADDRESS",
		"SHIP_FROM_POSTAL_CODE", "SHIP_FROM_AREA_ID", "SHIP_FROM_LATITUDE", "SHIP_FROM_LONGITUDE", "SHIPPING_COURIERS",
		"SHIPPING_RATE_CACHE_TTL", "SHIPPING_QUOTE_TTL"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil || cfg.Shipping.Enabled() || cfg.Shipping.Biteship.Env != BiteshipTest || cfg.Shipping.LiveEnabled ||
		len(cfg.Shipping.Couriers) != 5 || cfg.Shipping.RateCacheTTL != 10*time.Minute || cfg.Shipping.QuoteTTL != 30*time.Minute {
		t.Fatalf("defaults (no key: shipping off, test env): %v %+v", err, cfg.Shipping)
	}

	t.Setenv("BITESHIP_API_KEY", "biteship_test.secret-value")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SHIP_FROM_NAME") {
		t.Errorf("a key needs the shop origin: %v", err)
	}
	t.Setenv("SHIP_FROM_NAME", "Toko Ashanum")
	t.Setenv("SHIP_FROM_PHONE", "081200000001")
	t.Setenv("SHIP_FROM_ADDRESS", "Jl. Asia Afrika No. 8, Bandung")
	t.Setenv("SHIP_FROM_POSTAL_CODE", "40111")
	if cfg, err := Load(); err != nil || !cfg.Shipping.Enabled() {
		t.Fatalf("testing key with origin: %v", err)
	}

	// Live shipping only when explicitly enabled; keys must match the
	// environment; errors never echo the key.
	t.Setenv("BITESHIP_ENV", "live")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SHIPPING_LIVE_ENABLED") || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("live without SHIPPING_LIVE_ENABLED: %v", err)
	}
	t.Setenv("SHIPPING_LIVE_ENABLED", "true")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("live env with a testing key: %v", err)
	}
	t.Setenv("BITESHIP_API_KEY", "biteship_live.other-secret")
	if cfg, err := Load(); err != nil || !cfg.Shipping.LiveEnabled {
		t.Errorf("explicitly enabled live: %v", err)
	}
	t.Setenv("BITESHIP_ENV", "test")
	t.Setenv("SHIPPING_LIVE_ENABLED", "")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "other-secret") {
		t.Errorf("test env with a live key: %v", err)
	}
	t.Setenv("BITESHIP_API_KEY", "biteship_test.secret-value")

	bad := map[string]string{
		"SHIPPING_PROVIDER":       "rajaongkir",
		"SHIPPING_LIVE_ENABLED":   "yes",
		"BITESHIP_ENV":            "sandbox",
		"BITESHIP_WEBHOOK_SECRET": "short",
		"BITESHIP_WEBHOOK_HEADER": "X Bad Header",
		"BITESHIP_BASE_URL":       "not a url",
		"SHIP_FROM_POSTAL_CODE":   "4011",
		"SHIP_FROM_PHONE":         "telepon",
		"SHIP_FROM_LATITUDE":      "-6.9",
		"SHIPPING_COURIERS":       "jne,J&T",
		"SHIPPING_RATE_CACHE_TTL": "1h",
		"SHIPPING_QUOTE_TTL":      "1m",
	}
	for k, v := range bad {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := Load(); err == nil {
				t.Errorf("%s=%s accepted", k, v)
			}
		})
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("BITESHIP_BASE_URL", "http://127.0.0.1:9000")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BITESHIP_BASE_URL") {
		t.Errorf("a fake provider URL in production: %v", err)
	}
}
