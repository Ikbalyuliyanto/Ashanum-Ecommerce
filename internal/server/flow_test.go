package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ecommerce/internal/config"
	"ecommerce/internal/testutil"
)

// browser is a cookie-keeping client that does not follow redirects, so
// tests can assert on each 303.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
}

type response struct {
	status   int
	location string
	header   http.Header
	body     string
}

func newApp(t *testing.T) (string, *pgxpool.Pool) {
	db := testutil.DB(t)
	srv := httptest.NewServer(buildRouter(t, db, config.EnvDevelopment))
	t.Cleanup(srv.Close)
	return srv.URL, db
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) do(method, path string, form url.Values, htmx bool) response {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, location: res.Header.Get("Location"), header: res.Header, body: string(raw)}
}

func (b *browser) get(path string) response { return b.do(http.MethodGet, path, nil, false) }
func (b *browser) post(path string, form url.Values) response {
	return b.do(http.MethodPost, path, form, false)
}

func (b *browser) register(email string) {
	b.t.Helper()
	res := b.post("/register", url.Values{"email": {email}, "password": {"rahasia123"}, "password_confirm": {"rahasia123"}})
	if res.status != http.StatusSeeOther {
		b.t.Fatalf("register %s: status %d\n%s", email, res.status, res.body)
	}
}

func expect(t *testing.T, res response, status int, contains ...string) {
	t.Helper()
	if res.status != status {
		t.Fatalf("status = %d, want %d (location %q)\n%s", res.status, status, res.location, res.body)
	}
	for _, s := range contains {
		if !strings.Contains(res.body, s) {
			t.Errorf("body missing %q", s)
		}
	}
}

func TestAuthFlow(t *testing.T) {
	base, _ := newApp(t)
	b := newBrowser(t, base)

	// Protected route redirects guests to login, remembering the target.
	res := b.get("/account")
	if res.status != http.StatusSeeOther || res.location != "/login?next=%2Faccount" {
		t.Fatalf("guest /account: %d %q", res.status, res.location)
	}
	expect(t, b.get("/login?next=/account"), http.StatusOK, "Masuk", `name="next" value="/account"`)

	// Register signs the user in.
	res = b.post("/register", url.Values{"email": {"budi@example.com"}, "password": {"rahasia123"}, "password_confirm": {"rahasia123"}})
	if res.status != http.StatusSeeOther || res.location != "/account" {
		t.Fatalf("register: %d %q", res.status, res.location)
	}
	if c := res.header.Get("Set-Cookie"); !strings.Contains(c, "sid=") || !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Lax") {
		t.Errorf("session cookie attributes: %q", c)
	}
	res = b.get("/account")
	expect(t, res, http.StatusOK, "budi@example.com", "Akun Saya", "Alamat Saya", "Belum Bayar", "Selamat datang di Ashanum")
	if cc := res.header.Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("account Cache-Control = %q", cc)
	}

	// Signed-in users are sent away from login/register.
	if res := b.get("/login"); res.status != http.StatusSeeOther || res.location != "/account" {
		t.Errorf("signed-in /login: %d %q", res.status, res.location)
	}

	// Logout ends the session.
	res = b.post("/logout", nil)
	if res.status != http.StatusSeeOther || res.location != "/" {
		t.Fatalf("logout: %d %q", res.status, res.location)
	}
	if res := b.get("/account"); res.status != http.StatusSeeOther {
		t.Fatalf("after logout /account: %d", res.status)
	}

	// Duplicate email.
	res = newBrowser(t, base).post("/register", url.Values{"email": {"BUDI@example.com"}, "password": {"lainnya123"}, "password_confirm": {"lainnya123"}})
	expect(t, res, http.StatusUnprocessableEntity, "Email sudah terdaftar")

	// Wrong password, then correct login with a safe next path.
	expect(t, b.post("/login", url.Values{"email": {"budi@example.com"}, "password": {"salah12345"}}),
		http.StatusUnprocessableEntity, "Email atau password salah", `value="budi@example.com"`)
	res = b.post("/login", url.Values{"email": {"budi@example.com"}, "password": {"rahasia123"}, "next": {"/account/addresses"}})
	if res.status != http.StatusSeeOther || res.location != "/account/addresses" {
		t.Fatalf("login: %d %q", res.status, res.location)
	}
	expect(t, b.get("/account/addresses"), http.StatusOK, "Belum ada alamat")

	// An open redirect in next is neutralized.
	b2 := newBrowser(t, base)
	res = b2.post("/login", url.Values{"email": {"budi@example.com"}, "password": {"rahasia123"}, "next": {"//evil.example/"}})
	if res.location != "/" {
		t.Errorf("open redirect: location %q", res.location)
	}
}

