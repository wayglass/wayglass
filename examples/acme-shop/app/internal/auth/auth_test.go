package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore([]byte("test-key-test-key-test-key-test-key"))
	if err := s.Add("Ada@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoginAndVerify(t *testing.T) {
	s := newTestStore(t)
	tok, err := s.Login(" ada@EXAMPLE.com ", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if email, err := s.Verify(tok); err != nil || email != "ada@example.com" {
		t.Fatalf("Verify = %q, %v", email, err)
	}
}

func TestLoginFailuresLookTheSame(t *testing.T) {
	s := newTestStore(t)
	for _, tc := range []struct{ email, pw string }{
		{"ada@example.com", "wrong password!"},
		{"nobody@example.com", "correct horse battery"},
	} {
		if _, err := s.Login(tc.email, tc.pw); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("Login(%s) err = %v, want ErrInvalidCredentials", tc.email, err)
		}
	}
}

func TestShortPasswordRefused(t *testing.T) {
	if err := NewStore([]byte("k")).Add("a@example.com", "short"); err == nil {
		t.Fatal("Add accepted a short password")
	}
}

func TestVerifyRejectsTamperedAndExpired(t *testing.T) {
	s := newTestStore(t)
	tok, _ := s.Login("ada@example.com", "correct horse battery")
	payload, sig, _ := strings.Cut(tok, ".")
	if _, err := s.Verify(payload + "x." + sig); err == nil {
		t.Error("Verify accepted a tampered payload")
	}
	s.now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if _, err := s.Verify(tok); err == nil {
		t.Error("Verify accepted an expired session")
	}
}
