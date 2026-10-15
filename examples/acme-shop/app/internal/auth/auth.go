// Package auth signs people in with a password and issues signed session
// tokens. Passwords are stored only as salted PBKDF2 hashes.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionTTL is how long a session token stays valid.
const SessionTTL = 12 * time.Hour

const (
	iterations = 210_000
	keyLen     = 32
	minLength  = 12
)

// ErrInvalidCredentials is returned for an unknown email or a wrong
// password; callers cannot tell which, so emails cannot be enumerated.
var ErrInvalidCredentials = errors.New("invalid email or password")

type user struct {
	salt []byte
	hash []byte
}

// Store holds users and signs sessions with its key.
type Store struct {
	key   []byte
	now   func() time.Time
	mu    sync.Mutex
	users map[string]user
}

// NewStore returns an empty store that signs sessions with key.
func NewStore(key []byte) *Store {
	return &Store{key: key, now: time.Now, users: map[string]user{}}
}

func normalize(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// Add registers a user. Passwords shorter than 12 characters are refused.
func (s *Store) Add(email, password string) error {
	if len(password) < minLength {
		return fmt.Errorf("password must be at least %d characters", minLength)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[normalize(email)] = user{salt: salt, hash: hash}
	return nil
}

// Login checks a password and returns a session token.
func (s *Store) Login(email, password string) (string, error) {
	email = normalize(email)
	s.mu.Lock()
	u, ok := s.users[email]
	s.mu.Unlock()
	if !ok {
		return "", ErrInvalidCredentials
	}
	hash, err := pbkdf2.Key(sha256.New, password, u.salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare(hash, u.hash) != 1 {
		return "", ErrInvalidCredentials
	}
	return s.sign(email, s.now().Add(SessionTTL)), nil
}

// Verify returns the email a session token was issued to.
func (s *Store) Verify(token string) (string, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", errors.New("malformed session")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", errors.New("malformed session")
	}
	if !hmac.Equal([]byte(sig), []byte(s.mac(payload))) {
		return "", errors.New("bad session signature")
	}
	email, exp, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", errors.New("malformed session")
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || s.now().Unix() >= unix {
		return "", errors.New("session expired")
	}
	return email, nil
}

func (s *Store) sign(email string, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(email + "|" + strconv.FormatInt(exp.Unix(), 10)))
	return payload + "." + s.mac(payload)
}

func (s *Store) mac(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
