// Package gitx is the only place Wayglass runs the system git binary.
//
// Credentials: a repo token is handed to git as the config value
// http.extraHeader="Authorization: Bearer <token>". It is injected through
// git's GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n environment
// variables, which git treats exactly like `-c key=value`, so the token never
// appears in the process argument list (visible to every user via ps), on
// disk, or in a remote URL. Errors and logged command lines are redacted.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Redacted replaces any credential in output, errors and logs.
const Redacted = "[REDACTED]"

// Git runs git commands in one working directory.
type Git struct {
	// Dir is the working directory; empty means the current directory.
	Dir string
	// Token is an Artifacts repo token. When set, every command sends it as
	// an Authorization: Bearer header. Never log it.
	Token string
	// Env adds environment variables (KEY=VALUE) to every command.
	Env []string
	// Log, when set, receives each command line with credentials redacted.
	Log io.Writer
	// Binary overrides the git executable; empty means "git" on PATH.
	Binary string
}

// Error is a failed git command. Its text never contains the token.
type Error struct {
	Args     []string // redacted
	ExitCode int
	Stderr   string // redacted
	Err      error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ErrBadToken is returned for a token that could inject extra headers.
var ErrBadToken = errors.New("gitx: token contains whitespace or control characters")

// ErrBadArg is returned for a revision, ref, remote or directory that git
// would read as an option (it starts with "-"). Those values come from the
// Worker, a config file or a flag, never from this package's own literals,
// so one that looks like "--upload-pack=..." or "--output=..." is refused
// before git sees it.
var ErrBadArg = errors.New("gitx: argument would be read as a git option")

// checkArgs refuses values git would parse as options. A refspec may start
// with "+" (force); nothing legitimate starts with "-".
func checkArgs(vals ...string) error {
	for _, v := range vals {
		if strings.HasPrefix(v, "-") {
			return fmt.Errorf("%w: %q", ErrBadArg, v)
		}
	}
	return nil
}

// Run executes git with args and returns trimmed stdout.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	return g.RunInput(ctx, nil, args...)
}

// RunInput is Run with stdin. Without stdin git reads from an empty input,
// so it can never block waiting on a terminal.
func (g Git) RunInput(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	env, err := g.env()
	if err != nil {
		return "", err
	}
	bin := g.Binary
	if bin == "" {
		bin = "git"
	}
	redactedArgs := make([]string, len(args))
	for i, a := range args {
		redactedArgs[i] = g.redact(a)
	}
	if g.Log != nil {
		fmt.Fprintf(g.Log, "git %s\n", strings.Join(redactedArgs, " "))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = g.Dir
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		e := &Error{Args: redactedArgs, ExitCode: -1, Stderr: g.redact(stderr.String()), Err: err}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			e.ExitCode = exitErr.ExitCode()
			e.Err = fmt.Errorf("exit status %d", e.ExitCode)
		}
		return "", e
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

// env builds the child environment: the parent's, minus any inherited
// command-scope config, plus non-interactive settings and the auth header.
func (g Git) env() ([]string, error) {
	env := make([]string, 0, len(os.Environ())+len(g.Env)+6)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0", // fail instead of prompting for credentials
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"LC_ALL=C", // stable, parseable messages
	)
	env = append(env, g.Env...)
	if g.Token != "" {
		if strings.ContainsFunc(g.Token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return nil, ErrBadToken
		}
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Bearer "+g.Token,
		)
	}
	return env, nil
}

var (
	// Docs show art_v1_<hex>; real tokens since Oct 2026 are art_v2_<letter>_<hex>.
	artifactsToken = regexp.MustCompile(`art_v\d+_(?:[a-z]+_)?[0-9a-fA-F]{40}(\?expires=\d+)?`)
	bearer         = regexp.MustCompile(`(?i)(bearer\s+)\S+`)
)

func (g Git) redact(s string) string {
	if g.Token != "" {
		s = strings.ReplaceAll(s, g.Token, Redacted)
		if base, _, ok := strings.Cut(g.Token, "?"); ok && base != "" {
			s = strings.ReplaceAll(s, base, Redacted)
		}
	}
	s = artifactsToken.ReplaceAllString(s, Redacted)
	return bearer.ReplaceAllString(s, "${1}"+Redacted)
}

// Version returns the installed git version, e.g. "2.47.1".
func (g Git) Version(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "version")
	if err != nil {
		return "", err
	}
	v := strings.TrimPrefix(out, "git version ")
	v, _, _ = strings.Cut(v, " ")
	return v, nil
}

// AtLeast reports whether version string v (e.g. "2.47.1") is >= major.minor.
func AtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}
