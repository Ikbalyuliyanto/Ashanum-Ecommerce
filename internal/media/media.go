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
	"net"
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

// Attachment kinds, for evidence links (returns) and future uploads.
const (
	KindImage = "IMAGE"
	KindVideo = "VIDEO"
	KindLink  = "LINK"
)

// maxAttachmentURL bounds an attachment URL.
const maxAttachmentURL = 2048

// Attachment is a file kept in external storage, referenced by URL only:
// nothing is downloaded, stored or processed on the application server.
type Attachment struct {
	URL  string
	Kind string // KindImage, KindVideo or KindLink
}

// ShowInline reports whether the attachment may be shown as <img> on our
// pages. Only Cloudinary images are allowed by the Content-Security-Policy;
// any other URL is shown as a link the user opens.
func (a Attachment) ShowInline() bool {
	return a.Kind == KindImage && strings.HasPrefix(a.URL, "https://"+cloudinaryHost+"/")
}

// ParseAttachment accepts an absolute https URL to a public host (no
// credentials, no local or private address, no file paths) and guesses
// its kind from the path. A future uploader (e.g. Cloudinary) returns URLs
// that pass this check, so callers do not change when uploads arrive.
func ParseAttachment(raw string) (Attachment, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxAttachmentURL || strings.ContainsAny(raw, " \t\r\n\"'<>\\") {
		return Attachment{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return Attachment{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		!strings.Contains(host, ".") || isIPAddress(host) {
		return Attachment{}, false
	}
	a := Attachment{URL: u.String(), Kind: KindLink}
	path := strings.ToLower(u.Path)
	switch {
	case host == cloudinaryHost && strings.Contains(path, "/image/upload/"), hasExt(path, ".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic"):
		a.Kind = KindImage
	case host == cloudinaryHost && strings.Contains(path, "/video/upload/"), hasExt(path, ".mp4", ".mov", ".webm"):
		a.Kind = KindVideo
	}
	return a, true
}

func hasExt(path string, exts ...string) bool {
	for _, e := range exts {
		if strings.HasSuffix(path, e) {
			return true
		}
	}
	return false
}

// isIPAddress reports a literal IPv4/IPv6 host: attachments must name a
// public host, not an address (which could point inside a network).
func isIPAddress(host string) bool {
	return net.ParseIP(strings.Trim(host, "[]")) != nil
}
