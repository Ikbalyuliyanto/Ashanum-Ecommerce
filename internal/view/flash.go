package view

import (
	"encoding/base64"
	"net/http"
	"strings"
)

const flashCookie = "flash"

// Flash is a one-time message shown on the next rendered page, typically
// after a POST → redirect. It travels in a short-lived cookie holding only
// the message text, never user data.
type Flash struct {
	Kind    string // "success" | "error" | "info"
	Message string
}

// SetFlash queues a message for the next full page render.
func (r *Renderer) SetFlash(w http.ResponseWriter, kind, message string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + message)),
		Path:     "/",
		MaxAge:   60,
		HttpOnly: true,
		Secure:   !r.cfg.App.Dev,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeFlash reads and clears a pending flash message.
func (r *Renderer) takeFlash(w http.ResponseWriter, req *http.Request) *Flash {
	c, err := req.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   !r.cfg.App.Dev,
		SameSite: http.SameSiteLaxMode,
	})

	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(string(raw), ":")
	if !ok || msg == "" || (kind != "success" && kind != "error" && kind != "info") {
		return nil
	}
	return &Flash{Kind: kind, Message: msg}
}
