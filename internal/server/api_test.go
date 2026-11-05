package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/config"
	"ecommerce/internal/testutil"
)

// apiClient is a mobile-app-like JSON client: Bearer token, no cookies.
type apiClient struct {
	t       *testing.T
	base    string
	token   string
	refresh string
}

type apiRes struct {
	status int
	header http.Header
	raw    string
	body   map[string]any
}

func newAPI(t *testing.T, base string) *apiClient { return &apiClient{t: t, base: base + "/api/v1"} }

// call sends body as JSON (a string is sent as is); headers are key, value
// pairs.
func (c *apiClient) call(method, path string, body any, headers ...string) apiRes {
	c.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := apiRes{status: res.StatusCode, header: res.Header, raw: string(raw)}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			c.t.Fatalf("%s %s: not JSON (%d): %s", method, path, res.StatusCode, raw)
		}
	}
	return out
}

func (c *apiClient) get(path string) apiRes { c.t.Helper(); return c.call(http.MethodGet, path, nil) }
func (c *apiClient) post(path string, body any) apiRes {
	c.t.Helper()
	return c.call(http.MethodPost, path, body)
}

func (r apiRes) data() map[string]any { m, _ := r.body["data"].(map[string]any); return m }
func (r apiRes) list() []any          { l, _ := r.body["data"].([]any); return l }
func (r apiRes) meta() map[string]any { m, _ := r.body["meta"].(map[string]any); return m }
func (r apiRes) code() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}
func (r apiRes) field(name string) string {
	e, _ := r.body["error"].(map[string]any)
	f, _ := e["fields"].(map[string]any)
	s, _ := f[name].(string)
	return s
}

func want(t *testing.T, r apiRes, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		t.Fatalf("status %d code %q, want %d %q\n%s", r.status, r.code(), status, code, r.raw)
	}
}

// signIn registers (or logs in) and keeps the tokens.
// area returns the first delivery area found for q (GET /shipping/areas),
// the id an address saves as shipping_area_id.
func (c *apiClient) area(q string) string {
	c.t.Helper()
	res := c.get("/shipping/areas?q=" + url.QueryEscape(q))
	l := res.list()
	if res.status != http.StatusOK || len(l) == 0 {
		c.t.Fatalf("area search %q: %s", q, res.raw)
	}
	return l[0].(map[string]any)["id"].(string)
}

// quote previews a checkout and returns the selected shipping quote id and
// the total (items + shipping) to send with POST /orders.
func (c *apiClient) quote(body map[string]any) (string, float64) {
	c.t.Helper()
	res := c.post("/checkout/preview", body)
	sh, _ := res.data()["shipping"].(map[string]any)
	sel, _ := sh["selected"].(map[string]any)
	if res.status != http.StatusOK || sel == nil {
		c.t.Fatalf("preview without shipping: %s", res.raw)
	}
	return sel["quote_id"].(string), res.data()["total"].(float64)
}

func (c *apiClient) signIn(email string) {
	c.t.Helper()
	res := c.post("/auth/register", map[string]any{"email": email, "password": "rahasia123", "device_name": "Test"})
	if res.status == http.StatusConflict {
		res = c.post("/auth/login", map[string]any{"email": email, "password": "rahasia123"})
	}
	if res.status != http.StatusCreated && res.status != http.StatusOK {
		c.t.Fatalf("sign in %s: %d %s", email, res.status, res.raw)
	}
	c.token, _ = res.data()["access_token"].(string)
	c.refresh, _ = res.data()["refresh_token"].(string)
}

func num(v any) int64 { f, _ := v.(float64); return int64(f) }