func TestProtectedRoutes(t *testing.T) {
	base, _ := newApp(t)
	b := newBrowser(t, base)

	for _, path := range []string{"/account", "/account/profile", "/account/addresses", "/account/addresses/new", "/account/addresses/1/edit"} {
		if res := b.get(path); res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "/login?next=") {
			t.Errorf("GET %s: %d %q", path, res.status, res.location)
		}
	}
	for _, path := range []string{"/account/profile", "/account/addresses", "/account/addresses/1", "/account/addresses/1/primary", "/account/addresses/1/delete"} {
		if res := b.post(path, url.Values{}); res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "/login") {
			t.Errorf("POST %s: %d %q", path, res.status, res.location)
		}
	}

	// HTMX requests get HX-Redirect instead of a 303 into the swap target.
	res := b.do(http.MethodPost, "/account/addresses/1/delete", url.Values{}, true)
	if res.status != http.StatusUnauthorized || !strings.HasPrefix(res.header.Get("HX-Redirect"), "/login?next=") {
		t.Errorf("HTMX: %d HX-Redirect=%q", res.status, res.header.Get("HX-Redirect"))
	}

	// A forged session cookie is rejected and cleared.
	u, _ := url.Parse(base)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: "sid", Value: "forged-token-value"}})
	if res := b.get("/account"); res.status != http.StatusSeeOther {
		t.Errorf("forged cookie: %d", res.status)
	}
}

func TestProfileUpdate(t *testing.T) {
	base, _ := newApp(t)
	b := newBrowser(t, base)
	b.register("rina@example.com")

	expect(t, b.get("/account/profile"), http.StatusOK, "Profil Saya", `value="rina@example.com"`)

	// HTMX save returns only the form fragment with a success note.
	form := url.Values{"first_name": {"Rina"}, "last_name": {"Putri"}, "phone": {"0812 3456 7890"}, "gender": {"PEREMPUAN"}}
	res := b.do(http.MethodPost, "/account/profile", form, true)
	expect(t, res, http.StatusOK, "Profil berhasil disimpan", `id="profile-form"`, `value="081234567890"`)
	if strings.Contains(res.body, "<html") {
		t.Error("HTMX response must be a fragment, not a full page")
	}

	// Validation errors come back as 422 with field messages.
	res = b.do(http.MethodPost, "/account/profile", url.Values{"first_name": {""}, "phone": {"12"}}, true)
	expect(t, res, http.StatusUnprocessableEntity, "Nama depan wajib diisi", "nomor HP Indonesia yang valid")

	// The saved profile shows on the account hub.
	expect(t, b.get("/account"), http.StatusOK, "Rina Putri")

	// Plain form post (no JavaScript) redirects with a flash message.
	res = b.post("/account/profile", form)
	if res.status != http.StatusSeeOther || res.location != "/account/profile" {
		t.Fatalf("plain post: %d %q", res.status, res.location)
	}
	expect(t, b.get("/account/profile"), http.StatusOK, "Profil berhasil disimpan.")
}

