// Package view renders server-side HTML with html/template.
//
// Layout:
//
//	templates/layouts/*.html   shared layouts, e.g. {{define "base"}}
//	templates/partials/*.html  shared fragments, e.g. {{define "header"}}
//	templates/<module>/*.html  pages, addressed as "<module>/<file>" without .html
//
// A page picks its layout with {{template "base" .}} and fills the blocks it
// needs ({{define "title"}}, {{define "content"}}). Each page is parsed into
// its own set so block names never collide between pages.
package view

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// App is the global information every page can read as .App.
type App struct {
	Name string
	Dev  bool
}

// Page is the root value passed to every template.
type Page struct {
	App   App
	Path  string
	Query url.Values // request query, e.g. to keep the header search text
	User  any        // signed-in user or nil; see Config.CurrentUser
	Flash *Flash     // one-time message from the previous request
	// CartCount is the header cart badge (total quantity), full pages only.
	CartCount int
	Data      any
}

type Config struct {
	FS     fs.FS
	Assets *Assets
	App    App
	Log    *slog.Logger
	// CurrentUser returns the signed-in user for the header, or nil. It is a
	// hook so this package stays independent of the auth module.
	CurrentUser func(*http.Request) any
}

type Renderer struct {
	cfg       Config
	pages     map[string]*template.Template
	cartCount func(*http.Request) int
}

// SetCartCounter sets how full pages get the header cart badge for a
// signed-in user. Call it while wiring routes, before serving.
func (r *Renderer) SetCartCounter(fn func(*http.Request) int) { r.cartCount = fn }

// cartQuantifier is implemented by page data that already loaded the cart,
// so the badge needs no extra query.
type cartQuantifier interface{ CartQuantity() int }

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// New parses all templates. In development (App.Dev) templates are parsed
// again on each render so edits show up without a restart.
func New(cfg Config) (*Renderer, error) {
	if cfg.CurrentUser == nil {
		cfg.CurrentUser = func(*http.Request) any { return nil }
	}
	r := &Renderer{cfg: cfg}
	pages, err := r.parse()
	if err != nil {
		return nil, err
	}
	r.pages = pages
	return r, nil
}

func (r *Renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":       r.cfg.Assets.URL,
		"icon":        r.icon,
		"currentYear": func() int { return time.Now().Year() },
		"dict":        dict,
		"phone":       FormatPhone,
		"phoneLocal":  LocalPhone,
		"initial":     initial,
		"hasPrefix":   strings.HasPrefix,
		"rupiah":      FormatRupiah,
		"sold":        FormatSold,
		"img":         r.imageURL,
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
	}
}

func (r *Renderer) parse() (map[string]*template.Template, error) {
	shared, err := template.New("").Funcs(r.funcs()).ParseFS(r.cfg.FS, "layouts/*.html", "partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("view: parse layouts/partials: %w", err)
	}

	pages := make(map[string]*template.Template)
	err = fs.WalkDir(r.cfg.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".html" ||
			strings.HasPrefix(p, "layouts/") || strings.HasPrefix(p, "partials/") {
			return nil
		}
		src, err := fs.ReadFile(r.cfg.FS, p)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(p, ".html")
		t, err := shared.Clone()
		if err != nil {
			return err
		}
		if _, err := t.New("page:" + name).Parse(string(src)); err != nil {
			return fmt.Errorf("view: parse %s: %w", p, err)
		}
		pages[name] = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pages, nil
}

func (r *Renderer) lookup(name string) (*template.Template, error) {
	pages := r.pages
	if r.cfg.App.Dev {
		var err error
		if pages, err = r.parse(); err != nil {
			return nil, err
		}
	}
	t, ok := pages[name]
	if !ok {
		return nil, fmt.Errorf("view: page %q not found", name)
	}
	return t, nil
}

// Render writes a full page, e.g. Render(w, req, 200, "home/index", data).
// It also consumes a pending flash message.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, status int, page string, data any) {
	r.execute(w, req, status, page, "page:"+page, data, r.takeFlash(w, req), true)
}

