package media

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://res.cloudinary.com/ashanum/image/upload/v1/p/kaos.jpg": true,
		"data:image/svg+xml;base64,PHN2Zy8+":                            true,
		"data:image/png;base64,iVBORw0KGgo=":                            true,
		"http://res.cloudinary.com/ashanum/image/upload/x.jpg":          false,
		"https://evil.example/x.jpg":                                    false,
		"https://res.cloudinary.com.evil.example/x.jpg":                 false,
		"data:text/html;base64,PHNjcmlwdD4=":                            false,
		"data:image/svg+xml;base64,":                                    false,
		"javascript:alert(1)":                                           false,
		"/static/x.png":                                                 false,
	} {
		if got := Valid(raw); got != want {
			t.Errorf("Valid(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestResize(t *testing.T) {
	in := "https://res.cloudinary.com/ashanum/image/upload/v1/p/kaos.jpg"
	want := "https://res.cloudinary.com/ashanum/image/upload/c_limit,w_480,f_auto,q_auto/v1/p/kaos.jpg"
	if got := Resize(in, 480); got != want {
		t.Errorf("Resize = %q", got)
	}
	if got := Resize("data:image/png;base64,AA==", 480); got != "data:image/png;base64,AA==" {
		t.Errorf("non-Cloudinary images must be unchanged, got %q", got)
	}
}

func TestParseAttachment(t *testing.T) {
	ok := map[string]string{
		"https://drive.google.com/file/d/abc/view":                    KindLink,
		"https://i.imgur.com/foto-rusak.JPG":                          KindImage,
		"https://res.cloudinary.com/ashanum/image/upload/v1/retur/a1": KindImage,
		"https://res.cloudinary.com/ashanum/video/upload/v1/retur/v1": KindVideo,
		"https://contoh.example/video-unboxing.mp4?dl=1":              KindVideo,
		"  https://contoh.example/bukti  ":                            KindLink,
	}
	for raw, kind := range ok {
		a, valid := ParseAttachment(raw)
		if !valid || a.Kind != kind {
			t.Errorf("%q → %+v %v, want kind %s", raw, a, valid, kind)
		}
	}
	for _, raw := range []string{
		"", "http://contoh.example/a.jpg", "file:///C:/Users/a.jpg", "C:\foto\a.jpg", "/var/www/a.jpg",
		"https://user:pass@contoh.example/a.jpg", "https://localhost/a.jpg", "https://127.0.0.1/a.jpg",
		"https://[::1]/a.jpg", "https://printer.local/a.jpg", "https://intranet/a.jpg", "javascript:alert(1)",
		"https://contoh.example/a b.jpg", "https://contoh.example/\"onerror=x", "https://contoh.example/" + strings.Repeat("a", 2100),
	} {
		if _, valid := ParseAttachment(raw); valid {
			t.Errorf("%q must be rejected", raw)
		}
	}
	if a, _ := ParseAttachment("https://res.cloudinary.com/ashanum/image/upload/v1/a"); !a.ShowInline() {
		t.Error("Cloudinary images may be shown inline")
	}
	if a, _ := ParseAttachment("https://i.imgur.com/a.jpg"); a.ShowInline() {
		t.Error("other hosts are links only (CSP img-src)")
	}
}