func addressForm(label string) url.Values {
	return url.Values{
		"label": {label}, "recipient_name": {"Budi Santoso"}, "phone": {"081234567890"},
		"province": {"Jawa Barat"}, "city": {"Kota Bandung"}, "district": {"Coblong"}, "village": {"Dago"},
		"postal_code": {"40135"}, "street_address": {"Jl. Ir. H. Juanda No. 100"},
	}
}

func latestAddressID(t *testing.T, db *pgxpool.Pool, email string) string {
	t.Helper()
	var id int64
	err := db.QueryRow(context.Background(),
		`SELECT a.id FROM user_addresses a JOIN users u ON u.id = a.user_id WHERE u.email = $1 ORDER BY a.id DESC LIMIT 1`,
		email).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(id, 10)
}

func TestAddressFlowAndIsolation(t *testing.T) {
	base, db := newApp(t)
	alice := newBrowser(t, base)
	alice.register("alice@example.com")

	// Create.
	res := alice.post("/account/addresses", addressForm("Rumah"))
	if res.status != http.StatusSeeOther || res.location != "/account/addresses" {
		t.Fatalf("create: %d %q\n%s", res.status, res.location, res.body)
	}
	home := latestAddressID(t, db, "alice@example.com")
	expect(t, alice.get("/account/addresses"), http.StatusOK, "Alamat berhasil disimpan", "Rumah", "Utama", "&#43;62 812-3456-7890")

	// Invalid create re-renders the form with errors.
	bad := addressForm("Kantor")
	bad.Set("postal_code", "40")
	expect(t, alice.post("/account/addresses", bad), http.StatusUnprocessableEntity, "Kode pos harus 5 digit")

	alice.post("/account/addresses", addressForm("Kantor"))
	office := latestAddressID(t, db, "alice@example.com")

	// Update.
	upd := addressForm("Kantor Pusat")
	expect(t, alice.get("/account/addresses/"+office+"/edit"), http.StatusOK, `value="Kantor"`, "Jadikan sebagai alamat utama")
	if res := alice.post("/account/addresses/"+office, upd); res.status != http.StatusSeeOther {
		t.Fatalf("update: %d\n%s", res.status, res.body)
	}

	// Set primary via HTMX returns the refreshed list fragment.
	res = alice.do(http.MethodPost, "/account/addresses/"+office+"/primary", url.Values{}, true)
	expect(t, res, http.StatusOK, `id="address-list"`, "Alamat utama berhasil diubah", "Kantor Pusat")
	if strings.Contains(res.body, "<html") {
		t.Error("HTMX response must be a fragment")
	}

	// Bob cannot see or touch Alice's addresses.
	bob := newBrowser(t, base)
	bob.register("bob@example.com")
	expect(t, bob.get("/account/addresses/"+home+"/edit"), http.StatusNotFound)
	expect(t, bob.post("/account/addresses/"+home, addressForm("Dibajak")), http.StatusNotFound)
	expect(t, bob.post("/account/addresses/"+home+"/primary", url.Values{}), http.StatusNotFound)
	expect(t, bob.post("/account/addresses/"+home+"/delete", url.Values{}), http.StatusNotFound)
	res = bob.get("/account/addresses")
	expect(t, res, http.StatusOK, "Belum ada alamat")
	if strings.Contains(res.body, "Kantor Pusat") {
		t.Error("bob's list leaks alice's address")
	}
	expect(t, alice.get("/account/addresses/"+home+"/edit"), http.StatusOK, `value="Rumah"`)

	// Delete (plain form post) and the primary moves on.
	res = alice.post("/account/addresses/"+office+"/delete", url.Values{})
	if res.status != http.StatusSeeOther {
		t.Fatalf("delete: %d", res.status)
	}
	res = alice.get("/account/addresses")
	expect(t, res, http.StatusOK, "Alamat berhasil dihapus", "Rumah", "Utama")
	if strings.Contains(res.body, "Kantor Pusat") {
		t.Error("deleted address still listed")
	}
	expect(t, alice.get("/account/addresses/"+office+"/edit"), http.StatusNotFound)
}
