// Package auth handles the daemon's single-user web UI password +
// session cookies, and a separate "cli token" file that lets in-process
// CLI invocations skip the password.
//
// Single-user model: fnshare runs on the user's own NAS, so there's no
// multi-user permission story — just "is the visitor the owner?"
//
//   - Password stored as bcrypt hash in BadgerDB key "auth/password".
//   - Sessions: opaque random tokens kept in memory, valid for 30 days.
//   - CLI: daemon writes a random token to <data>/.cli-token on startup
//     (chmod 0600). The CLI reads it and sends as X-Auth-Token. Anyone
//     with read access to the data dir is the legit owner anyway.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fnshare/fnshare/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	keyPasswordHash = "auth/password"
	tokenTTL        = 30 * 24 * time.Hour
)

var ErrBadPassword = errors.New("auth: bad password")

type Service struct {
	store    *store.Store
	cliToken string

	mu       sync.Mutex
	sessions map[string]time.Time // token → expiry
}

// New initializes the auth service. Generates the CLI token and writes it
// to <dataDir>/.cli-token (mode 0600) so the in-container CLI can read it.
func New(s *store.Store, dataDir string) (*Service, error) {
	cliTok, err := newRandomToken()
	if err != nil {
		return nil, err
	}
	tokPath := filepath.Join(dataDir, ".cli-token")
	if err := os.WriteFile(tokPath, []byte(cliTok), 0o600); err != nil {
		return nil, err
	}
	return &Service{
		store:    s,
		cliToken: cliTok,
		sessions: map[string]time.Time{},
	}, nil
}

// HasPassword reports whether the owner has set a password yet.
func (s *Service) HasPassword() bool {
	_, err := s.store.Get([]byte(keyPasswordHash))
	return err == nil
}

// SetPassword stores a fresh bcrypt hash. Used on first-run setup AND
// when the owner wants to change their password from the UI.
func (s *Service) SetPassword(password string) error {
	if len(password) < 6 {
		return errors.New("auth: password must be at least 6 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.store.Put([]byte(keyPasswordHash), hash)
}

// VerifyAndIssue checks the password and returns a fresh session token.
func (s *Service) VerifyAndIssue(password string) (string, error) {
	hash, err := s.store.Get([]byte(keyPasswordHash))
	if err != nil {
		return "", ErrBadPassword
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		return "", ErrBadPassword
	}
	tok, err := newRandomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(tokenTTL)
	s.mu.Unlock()
	return tok, nil
}

// ValidateSession returns true if `token` is a live session OR matches
// the in-process CLI token. Empty/expired/unknown tokens return false.
func (s *Service) ValidateSession(token string) bool {
	if token == "" {
		return false
	}
	if token == s.cliToken {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Logout invalidates a session token. CLI tokens are not affected.
func (s *Service) Logout(token string) {
	if token == s.cliToken {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// CLIToken is the daemon-internal token; the CLI client reads it from
// <data>/.cli-token and sends as X-Auth-Token. Surfaced for tests.
func (s *Service) CLIToken() string { return s.cliToken }

func newRandomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
