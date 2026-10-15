package gitx

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var testToken = "art_v1_" + strings.Repeat("c0ffee", 6) + "abcd" + "?expires=1760000000"

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func TestTokenReachesGitAsExtraHeader(t *testing.T) {
	needGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1") // an inherited value must not clash
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Bearer stale")
	g := Git{Dir: t.TempDir(), Token: testToken}
	out, err := g.Run(t.Context(), "config", "--get-all", "http.extraHeader")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Authorization: Bearer " + testToken; out != want {
		t.Fatalf("http.extraHeader = %q, want %q", out, want)
	}
}

func TestNoTokenNoHeader(t *testing.T) {
	needGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "")
	g := Git{Dir: t.TempDir()}
	if _, err := g.Run(t.Context(), "config", "--get", "http.extraHeader"); err == nil {
		t.Fatal("expected no http.extraHeader without a token")
	}
}

func TestErrorsAndLogsNeverContainToken(t *testing.T) {
	needGit(t)
	var log bytes.Buffer
	g := Git{Dir: t.TempDir(), Token: testToken, Log: &log}
	// Not a repo, and the token is (wrongly) passed as an argument: both the
	// logged command line and the error must be redacted.
	_, err := g.Run(t.Context(), "rev-parse", "--verify", testToken)
	if err == nil {
		t.Fatal("expected rev-parse to fail outside a repo")
	}
	var gerr *Error
	if !errors.As(err, &gerr) || gerr.ExitCode == 0 {
		t.Fatalf("want *Error with non-zero exit, got %#v", err)
	}
	base, _, _ := strings.Cut(testToken, "?")
	for name, s := range map[string]string{"error": err.Error(), "log": log.String()} {
		if strings.Contains(s, base) {
			t.Errorf("%s leaks the token: %q", name, s)
		}
		if !strings.Contains(s, Redacted) {
			t.Errorf("%s was not redacted: %q", name, s)
		}
	}
}

func TestRejectsHeaderInjection(t *testing.T) {
	for _, tok := range []string{"a\r\nX-Evil: 1", "a b", "a\tb"} {
		_, err := Git{Token: tok}.Run(t.Context(), "version")
		if !errors.Is(err, ErrBadToken) {
			t.Errorf("token %q: err = %v, want ErrBadToken", tok, err)
		}
	}
}

func TestRedact(t *testing.T) {
	g := Git{Token: "secret-token"}
	tests := []struct{ in, want string }{
		{"fatal: secret-token bad", "fatal: [REDACTED] bad"},
		{"Authorization: Bearer abc.def", "Authorization: Bearer [REDACTED]"},
		{"url art_v1_" + strings.Repeat("0", 40) + "?expires=1 end", "url [REDACTED] end"},
		{"url art_v2_u_" + strings.Repeat("0", 40) + "?expires=1 end", "url [REDACTED] end"},
		{"nothing here", "nothing here"},
	}
	for _, tt := range tests {
		if got := g.redact(tt.in); got != tt.want {
			t.Errorf("redact(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestVersion(t *testing.T) {
	needGit(t)
	v, err := Git{}.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !AtLeast(v, 2, 0) {
		t.Fatalf("unexpected git version %q", v)
	}
}

func TestAtLeast(t *testing.T) {
	tests := []struct {
		v    string
		ma   int
		mi   int
		want bool
	}{
		{"2.47.1", 2, 40, true},
		{"2.39.5", 2, 40, false},
		{"3.0", 2, 40, true},
		{"2.40.0.windows.1", 2, 40, true},
		{"garbage", 2, 0, false},
	}
	for _, tt := range tests {
		if got := AtLeast(tt.v, tt.ma, tt.mi); got != tt.want {
			t.Errorf("AtLeast(%q, %d, %d) = %v", tt.v, tt.ma, tt.mi, got)
		}
	}
}

// TestOnlyGitxExecsGit enforces the AGENTS.md rule that git runs only here.
func TestOnlyGitxExecsGit(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	execGit := regexp.MustCompile(`exec\.Command(Context)?\([^)]*"git"`)
	self, _ := filepath.Abs(".")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "worker", "web", ".agents":
				return filepath.SkipDir
			}
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if execGit.Match(b) {
			t.Errorf("%s runs git directly; use internal/gitx", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
