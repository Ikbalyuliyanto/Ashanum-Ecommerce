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
	App  App
	Path string
	Data any
}

type Renderer struct {
	fsys   fs.FS
	app    App
	assets *Assets
	log    *slog.Logger
	pages  map[string]*template.Template
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// New parses all templates. In development (app.Dev) templates are parsed
// again on each render so edits show up without a restart.
func New(fsys fs.FS, assets *Assets, app App, log *slog.Logger) (*Renderer, error) {
	r := &Renderer{fsys: fsys, app: app, assets: assets, log: log}
	pages, err := r.parse()
	if err != nil {
		return nil, err
	}
	r.pages = pages
	return r, nil
}

func (r *Renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":       r.assets.URL,
		"currentYear": func() int { return time.Now().Year() },
	}
}

func (r *Renderer) parse() (map[string]*template.Template, error) {
	shared, err := template.New("").Funcs(r.funcs()).ParseFS(r.fsys, "layouts/*.html", "partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("view: parse layouts/partials: %w", err)
	}

	pages := make(map[string]*template.Template)
	err = fs.WalkDir(r.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".html" ||
			strings.HasPrefix(p, "layouts/") || strings.HasPrefix(p, "partials/") {
			return nil
		}
		src, err := fs.ReadFile(r.fsys, p)
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
	if r.app.Dev {
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
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, status int, page string, data any) {
	r.execute(w, req, status, page, "page:"+page, data)
}

// Fragment writes one {{define}} block from a page's template set, for HTMX
// responses that swap part of the page.
func (r *Renderer) Fragment(w http.ResponseWriter, req *http.Request, status int, page, block string, data any) {
	r.execute(w, req, status, page, block, data)
}

// Error renders the shared error page for the given status code.
func (r *Renderer) Error(w http.ResponseWriter, req *http.Request, status int) {
	r.Render(w, req, status, "errors/error", ErrorData(status))
}

// execute renders into a pooled buffer first, so a template error never sends
// a half-written page with a 200 status.
func (r *Renderer) execute(w http.ResponseWriter, req *http.Request, status int, page, name string, data any) {
	t, err := r.lookup(page)
	if err != nil {
		r.fail(w, req, err)
		return
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	root := Page{App: r.app, Path: req.URL.Path, Data: data}
	if err := t.ExecuteTemplate(buf, name, root); err != nil {
		r.fail(w, req, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (r *Renderer) fail(w http.ResponseWriter, req *http.Request, err error) {
	r.log.Error("render failed", "path", req.URL.Path, "error", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// IsHTMX reports whether the request was sent by HTMX (HX-Request header).
func IsHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

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
		d.Title, d.Message = "Terlalu banyak permintaan", "Silakan tunggu sebentar lalu coba lagi."
	}
	return d
}
