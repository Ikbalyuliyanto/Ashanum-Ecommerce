package media

import "testing"

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
