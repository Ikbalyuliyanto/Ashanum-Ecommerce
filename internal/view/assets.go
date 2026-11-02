package view

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// contentTypes avoids depending on the OS MIME table (the Windows registry
// can map .js to text/plain, which nosniff then blocks).
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

// Assets serves files under /static/ and builds cache-busted URLs for them.
type Assets struct {
	fsys     fs.FS
	prefix   string
	dev      bool
	versions map[string]string
}

// NewAssets hashes every file once at startup. The hash is used as the ?v=
// query in URLs and as the ETag, so browsers and Cloudflare can cache assets
// for a year and still pick up a new deploy.
func NewAssets(fsys fs.FS, prefix string, dev bool) (*Assets, error) {
	a := &Assets{fsys: fsys, prefix: prefix, dev: dev, versions: make(map[string]string)}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		a.versions[p] = hex.EncodeToString(h.Sum(nil))[:12]
		return nil
	})
	return a, err
}

// URL returns the public URL of a static file, e.g. "css/app.css" →
// "/static/css/app.css?v=3f2a9c1b04de".
func (a *Assets) URL(name string) string {
	if v, ok := a.versions[name]; ok && !a.dev {
		return a.prefix + name + "?v=" + v
	}
	return a.prefix + name
}

// ServeHTTP serves a single file; directory listings are never exposed.
func (a *Assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, a.prefix)
	if !fs.ValidPath(name) || name == "." || strings.HasSuffix(name, "/") {
		http.NotFound(w, r)
		return
	}
	f, err := a.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "unseekable asset", http.StatusInternalServerError)
		return
	}

	ct := contentTypes[path.Ext(name)]
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(name))
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	switch {
	case a.dev:
		w.Header().Set("Cache-Control", "no-cache")
	case r.URL.Query().Get("v") != "":
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"`+a.versions[name]+`"`)
	default:
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("ETag", `"`+a.versions[name]+`"`)
	}
	http.ServeContent(w, r, name, info.ModTime(), rs)
}
