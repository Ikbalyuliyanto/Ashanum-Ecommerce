package review

import "testing"

func TestMaskName(t *testing.T) {
	for _, c := range []struct{ first, last, email, want string }{
		{"Ikbal", "Yuliyanto", "x@example.com", "I**** Y********"},
		{"Siti", "", "x@example.com", "S***"},
		{"", "", "siti.rahma@example.com", "s*********"},
		{"Élodie", "Ñ", "", "É***** Ñ"},
		{"", "", "", "Pembeli"},
	} {
		if got := MaskName(c.first, c.last, c.email); got != c.want {
			t.Errorf("MaskName(%q, %q, %q) = %q, want %q", c.first, c.last, c.email, got, c.want)
		}
	}
}

func TestCleanComment(t *testing.T) {
	if got := cleanComment("  Bagus \r\n\r\n\r\n\r\n  Sesuai  \n"); got != "Bagus\n\nSesuai" {
		t.Errorf("got %q", got)
	}
}
