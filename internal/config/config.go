// Package config loads application configuration from environment variables.
//
// In development a local .env file is read first (existing environment
// variables always win). In production the variables come from systemd's
// EnvironmentFile, so no secret ever lives in the source tree.
package config

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

type Config struct {
	Env  string
	Host string
	Port int
	Name string

	// BaseURL is the public site URL without a trailing slash, e.g.
	// https://ashanum.example; payment providers redirect buyers back to it.
	BaseURL string

	Database  Database
	Session   Session
	Midtrans  Midtrans
	Returns   Returns
	Inventory Inventory
	API       API
	Shipping  Shipping
}

type Database struct {
	URL      string
	MaxConns int32
	MinConns int32
}

// Session holds the key used to HMAC session tokens before storing them.
type Session struct {
	Secret []byte
	// Ephemeral is true when no SESSION_SECRET was set in development and a
	// random one was generated: sessions then end when the process restarts.
	Ephemeral bool
}

// Midtrans is the payment provider configuration. Only the sandbox is
// allowed: real-money payments stay off until the real Midtrans sandbox and
// the shipping provider have been tested end to end and a production
// payment review is done. The server key is used by the backend only.
type Midtrans struct {
	Env       string
	ServerKey string
}

const MidtransSandbox = "sandbox"

// Enabled reports whether online payment is configured.
func (m Midtrans) Enabled() bool { return m.ServerKey != "" }

// Returns holds the return policy. WindowDays is a business setting (how
// many days after an order is completed the buyer may ask for a return),
// not a fixed rule.
type Returns struct {
	WindowDays int
}

// Inventory holds stock reporting settings. LowStockThreshold marks a
// variant as "stok rendah" on the admin dashboard and reports (1..N units
// left); it triggers no notification or worker.
type Inventory struct {
	LowStockThreshold int
}

// API configures the REST API (/api/v1) used by mobile apps.
type API struct {
	// Token lifetimes: the access token is short, the refresh token long
	// (API_ACCESS_TOKEN_TTL, API_REFRESH_TOKEN_TTL; Go durations or "30d").
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// CORSOrigins are the browser origins allowed to call the API
	// (API_CORS_ALLOWED_ORIGINS, comma separated). Empty — the default — sends
	// no CORS headers: native apps do not need them. "*" is refused.
	CORSOrigins []string
	// PaymentFinishURL is where the payment provider sends the buyer back
	// after paying from the app (API_PAYMENT_FINISH_URL), e.g. an app link
	// https://shop.example/app/payment-finish?order={order_number} or
	// ashanum://payment/finish?order={order_number}. Server configured only:
	// clients can never choose it. Empty uses the web finish page.
	PaymentFinishURL string
}

// Shipping configures the shipping provider (Phase 10). Only Biteship is
// supported. Without BITESHIP_API_KEY shipping is off: checkout shows that
// the shipping cost cannot be calculated and no order can be placed (an
// order never gets shipping cost 0 as a fallback).
type Shipping struct {
	Provider string // SHIPPING_PROVIDER: "biteship"
	// LiveEnabled must be true for BITESHIP_ENV=live (real pickups, real
	// money). Default false: only the testing environment runs.
	LiveEnabled bool
	Biteship    Biteship
	Origin      ShipFrom
	// Couriers are the courier codes asked for rates (SHIPPING_COURIERS).
	Couriers []string
	// RateCacheTTL: provider rates reused for the same route and weight.
	RateCacheTTL time.Duration
	// QuoteTTL: how long a shipping cost shown at checkout can be ordered.
	QuoteTTL time.Duration
}

// Biteship holds the provider credentials; backend only (never rendered,
// sent to apps, or logged).
type Biteship struct {
	Env    string // "test" (biteship_test.* key) or "live"
	APIKey string
	// BaseURL replaces https://api.biteship.com; development only (a local
	// fake for browser tests).
	BaseURL string
	// WebhookHeader / WebhookSecret: the webhook "Headers Signature Key"
	// and "Secret" set in the Biteship dashboard. Empty secret: the webhook
	// accepts nothing.
	WebhookHeader string
	WebhookSecret string
}

