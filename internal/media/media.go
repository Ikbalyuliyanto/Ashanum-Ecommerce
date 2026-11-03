// Package media decides which image references the storefront accepts and
// how they are displayed.
//
// Images are never stored or processed on the application server: the
// database keeps a URL to external storage (Cloudinary in production), and
// resizing is delegated to Cloudinary's on-the-fly transformations. Uploading
// arrives with the admin phase; switching storage provider only touches this
// package and the Content-Security-Policy img-src list.
package media

import (
	"net/url"
	"strconv"
	"strings"
)

// cloudinaryHost must stay in sync with img-src in middleware.SecureHeaders.
const cloudinaryHost = "res.cloudinary.com"

// maxDataURI bounds inline images, which exist for demo/development data.
const maxDataURI = 64 << 10

var dataPrefixes = []string{
	"data:image/png;base64,",
	"data:image/jpeg;base64,",
	"data:image/webp;base64,",
	"data:image/svg+xml;base64,",
}

// Valid reports whether raw can be stored and shown as an image: an https
// Cloudinary URL, or a small base64 data:image URI.
func Valid(raw string) bool {
	if strings.HasPrefix(raw, "data:") {
		if len(raw) > maxDataURI {
			return false
		}
		for _, p := range dataPrefixes {
			if strings.HasPrefix(raw, p) && len(raw) > len(p) {
				return true
			}
		}
		return false
	}
	if len(raw) > 1000 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Hostname() == cloudinaryHost
}

// Resize returns a URL showing raw at most width pixels wide. Cloudinary
// images get a resize + automatic format/quality transformation; anything
// else is returned unchanged.
func Resize(raw string, width int) string {
	const marker = "/image/upload/"
	if width <= 0 || !strings.HasPrefix(raw, "https://"+cloudinaryHost+"/") {
		return raw
	}
	i := strings.Index(raw, marker)
	if i < 0 {
		return raw
	}
	i += len(marker)
	return raw[:i] + "c_limit,w_" + strconv.Itoa(width) + ",f_auto,q_auto/" + raw[i:]
}
