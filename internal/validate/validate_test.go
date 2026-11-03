package validate

import (
	"errors"
	"testing"
)

func TestEmail(t *testing.T) {
	for in, want := range map[string]string{
		"  Budi@Example.COM ":   "budi@example.com",
		"a.b+tag@sub.domain.id": "a.b+tag@sub.domain.id",
	} {
		got, ok := Email(in)
		if !ok || got != want {
			t.Errorf("Email(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "budi", "budi@", "@x.com", "budi@localhost", "Budi <budi@x.com>", "budi@x.com.", "a b@x.com"} {
		if _, ok := Email(in); ok {
			t.Errorf("Email(%q) should be invalid", in)
		}
	}
}

func TestPhone(t *testing.T) {
	for in, want := range map[string]string{
		"081234567890":      "+6281234567890",
		"+62 812-3456-7890": "+6281234567890",
		"62812345678":       "+62812345678",
		"(0812) 3456.789":   "+628123456789",
	} {
		got, ok := Phone(in)
		if !ok || got != want {
			t.Errorf("Phone(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0212345678", "0812", "08123456789012345", "0812abc4567", "+1 555 123 4567", "08-12+34"} {
		if _, ok := Phone(in); ok {
			t.Errorf("Phone(%q) should be invalid", in)
		}
	}
}

func TestFieldRules(t *testing.T) {
	e := Errors{}
	Name(e, "first", "Siti Nur'aini", "Nama", true)
	Name(e, "last", "", "Nama belakang", false)
	Password(e, "pw", "rahasia123")
	Length(e, "street", "Jl. Melati No. 5 RT 01/RW 02", "Alamat", 10, 200)
	if err := e.Err(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}

	Name(e, "bad", "R2D2", "Nama", true)
	Password(e, "short", "abc")
	Password(e, "long", string(make([]byte, 73)))
	Length(e, "req", "", "Kota", 2, 60)
	Length(e, "ctrl", "abc\x00def", "Kota", 2, 60)
	for _, f := range []string{"bad", "short", "long", "req", "ctrl"} {
		if _, ok := e[f]; !ok {
			t.Errorf("expected error for %s", f)
		}
	}

	var ve Errors
	if !errors.As(e.Err(), &ve) {
		t.Error("Errors must be retrievable with errors.As")
	}
}

func TestText(t *testing.T) {
	if got := Text("  Jl.   Mawar \t 5 "); got != "Jl. Mawar 5" {
		t.Errorf("Text = %q", got)
	}
}