// ShipFrom is the shop address where parcels are picked up.
type ShipFrom struct {
	Name       string
	Phone      string
	Address    string
	PostalCode string
	AreaID     string
	Latitude   string
	Longitude  string
}

const (
	ShippingBiteship = "biteship"
	BiteshipTest     = "test"
	BiteshipLive     = "live"
)

// Enabled reports whether a shipping provider is configured.
func (s Shipping) Enabled() bool { return s.Provider == ShippingBiteship && s.Biteship.APIKey != "" }

// minSessionSecret is the minimum SESSION_SECRET length (bytes).
const minSessionSecret = 32

func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// Load reads .env (if present) and the process environment, then validates.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, err
	}

	var errs []error
	cfg := &Config{
		Env:     getString("APP_ENV", EnvDevelopment),
		Host:    getString("APP_HOST", "127.0.0.1"),
		Port:    getInt("APP_PORT", 8080, &errs),
		Name:    getString("APP_NAME", "Ashanum"),
		BaseURL: strings.TrimRight(getString("APP_BASE_URL", ""), "/"),
		Midtrans: Midtrans{
			Env:       getString("MIDTRANS_ENV", MidtransSandbox),
			ServerKey: getString("MIDTRANS_SERVER_KEY", ""),
		},
		Returns:   Returns{WindowDays: getInt("RETURN_WINDOW_DAYS", 7, &errs)},
		Inventory: Inventory{LowStockThreshold: getInt("LOW_STOCK_THRESHOLD", 5, &errs)},
		API: API{
			AccessTTL:        getDuration("API_ACCESS_TOKEN_TTL", 15*time.Minute, &errs),
			RefreshTTL:       getDuration("API_REFRESH_TOKEN_TTL", 30*24*time.Hour, &errs),
			CORSOrigins:      getList("API_CORS_ALLOWED_ORIGINS"),
			PaymentFinishURL: getString("API_PAYMENT_FINISH_URL", ""),
		},
		Shipping: Shipping{
			Provider:    getString("SHIPPING_PROVIDER", ShippingBiteship),
			LiveEnabled: getString("SHIPPING_LIVE_ENABLED", "false") == "true",
			Biteship: Biteship{
				Env:           getString("BITESHIP_ENV", BiteshipTest),
				APIKey:        getString("BITESHIP_API_KEY", ""),
				BaseURL:       strings.TrimRight(getString("BITESHIP_BASE_URL", ""), "/"),
				WebhookHeader: getString("BITESHIP_WEBHOOK_HEADER", "X-Ashanum-Webhook-Secret"),
				WebhookSecret: getString("BITESHIP_WEBHOOK_SECRET", ""),
			},
			Origin: ShipFrom{
				Name:       getString("SHIP_FROM_NAME", ""),
				Phone:      getString("SHIP_FROM_PHONE", ""),
				Address:    getString("SHIP_FROM_ADDRESS", ""),
				PostalCode: getString("SHIP_FROM_POSTAL_CODE", ""),
				AreaID:     getString("SHIP_FROM_AREA_ID", ""),
				Latitude:   getString("SHIP_FROM_LATITUDE", ""),
				Longitude:  getString("SHIP_FROM_LONGITUDE", ""),
			},
			Couriers:     getList("SHIPPING_COURIERS"),
			RateCacheTTL: getDuration("SHIPPING_RATE_CACHE_TTL", 10*time.Minute, &errs),
			QuoteTTL:     getDuration("SHIPPING_QUOTE_TTL", 30*time.Minute, &errs),
		},
		Database: Database{
			URL:      getString("DATABASE_URL", ""),
			MaxConns: int32(getInt("DB_MAX_CONNS", 10, &errs)),
			MinConns: int32(getInt("DB_MIN_CONNS", 1, &errs)),
		},
	}

	if cfg.Env != EnvDevelopment && cfg.Env != EnvProduction {
		errs = append(errs, fmt.Errorf("APP_ENV must be %q or %q, got %q", EnvDevelopment, EnvProduction, cfg.Env))
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		errs = append(errs, fmt.Errorf("APP_PORT out of range: %d", cfg.Port))
	}
	if cfg.Database.URL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.Database.MaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be >= 1"))
	}
	if cfg.Database.MinConns < 0 || cfg.Database.MinConns > cfg.Database.MaxConns {
		errs = append(errs, errors.New("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS"))
	}

	errs = append(errs, cfg.validatePayments()...)
	if d := cfg.Returns.WindowDays; d < 1 || d > 365 {
		errs = append(errs, fmt.Errorf("RETURN_WINDOW_DAYS must be between 1 and 365, got %d", d))
	}
	if n := cfg.Inventory.LowStockThreshold; n < 0 || n > 10000 {
		errs = append(errs, fmt.Errorf("LOW_STOCK_THRESHOLD must be between 0 and 10000, got %d", n))
	}
	errs = append(errs, cfg.validateAPI()...)
	errs = append(errs, cfg.validateShipping()...)

	switch secret := getString("SESSION_SECRET", ""); {
	case secret != "" && len(secret) < minSessionSecret:
		errs = append(errs, fmt.Errorf("SESSION_SECRET must be at least %d characters", minSessionSecret))
	case secret != "":
		cfg.Session.Secret = []byte(secret)
	case cfg.Env == EnvProduction:
		errs = append(errs, errors.New("SESSION_SECRET is required in production"))
	default:
		cfg.Session.Secret = make([]byte, minSessionSecret)
		rand.Read(cfg.Session.Secret)
		cfg.Session.Ephemeral = true
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// validatePayments checks MIDTRANS_* and APP_BASE_URL. Error messages never
// include the server key.
func (c *Config) validatePayments() []error {
	var errs []error
	if c.Midtrans.Env != MidtransSandbox {
		errs = append(errs, fmt.Errorf("MIDTRANS_ENV must be %q: production payments stay disabled until the real sandbox and shipping are tested end to end and the production payment review is done", MidtransSandbox))
	}
	if k := c.Midtrans.ServerKey; k != "" && !strings.HasPrefix(k, "SB-Mid-server-") {
		errs = append(errs, errors.New("MIDTRANS_SERVER_KEY must be a sandbox server key (SB-Mid-server-...)"))
	}

	if c.BaseURL == "" {
		if c.IsProduction() && c.Midtrans.Enabled() {
			errs = append(errs, errors.New("APP_BASE_URL is required in production when payments are enabled"))
		}
		c.BaseURL = "http://" + c.Addr()
		return errs
	}
	u, err := url.Parse(c.BaseURL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") || u.RawQuery != "":
		errs = append(errs, fmt.Errorf("APP_BASE_URL must be an absolute URL such as https://shop.example, got %q", c.BaseURL))
	case c.IsProduction() && u.Scheme != "https":
		errs = append(errs, errors.New("APP_BASE_URL must use https in production"))
	}
	return errs
}

// validateAPI checks the API_* settings.
func (c *Config) validateAPI() []error {
	var errs []error
	a := c.API
	if a.AccessTTL < time.Minute || a.AccessTTL > 24*time.Hour {
		errs = append(errs, fmt.Errorf("API_ACCESS_TOKEN_TTL must be between 1m and 24h, got %s", a.AccessTTL))
	}
	if a.RefreshTTL < time.Hour || a.RefreshTTL > 365*24*time.Hour || a.RefreshTTL <= a.AccessTTL {
		errs = append(errs, fmt.Errorf("API_REFRESH_TOKEN_TTL must be between 1h and 365d and longer than the access token, got %s", a.RefreshTTL))
	}
	for _, o := range a.CORSOrigins {
		u, err := url.Parse(o)
		switch {
		case o == "*" || strings.Contains(o, "*"):
			errs = append(errs, errors.New("API_CORS_ALLOWED_ORIGINS must list exact origins, never *"))
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") ||
			u.RawQuery != "" || u.User != nil:
			errs = append(errs, fmt.Errorf("API_CORS_ALLOWED_ORIGINS: %q is not an origin such as https://app.example", o))
		case c.IsProduction() && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("API_CORS_ALLOWED_ORIGINS: %q must use https in production", o))
		}
	}
	if f := a.PaymentFinishURL; f != "" {
		u, err := url.Parse(strings.ReplaceAll(f, "{order_number}", "X"))
		scheme := ""
		if err == nil {
			scheme = strings.ToLower(u.Scheme)
		}
		switch {
		case err != nil || scheme == "" || len(f) > 300 || strings.ContainsAny(f, " \t\r\n\"'<>"):
			errs = append(errs, errors.New("API_PAYMENT_FINISH_URL must be an absolute URL or app link"))
		case scheme == "javascript" || scheme == "data" || scheme == "file" || scheme == "vbscript":
			errs = append(errs, fmt.Errorf("API_PAYMENT_FINISH_URL: scheme %q is not allowed", scheme))
		case (scheme == "https" || scheme == "http") && u.Host == "":
			errs = append(errs, errors.New("API_PAYMENT_FINISH_URL needs a host"))
		case scheme == "http" && c.IsProduction():
			errs = append(errs, errors.New("API_PAYMENT_FINISH_URL must use https (or an app scheme) in production"))
		}
	}
	return errs
}

