package api

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Environment variables that override the stored login.
const (
	EnvURL   = "WAYGLASS_URL"
	EnvKey   = "WAYGLASS_KEY"
	EnvAgent = "WAYGLASS_AGENT"
	// EnvFamily is the caller's model family (claude, openai, gemini, meta, other,
	// human) for policy rules that need a reviewer from another family.
	EnvFamily = "WAYGLASS_AGENT_FAMILY"
	// EnvConfigDir overrides the config directory (tests, sandboxes).
	EnvConfigDir = "WAYGLASS_CONFIG_DIR"
)

// Credentials is what `wayglass login` stores. The key is a Wayglass key,
// never a Cloudflare API token.
// TODO(keychain): move Key into the OS keychain; the file stays as fallback.
type Credentials struct {
	URL   string `yaml:"url" json:"url"`
	Key   string `yaml:"key" json:"-"`
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	// Source is "access" for a human session from 'wayglass login --human';
	// empty for a key stored with 'wayglass login'.
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	// ExpiresAt is when a human session key stops working (RFC 3339).
	ExpiresAt string `yaml:"expires_at,omitempty" json:"expires_at,omitempty"`
	// envKey is true when WAYGLASS_KEY replaced the stored key, so the
	// stored session expiry no longer describes the key in use.
	envKey bool
}

// SessionExpiry is when the stored human session ends; ok is false when
// the key in use has no recorded expiry (an admin or agent key, or a key
// from WAYGLASS_KEY).
func (c Credentials) SessionExpiry() (t time.Time, ok bool) {
	if c.envKey || c.ExpiresAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, c.ExpiresAt)
	return t, err == nil
}

// Identity is who requests are made as: the stored or $WAYGLASS_AGENT
// name, else DefaultAgent.
func (c Credentials) Identity() string {
	return cmp.Or(strings.TrimSpace(c.Agent), DefaultAgent())
}

// DefaultAgent is user@host (short host name) for whoever runs the binary,
// so nobody has to configure a name before their first command.
func DefaultAgent() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	if host = strings.Split(host, ".")[0]; host != "" && name != "" {
		return name + "@" + host
	}
	return cmp.Or(name, "unknown")
}

// ConfigDir is $WAYGLASS_CONFIG_DIR, else $XDG_CONFIG_HOME/wayglass, else
// ~/.config/wayglass (also on macOS, where tools in this stack live there).
func ConfigDir() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "wayglass"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "wayglass"), nil
}

// CredentialsPath is the login file.
func CredentialsPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "credentials.yaml"), nil
}

// LoadCredentials reads the stored login, then applies WAYGLASS_URL,
// WAYGLASS_KEY and WAYGLASS_AGENT on top. A missing file is not an error.
func LoadCredentials() (Credentials, error) {
	var c Credentials
	p, err := CredentialsPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // our own config file
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", p, err)
		}
	}
	if v := os.Getenv(EnvURL); v != "" {
		c.URL = v
	}
	if v := os.Getenv(EnvKey); v != "" {
		c.envKey = v != c.Key
		c.Key = v
	}
	if v := os.Getenv(EnvAgent); v != "" {
		c.Agent = v
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c, nil
}

// SaveCredentials writes the login file with mode 0600 in a 0700 directory.
// It writes a temp file and renames it, so a crash never leaves a
// world-readable or half-written key behind.
func SaveCredentials(c Credentials) (string, error) {
	if err := ValidateURL(c.URL); err != nil {
		return "", err
	}
	p, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return p, nil
}

// ValidateURL accepts https URLs, and http only for localhost (wrangler dev).
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("worker URL %q is not an absolute URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
		return fmt.Errorf("worker URL %q must use https (http is only allowed for localhost)", raw)
	default:
		return fmt.Errorf("worker URL %q must use https", raw)
	}
}

// Families are the model families a policy can tell apart; "human" is a
// person. Mirrors worker/src/family.ts.
var Families = []string{"claude", "openai", "gemini", "meta", "other", "human"}

var familyAliases = map[string]string{
	"anthropic": "claude", "codex": "openai", "gpt": "openai", "chatgpt": "openai",
	"google": "gemini", "llama": "meta", "person": "human",
}

// NormalizeFamily returns the family for a declared value ("" for none) and
// whether it is one the Worker accepts.
func NormalizeFamily(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" || slices.Contains(Families, v) {
		return v, true
	}
	f, ok := familyAliases[v]
	return f, ok
}