func newAPIApp(t *testing.T, mod func(*config.Config)) (string, *pgxpool.Pool) {
	db := testutil.DB(t)
	srv := httptest.NewServer(buildRouterCfg(t, db, config.EnvDevelopment, nil, "", mod))
	t.Cleanup(srv.Close)
	return srv.URL, db
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var rfc3339WIB = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+07:00$`)

func TestAPIAuthAndSecurity(t *testing.T) {
	base, db := newAPIApp(t, nil)
	c := newAPI(t, base)

	// Register: 201, tokens, the user — never a password or hash.
	res := c.post("/auth/register", map[string]any{"email": "Budi@Example.com", "password": "rahasia123", "device_name": "Pixel 8"})
	want(t, res, http.StatusCreated, "")
	d := res.data()
	if d["token_type"] != "Bearer" || num(d["expires_in"]) < 890 || num(d["expires_in"]) > 900 || num(d["refresh_expires_in"]) < 2591000 {
		t.Errorf("token response: %v", d)
	}
	u, _ := d["user"].(map[string]any)
	if u["email"] != "budi@example.com" || !rfc3339WIB.MatchString(u["created_at"].(string)) {
		t.Errorf("user: %v", u)
	}
	for _, leak := range []string{"password", "hash", "role", "session"} {
		if strings.Contains(strings.ToLower(res.raw), leak) {
			t.Errorf("register response contains %q: %s", leak, res.raw)
		}
	}
	if res.header.Get("Cache-Control") != "no-store" {
		t.Errorf("token response Cache-Control = %q", res.header.Get("Cache-Control"))
	}
	if role := queryString(t, db, "SELECT role FROM users WHERE email = 'budi@example.com'"); role != "CUSTOMER" {
		t.Errorf("role = %s", role)
	}
	want(t, c.post("/auth/register", map[string]any{"email": "budi@example.com", "password": "rahasia123"}), http.StatusConflict, "EMAIL_TAKEN")
	res = c.post("/auth/register", map[string]any{"email": "bukan-email", "password": "123"})
	want(t, res, http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	if res.field("email") == "" || res.field("password") == "" {
		t.Errorf("field errors: %s", res.raw)
	}
	want(t, c.post("/auth/register", map[string]any{"email": "x@example.com", "password": "rahasia123", "role": "ADMIN"}), http.StatusBadRequest, "UNKNOWN_FIELD")

	// Login and the protected API.
	want(t, c.post("/auth/login", map[string]any{"email": "budi@example.com", "password": "salah-salah"}), http.StatusUnauthorized, "INVALID_CREDENTIALS")
	res = c.post("/auth/login", map[string]any{"email": "budi@example.com", "password": "rahasia123"})
	want(t, res, http.StatusOK, "")
	c.token, c.refresh = res.data()["access_token"].(string), res.data()["refresh_token"].(string)
	me := c.get("/me")
	want(t, me, http.StatusOK, "")
	if me.data()["email"] != "budi@example.com" || me.header.Get("Cache-Control") != "no-store" {
		t.Errorf("me: %s", me.raw)
	}

	// Missing / invalid / expired tokens.
	anon := newAPI(t, base)
	res = anon.get("/me")
	want(t, res, http.StatusUnauthorized, "UNAUTHORIZED")
	if !strings.HasPrefix(res.header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate = %q", res.header.Get("WWW-Authenticate"))
	}
	anon.token = "ashat_tidak-valid"
	want(t, anon.get("/me"), http.StatusUnauthorized, "INVALID_TOKEN")
	exec(t, db, "UPDATE api_sessions SET access_expires_at = now() - interval '1 minute' WHERE revoked_at IS NULL AND rotated_at IS NULL")
	want(t, c.get("/me"), http.StatusUnauthorized, "TOKEN_EXPIRED")

	// Refresh rotates; the old refresh token is reuse and revokes the device.
	old := c.refresh
	res = c.post("/auth/refresh", map[string]any{"refresh_token": old})
	want(t, res, http.StatusOK, "")
	c.token, c.refresh = res.data()["access_token"].(string), res.data()["refresh_token"].(string)
	want(t, c.get("/me"), http.StatusOK, "")
	want(t, c.post("/auth/refresh", map[string]any{"refresh_token": old}), http.StatusUnauthorized, "REFRESH_TOKEN_REUSED")
	want(t, c.get("/me"), http.StatusUnauthorized, "INVALID_TOKEN")
	want(t, c.post("/auth/refresh", map[string]any{"refresh_token": c.refresh}), http.StatusUnauthorized, "INVALID_TOKEN")
	want(t, c.post("/auth/refresh", map[string]any{}), http.StatusUnprocessableEntity, "VALIDATION_ERROR")

	// Logout one device; logout-all; the web session lives on.
	c.signIn("budi@example.com")
	other := newAPI(t, base)
	other.signIn("budi@example.com")
	web := newBrowser(t, base)
	web.post("/login", map[string][]string{"email": {"budi@example.com"}, "password": {"rahasia123"}})
	if r := c.call(http.MethodPost, "/auth/logout", nil); r.status != http.StatusNoContent {
		t.Fatalf("logout: %d %s", r.status, r.raw)
	}
	want(t, c.get("/me"), http.StatusUnauthorized, "INVALID_TOKEN")
	want(t, other.get("/me"), http.StatusOK, "")
	if r := other.call(http.MethodPost, "/auth/logout-all", nil); r.status != http.StatusNoContent {
		t.Fatalf("logout-all: %d", r.status)
	}
	want(t, other.get("/me"), http.StatusUnauthorized, "INVALID_TOKEN")
	expect(t, web.get("/account"), http.StatusOK, "Akun Saya")
	third := newAPI(t, base)
	third.signIn("budi@example.com")
	if r := newAPI(t, base).post("/auth/logout", map[string]any{"refresh_token": third.refresh}); r.status != http.StatusNoContent {
		t.Fatalf("logout by refresh token: %d", r.status)
	}
	want(t, third.get("/me"), http.StatusUnauthorized, "INVALID_TOKEN")

	// Transports stay apart: the web cookie is no API credential and a
	// Bearer token is no web credential.
	jar := web.client.Jar.Cookies(mustURL(t, base))
	if len(jar) == 0 {
		t.Fatal("no web cookie")
	}
	res = newAPI(t, base).call(http.MethodGet, "/me", nil, "Cookie", jar[0].Name+"="+jar[0].Value)
	want(t, res, http.StatusUnauthorized, "UNAUTHORIZED")
	fourth := newAPI(t, base)
	fourth.signIn("budi@example.com")
	req, _ := http.NewRequest(http.MethodGet, base+"/account", nil)
	req.Header.Set("Authorization", "Bearer "+fourth.token)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r, err := noFollow.Do(req); err != nil || r.StatusCode != http.StatusSeeOther {
		t.Errorf("Bearer token on a web page must not sign in: %v %d", err, r.StatusCode)
	}

	// Request contract: JSON only, strict, bounded.
	want(t, fourth.call(http.MethodPatch, "/me/profile", `{"first_name":"Budi","nama":"x"}`), http.StatusBadRequest, "UNKNOWN_FIELD")
	want(t, fourth.call(http.MethodPatch, "/me/profile", `{"first_name":`), http.StatusBadRequest, "INVALID_REQUEST")
	want(t, fourth.call(http.MethodPatch, "/me/profile", `{"first_name":"A"} {"x":1}`), http.StatusBadRequest, "INVALID_REQUEST")
	want(t, fourth.call(http.MethodPatch, "/me/profile", `{"first_name":7}`), http.StatusBadRequest, "INVALID_REQUEST")
	want(t, fourth.call(http.MethodPatch, "/me/profile", "first_name=Budi", "Content-Type", "application/x-www-form-urlencoded"),
		http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
	big := `{"first_name":"` + strings.Repeat("a", 70<<10) + `"}`
	want(t, fourth.call(http.MethodPatch, "/me/profile", big), http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE")
	res = fourth.call(http.MethodPatch, "/me/profile", map[string]any{"first_name": "Budi", "last_name": "Santoso", "phone": "081234567890", "gender": "LAKI_LAKI"})
	want(t, res, http.StatusOK, "")
	if res.data()["first_name"] != "Budi" || res.data()["phone"] != "+6281234567890" {
		t.Errorf("profile: %s", res.raw)
	}
	res = fourth.call(http.MethodPatch, "/me/profile", map[string]any{"phone": "12"})
	want(t, res, http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	if res.field("phone") == "" {
		t.Errorf("phone field error: %s", res.raw)
	}

	// Errors are JSON everywhere under /api, and say nothing internal.
	want(t, fourth.get("/tidak-ada"), http.StatusNotFound, "NOT_FOUND")
	want(t, fourth.call(http.MethodPut, "/me", nil), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
	r, _ := http.Get(base + "/api/products")
	if r.StatusCode != http.StatusNotFound || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("/api outside v1: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}

	// CORS is closed by default: no allow-origin header, preflight refused.
	res = newAPI(t, base).call(http.MethodGet, "/categories", nil, "Origin", "https://evil.example")
	if res.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS open by default: %q", res.header.Get("Access-Control-Allow-Origin"))
	}
	res = newAPI(t, base).call(http.MethodOptions, "/auth/login", nil, "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
	if res.status != http.StatusForbidden || res.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight from an unknown origin: %d", res.status)
	}
	// A cross-site POST with a Bearer token is not a CSRF risk and is not
	// blocked (no cookie involved); the web keeps its check.
	res = fourth.call(http.MethodPatch, "/me/profile", map[string]any{"first_name": "Budi"}, "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site")
	want(t, res, http.StatusOK, "")
	if r := web.send("/account/profile", map[string][]string{"first_name": {"X"}}, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); r.status != http.StatusForbidden {
		t.Errorf("web CSRF weakened: %d", r.status)
	}
}

func TestAPIRateLimitAndCORSAllowlist(t *testing.T) {
	base, _ := newAPIApp(t, func(c *config.Config) { c.API.CORSOrigins = []string{"http://localhost:5173"} })
	c := newAPI(t, base)
	res := c.call(http.MethodOptions, "/products", nil, "Origin", "http://localhost:5173", "Access-Control-Request-Method", "GET",
		"Access-Control-Request-Headers", "Authorization")
	if res.status != http.StatusNoContent || res.header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		!strings.Contains(res.header.Get("Access-Control-Allow-Headers"), "Authorization") || res.header.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("preflight from an allowed origin: %d %v", res.status, res.header)
	}
	res = c.call(http.MethodGet, "/categories", nil, "Origin", "http://localhost:5173")
	if res.header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" || !strings.Contains(res.header.Get("Vary"), "Origin") {
		t.Errorf("allowed origin: %v", res.header)
	}
	if res := c.call(http.MethodGet, "/categories", nil, "Origin", "http://localhost:5174"); res.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("an unlisted origin got CORS headers")
	}

	// Login: 20 attempts per 5 minutes per IP, then 429 with Retry-After.
	var last apiRes
	for i := 0; i < 21; i++ {
		last = c.post("/auth/login", map[string]any{"email": fmt.Sprintf("u%d@example.com", i), "password": "rahasia123"})
	}
	want(t, last, http.StatusTooManyRequests, "RATE_LIMITED")
	if last.header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	for i := 0; i < 11; i++ {
		last = c.post("/auth/register", map[string]any{"email": fmt.Sprintf("r%d@example.com", i), "password": "rahasia123"})
	}
	want(t, last, http.StatusTooManyRequests, "RATE_LIMITED")
	for i := 0; i < 61; i++ {
		last = c.post("/auth/refresh", map[string]any{"refresh_token": "ashrt_x"})
	}
	want(t, last, http.StatusTooManyRequests, "RATE_LIMITED")
}
