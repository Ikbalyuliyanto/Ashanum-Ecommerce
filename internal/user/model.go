// Package user owns the customer profile, kept apart from login
// credentials (package auth). The Service has no HTTP dependency so the web
// pages here and a future /api/v1/users handler share it.
package user

import (
	"strings"
	"time"
)

type Gender string

const (
	GenderMale   Gender = "LAKI_LAKI"
	GenderFemale Gender = "PEREMPUAN"
)

func (g Gender) Label() string {
	switch g {
	case GenderMale:
		return "Laki-laki"
	case GenderFemale:
		return "Perempuan"
	}
	return ""
}

type Profile struct {
	FirstName string
	LastName  string
	Phone     string // E.164 (+628…) or empty
	Gender    Gender // empty when not set
}

func (p Profile) FullName() string { return strings.TrimSpace(p.FirstName + " " + p.LastName) }

// Account is a user as shown on "Akun Saya".
type Account struct {
	UserID    int64
	Email     string
	CreatedAt time.Time
	Profile   Profile
}

// DisplayName is the full name, or the email's local part when no name is set.
func (a Account) DisplayName() string {
	if n := a.Profile.FullName(); n != "" {
		return n
	}
	local, _, _ := strings.Cut(a.Email, "@")
	return local
}

// ProfileInput is raw user input; the Service validates and normalizes it.
type ProfileInput struct {
	FirstName string
	LastName  string
	Phone     string
	Gender    string
}
