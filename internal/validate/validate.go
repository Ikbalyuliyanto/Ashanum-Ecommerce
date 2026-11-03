// Package validate holds server-side input rules shared by services.
//
// Services return Errors (field → user-facing message, in Indonesian), which
// web handlers render next to form fields and a future JSON API can return
// as-is. Nothing here depends on HTTP.
package validate

import (
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Errors maps a field name to its message. It implements error so services
// can return it like any other error; use errors.As to inspect it.
type Errors map[string]string

func (e Errors) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("validation failed:")
	for _, k := range keys {
		b.WriteString(" " + k + ": " + e[k] + ";")
	}
	return b.String()
}

// Add records the first message for a field.
func (e Errors) Add(field, msg string) {
	if _, exists := e[field]; !exists {
		e[field] = msg
	}
}

// Err returns e as an error, or nil when there are no field errors.
func (e Errors) Err() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// Text trims s and collapses internal runs of whitespace into one space.
func Text(s string) string { return strings.Join(strings.Fields(s), " ") }

// Length checks a required or optional free-text value.
func Length(e Errors, field, value, label string, min, max int) {
	n := utf8.RuneCountInString(value)
	switch {
	case n == 0 && min > 0:
		e.Add(field, label+" wajib diisi.")
	case n < min:
		e.Add(field, label+" terlalu pendek.")
	case n > max:
		e.Add(field, label+" maksimal "+strconv.Itoa(max)+" karakter.")
	case !printable(value):
		e.Add(field, label+" mengandung karakter yang tidak diizinkan.")
	}
}

// Name checks a person's name: letters, spaces and . ' - only.
func Name(e Errors, field, value, label string, required bool) {
	min := 0
	if required {
		min = 1
	}
	Length(e, field, value, label, min, 50)
	for _, r := range value {
		if !unicode.IsLetter(r) && r != ' ' && r != '.' && r != '\'' && r != '-' {
			e.Add(field, label+" hanya boleh berisi huruf, spasi, titik, apostrof, atau tanda hubung.")
			return
		}
	}
}

// Email normalizes an address (trimmed, lower-case) and reports whether it
// is a plain, valid address such as nama@domain.com.
func Email(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return s, false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return s, false
	}
	at := strings.LastIndexByte(s, '@')
	domain := s[at+1:]
	if at < 1 || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return s, false
	}
	return s, true
}

// Phone normalizes an Indonesian mobile number to E.164 (+628…). It accepts
// 0812…, 62812…, +62 812-…, with spaces, dots or dashes.
func Phone(s string) (string, bool) {
	var digits strings.Builder
	for i, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0, r == ' ', r == '-', r == '.', r == '(', r == ')':
		default:
			return s, false
		}
	}
	d := digits.String()
	switch {
	case strings.HasPrefix(d, "62"):
		d = d[2:]
	case strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	// National number: 8xx followed by 7–11 more digits (10–13 digits with 0).
	if len(d) < 9 || len(d) > 12 || d[0] != '8' {
		return s, false
	}
	return "+62" + d, true
}

// Password checks length only (no composition rules). bcrypt ignores input
// beyond 72 bytes, so longer passwords are rejected rather than truncated.
func Password(e Errors, field, value string) {
	switch {
	case value == "":
		e.Add(field, "Password wajib diisi.")
	case utf8.RuneCountInString(value) < 8:
		e.Add(field, "Password minimal 8 karakter.")
	case len(value) > 72:
		e.Add(field, "Password terlalu panjang (maksimal 72 byte).")
	}
}

func printable(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n') {
			return false
		}
	}
	return true
}

// Slug turns text into a URL slug: lower-case ASCII letters and digits
// separated by single hyphens ("Kaos Ashanum – Hitam" → "kaos-ashanum-hitam").
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// IsSlug reports whether s is already a valid slug of at most 120 bytes.
func IsSlug(s string) bool { return s != "" && len(s) <= 120 && Slug(s) == s }
