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
	"os"
	"strconv"
	"strings"
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

	Database Database
	Session  Session
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
		Env:  getString("APP_ENV", EnvDevelopment),
		Host: getString("APP_HOST", "127.0.0.1"),
		Port: getInt("APP_PORT", 8080, &errs),
		Name: getString("APP_NAME", "Ashanum"),
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