var courierCode = regexp.MustCompile(`^[a-z][a-z0-9_]{1,29}$`)

// validateShipping checks SHIPPING_*, BITESHIP_* and SHIP_FROM_*. Error
// messages never include the API key or the webhook secret.
func (c *Config) validateShipping() []error {
	var errs []error
	s := &c.Shipping
	if s.Provider != ShippingBiteship {
		errs = append(errs, fmt.Errorf("SHIPPING_PROVIDER must be %q, got %q", ShippingBiteship, s.Provider))
	}
	if v := getString("SHIPPING_LIVE_ENABLED", "false"); v != "true" && v != "false" {
		errs = append(errs, fmt.Errorf("SHIPPING_LIVE_ENABLED must be true or false, got %q", v))
	}
	b := s.Biteship
	switch b.Env {
	case BiteshipTest:
		if b.APIKey != "" && !strings.HasPrefix(b.APIKey, "biteship_test.") {
			errs = append(errs, errors.New("BITESHIP_API_KEY must be a testing key (biteship_test.…) when BITESHIP_ENV=test"))
		}
	case BiteshipLive:
		if !s.LiveEnabled {
			errs = append(errs, errors.New("BITESHIP_ENV=live is refused: set SHIPPING_LIVE_ENABLED=true only after the shipping flow was tested with a testing key"))
		}
		if b.APIKey != "" && !strings.HasPrefix(b.APIKey, "biteship_live.") {
			errs = append(errs, errors.New("BITESHIP_API_KEY must be a live key (biteship_live.…) when BITESHIP_ENV=live"))
		}
	default:
		errs = append(errs, fmt.Errorf("BITESHIP_ENV must be %q or %q, got %q", BiteshipTest, BiteshipLive, b.Env))
	}
	if b.BaseURL != "" {
		u, err := url.Parse(b.BaseURL)
		switch {
		case c.IsProduction():
			errs = append(errs, errors.New("BITESHIP_BASE_URL is for local testing only; leave it empty in production"))
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			errs = append(errs, fmt.Errorf("BITESHIP_BASE_URL must be an absolute URL, got %q", b.BaseURL))
		}
	}
	if b.WebhookSecret != "" && len(b.WebhookSecret) < 24 {
		errs = append(errs, errors.New("BITESHIP_WEBHOOK_SECRET must be at least 24 characters"))
	}
	if b.WebhookHeader != "" && !httpToken(b.WebhookHeader) {
		errs = append(errs, fmt.Errorf("BITESHIP_WEBHOOK_HEADER must be an HTTP header name, got %q", b.WebhookHeader))
	}
	if len(s.Couriers) == 0 {
		s.Couriers = []string{"jne", "sicepat", "jnt", "anteraja", "pos"}
	}
	for _, code := range s.Couriers {
		if !courierCode.MatchString(code) {
			errs = append(errs, fmt.Errorf("SHIPPING_COURIERS: %q is not a courier code such as jne", code))
		}
	}
	if s.RateCacheTTL < 5*time.Minute || s.RateCacheTTL > 15*time.Minute {
		errs = append(errs, fmt.Errorf("SHIPPING_RATE_CACHE_TTL must be between 5m and 15m, got %s", s.RateCacheTTL))
	}
	if s.QuoteTTL < 5*time.Minute || s.QuoteTTL > 2*time.Hour {
		errs = append(errs, fmt.Errorf("SHIPPING_QUOTE_TTL must be between 5m and 2h, got %s", s.QuoteTTL))
	}

	if !s.Enabled() {
		return errs // checkout then refuses orders: never shipping cost 0
	}
	o := s.Origin
	if l := len([]rune(o.Name)); l < 2 || l > 100 {
		errs = append(errs, errors.New("SHIP_FROM_NAME is required (2–100 characters) when shipping is enabled"))
	}
	if !phoneLike(o.Phone) {
		errs = append(errs, errors.New("SHIP_FROM_PHONE must be a phone number such as 081234567890"))
	}
	if l := len([]rune(o.Address)); l < 10 || l > 300 {
		errs = append(errs, errors.New("SHIP_FROM_ADDRESS is required (10–300 characters) when shipping is enabled"))
	}
	if !digits(o.PostalCode, 5) {
		errs = append(errs, errors.New("SHIP_FROM_POSTAL_CODE must be 5 digits"))
	}
	if len(o.AreaID) > 100 {
		errs = append(errs, errors.New("SHIP_FROM_AREA_ID is too long"))
	}
	if (o.Latitude == "") != (o.Longitude == "") {
		errs = append(errs, errors.New("SHIP_FROM_LATITUDE and SHIP_FROM_LONGITUDE go together"))
	} else if o.Latitude != "" {
		lat, err1 := strconv.ParseFloat(o.Latitude, 64)
		lng, err2 := strconv.ParseFloat(o.Longitude, 64)
		if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
			errs = append(errs, errors.New("SHIP_FROM_LATITUDE/LONGITUDE must be decimal coordinates"))
		}
	}
	return errs
}

func httpToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func phoneLike(s string) bool {
	s = strings.TrimPrefix(s, "+")
	return len(s) >= 9 && len(s) <= 15 && digits(s, len(s))
}

func digits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// getDuration reads a Go duration ("15m", "720h") or whole days ("30d").
func getDuration(key string, def time.Duration, errs *[]error) time.Duration {
	v := getString(key, "")
	if v == "" {
		return def
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be a duration such as 15m, 720h or 30d, got %q", key, v))
		return def
	}
	return d
}

// getList reads a comma-separated list, dropping empty entries.
func getList(key string) []string {
	var out []string
	for _, p := range strings.Split(getString(key, ""), ",") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func getInt(key string, def int, errs *[]error) int {
	v := getString(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be an integer, got %q", key, v))
		return def
	}
	return n
}

// loadDotEnv sets KEY=VALUE pairs from path without overriding variables that
// are already present. A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return fmt.Errorf("config: %s line %d: expected KEY=VALUE", path, n)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
