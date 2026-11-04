package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"ecommerce/internal/validate"
)

const (
	// SessionTTL is how long a session lives without activity.
	SessionTTL = 30 * 24 * time.Hour

	// bcryptCost 10 matches the old Ashanum app, so its hashes stay valid,
	// and keeps a login around a few hundred ms on the HG680P's Cortex-A53.
	// Re-measure on the device in Phase 9 before raising it.
	bcryptCost = 10

	tokenBytes = 32
)

type Service struct {
	repo   *Repository
	secret []byte
	now    func() time.Time

	// hashSlots caps concurrent bcrypt work so a burst of logins cannot
	// take every CPU core from page rendering.
	hashSlots chan struct{}
	// dummyHash is compared against when an email is unknown, so the
	// response time does not reveal which emails are registered.
	dummyHash func() []byte
}

func NewService(repo *Repository, secret []byte) *Service {
	return &Service{
		repo:      repo,
		secret:    secret,
		now:       time.Now,
		hashSlots: make(chan struct{}, max(1, runtime.NumCPU()/2)),
		dummyHash: sync.OnceValue(func() []byte {
			h, err := bcrypt.GenerateFromPassword([]byte("ashanum-timing-equalizer"), bcryptCost)
			if err != nil {
				panic(err) // only fails for an invalid cost or >72-byte input
			}
			return h
		}),
	}
}

// Register creates an account with the CUSTOMER role (the database
// default; a role is never read from the form). Invalid input returns
// validate.Errors; an existing email returns ErrEmailTaken.
func (s *Service) Register(ctx context.Context, in RegisterInput) (User, error) {
	errs := validate.Errors{}
	email, ok := validate.Email(in.Email)
	if !ok {
		errs.Add("email", "Masukkan alamat email yang valid.")
	}
	validate.Password(errs, "password", in.Password)
	if in.PasswordConfirm != in.Password {
		errs.Add("password_confirm", "Konfirmasi password tidak sama.")
	}
	if err := errs.Err(); err != nil {
		return User{}, err
	}

	hash, err := s.hashPassword(ctx, in.Password)
	if err != nil {
		return User{}, err
	}
	return s.repo.CreateUser(ctx, email, hash)
}

// minAdminPassword is stricter than the customer minimum (8): an admin
// account can change orders, stock and refunds.
const minAdminPassword = 12

// CreateAdmin creates an ADMIN account, or with in.Promote makes an existing
// account ADMIN (password unchanged). The password is hashed and never
// logged. Each grant is written to the admin audit log.
func (s *Service) CreateAdmin(ctx context.Context, in AdminInput) (AdminResult, error) {
	errs := validate.Errors{}
	email, ok := validate.Email(in.Email)
	if !ok {
		errs.Add("email", "Masukkan alamat email yang valid.")
	}
	var hash string
	if !in.Promote {
		validate.Password(errs, "password", in.Password)
		if _, bad := errs["password"]; !bad && utf8.RuneCountInString(in.Password) < minAdminPassword {
			errs.Add("password", fmt.Sprintf("Password admin minimal %d karakter.", minAdminPassword))
		}
		validate.Name(errs, "first_name", in.FirstName, "Nama depan", true)
		validate.Name(errs, "last_name", in.LastName, "Nama belakang", false)
	}
	if err := errs.Err(); err != nil {
		return AdminResult{}, err
	}
	if !in.Promote {
		var err error
		if hash, err = s.hashPassword(ctx, in.Password); err != nil {
			return AdminResult{}, err
		}
	}
	return s.repo.CreateAdmin(ctx, email, hash, validate.Text(in.FirstName), validate.Text(in.LastName), in.Promote)
}

// Login checks credentials and starts a new session. Wrong email and wrong
// password both return ErrInvalidCredentials.
func (s *Service) Login(ctx context.Context, email, password string) (User, Session, error) {
	email, ok := validate.Email(email)
	if !ok || password == "" || len(password) > 72 {
		return User{}, Session{}, ErrInvalidCredentials
	}

	u, hash, err := s.repo.FindCredentials(ctx, email)
	if errors.Is(err, errUserNotFound) {
		s.comparePassword(ctx, s.dummyHash(), password)
		return User{}, Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, Session{}, err
	}

	switch err := s.comparePassword(ctx, []byte(hash), password); {
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return User{}, Session{}, ErrInvalidCredentials
	case err != nil:
		return User{}, Session{}, err
	}

	sess, err := s.StartSession(ctx, u.ID)
	return u, sess, err
}

// StartSession issues a fresh random token for a user (after login or
// registration). Expired sessions are purged here instead of by a
// background worker.
func (s *Service) StartSession(ctx context.Context, userID int64) (Session, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	sess := Session{
		Token:     base64.RawURLEncoding.EncodeToString(raw),
		ExpiresAt: s.now().Add(SessionTTL).Truncate(time.Second),
	}

	// Best effort: a failed cleanup must not block the login itself.
	_ = s.repo.DeleteExpiredSessions(ctx)

	if err := s.repo.CreateSession(ctx, s.tokenHash(sess.Token), userID, sess.ExpiresAt); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Authenticate resolves a token to its user. When less than half of the
// lifetime remains the session is extended and the new expiry returned, so
// the caller can refresh the cookie; most requests therefore cost one read.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, *Session, error) {
	if token == "" || len(token) > 128 {
		return Principal{}, nil, ErrSessionNotFound
	}
	hash := s.tokenHash(token)
	p, expiresAt, err := s.repo.ActiveSession(ctx, hash)
	if err != nil {
		return Principal{}, nil, err
	}

	now := s.now()
	if expiresAt.Sub(now) > SessionTTL/2 {
		return p, nil, nil
	}
	renewed := &Session{Token: token, ExpiresAt: now.Add(SessionTTL).Truncate(time.Second)}
	if err := s.repo.ExtendSession(ctx, hash, renewed.ExpiresAt); err != nil {
		return Principal{}, nil, err
	}
	return p, renewed, nil
}

// Logout deletes the session. Unknown tokens are not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.repo.DeleteSession(ctx, s.tokenHash(token))
}

// tokenHash keys the stored session id with SESSION_SECRET: a leaked
// sessions table cannot be turned back into cookies, and rotating the secret
// signs everyone out.
func (s *Service) tokenHash(token string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(token))
	return m.Sum(nil)
}

func (s *Service) hashPassword(ctx context.Context, password string) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(h), err
}

func (s *Service) comparePassword(ctx context.Context, hash []byte, password string) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return bcrypt.CompareHashAndPassword(hash, []byte(password))
}

func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.hashSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) release() { <-s.hashSlots }