// Fragment writes one {{define}} block from a page's template set, for HTMX
// responses that swap part of the page.
func (r *Renderer) Fragment(w http.ResponseWriter, req *http.Request, status int, page, block string, data any) {
	r.execute(w, req, status, page, block, data, nil, false)
}

// Error renders the shared error page for the given status code.
func (r *Renderer) Error(w http.ResponseWriter, req *http.Request, status int) {
	r.Render(w, req, status, "errors/error", ErrorData(status))
}

// ServerError logs err and renders the 500 page.
func (r *Renderer) ServerError(w http.ResponseWriter, req *http.Request, err error) {
	r.cfg.Log.Error("request failed", "method", req.Method, "path", req.URL.Path, "error", err)
	r.Error(w, req, http.StatusInternalServerError)
}

// execute renders into a pooled buffer first, so a template error never sends
// a half-written page with a 200 status.
func (r *Renderer) execute(w http.ResponseWriter, req *http.Request, status int, page, name string, data any, flash *Flash, full bool) {
	t, err := r.lookup(page)
	if err != nil {
		r.fail(w, req, err)
		return
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	root := Page{
		App:   r.cfg.App,
		Path:  req.URL.Path,
		Query: req.URL.Query(),
		User:  r.cfg.CurrentUser(req),
		Flash: flash,
		Data:  data,
	}
	if full && root.User != nil {
		if cq, ok := data.(cartQuantifier); ok {
			root.CartCount = cq.CartQuantity()
		} else if r.cartCount != nil {
			root.CartCount = r.cartCount(req)
		}
	}
	if err := t.ExecuteTemplate(buf, name, root); err != nil {
		r.fail(w, req, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (r *Renderer) fail(w http.ResponseWriter, req *http.Request, err error) {
	r.cfg.Log.Error("render failed", "path", req.URL.Path, "error", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func (r *Renderer) icon(name string) template.HTML {
	href := template.HTMLEscapeString(r.cfg.Assets.URL("icons/sprite.svg") + "#" + name)
	return template.HTML(`<svg class="icon" aria-hidden="true" focusable="false"><use href="` + href + `"></use></svg>`)
}

// IsHTMX reports whether the request was sent by HTMX (HX-Request header).
func IsHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// ParseForm parses a urlencoded form body limited to 64 KiB.
func ParseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	return r.ParseForm()
}

type ErrorInfo struct {
	Status  int
	Title   string
	Message string
}

// ErrorData returns user-facing text (Indonesian) for an HTTP status.
func ErrorData(status int) ErrorInfo {
	d := ErrorInfo{Status: status, Title: "Terjadi kesalahan", Message: "Silakan coba beberapa saat lagi."}
	switch status {
	case http.StatusNotFound:
		d.Title, d.Message = "Halaman tidak ditemukan", "Halaman yang Anda cari tidak ada atau sudah dipindahkan."
	case http.StatusMethodNotAllowed:
		d.Title, d.Message = "Metode tidak diizinkan", "Permintaan ini tidak didukung untuk halaman tersebut."
	case http.StatusForbidden:
		d.Title, d.Message = "Akses ditolak", "Anda tidak memiliki izin untuk melakukan aksi ini."
	case http.StatusTooManyRequests:
		d.Title, d.Message = "Terlalu banyak percobaan", "Silakan tunggu beberapa menit lalu coba lagi."
	}
	return d
}

func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// FormatPhone displays a stored +628… number as "+62 812-3456-7890".
func FormatPhone(e164 string) string {
	n, ok := strings.CutPrefix(e164, "+62")
	if !ok || len(n) < 8 {
		return e164
	}
	return "+62 " + n[:3] + "-" + n[3:7] + "-" + n[7:]
}

// LocalPhone turns a stored +628… number back into 08… for form inputs.
func LocalPhone(e164 string) string {
	if n, ok := strings.CutPrefix(e164, "+62"); ok {
		return "0" + n
	}
	return e164
}

// initial returns the upper-case first letter of s, for avatars.
func initial(s string) string {
	for _, r := range s {
		return strings.ToUpper(string(r))
	}
	return "?"
}
