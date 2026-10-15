package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
)

// credentialUsername is what the helper reports as the user. Artifacts
// checks only the token in the password slot.
const credentialUsername = "wayglass"

// credentialInputLimit bounds what the helper reads from git.
const credentialInputLimit = 64 << 10

// intentIDRe matches an intent id as the Worker mints them.
var intentIDRe = regexp.MustCompile(`^i[0-9a-f]{10}$`)

// gitCredential is the parsed stdin of one credential-helper call.
type gitCredential struct {
	Protocol string
	Host     string
	Path     string
}

func newGitCredentialCmd(a *app) *cobra.Command {
	var project, intent string
	cmd := &cobra.Command{
		Use:   "git-credential <get|store|erase>",
		Short: "Plumbing: git credential helper that mints a read-only token per fetch",
		Long: "The git credential helper 'wayglass clone' configures in a checkout, so plain\n" +
			"'git fetch' and 'git pull' keep working after the clone's token has expired.\n" +
			"On 'get' it asks the Worker for a fresh read-only token for the project's\n" +
			"trunk (or, with --intent, that intent's fork) using your stored Wayglass key\n" +
			"and answers git with it on stdout. Nothing is stored: 'store' and 'erase'\n" +
			"do nothing, and the token is never written to disk or logged. Read only:\n" +
			"pushes still go through 'wayglass push' and 'wayglass land'.\n\n" +
			"Git runs this; you do not. In .git/config it looks like:\n\n" +
			"  [credential \"https://<account>.artifacts.cloudflare.net/git/<ns>/<repo>.git\"]\n" +
			"      helper =\n" +
			"      helper = !wayglass git-credential --project <project>",
		Example: "  printf 'protocol=https\\nhost=x.artifacts.cloudflare.net\\n\\n' | wayglass git-credential --project demo get",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := args[0]
			switch op {
			case "store", "erase":
				return nil // nothing is ever kept
			case "get":
			default:
				return UsageError("operation must be get, store or erase, not %q", op)
			}
			if project == "" {
				return UsageError("--project is required")
			}
			if !validProjectName(project) {
				return UsageError("%q is not a project name", project)
			}
			if intent != "" && !intentIDRe.MatchString(intent) {
				return UsageError("%q is not an intent id", intent)
			}
			req, err := readGitCredential(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if err := serveGitCredential(cmd.Context(), a, cmd.OutOrStdout(), req, project, intent); err != nil {
				// No other helper can answer and a typed password cannot
				// exist: tell git to stop instead of prompting. (--json
				// keeps stdout to the envelope; git never passes it.)
				if !a.asJSON {
					_, _ = io.WriteString(cmd.OutOrStdout(), "quit=1\n")
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "the Wayglass project the remote belongs to")
	cmd.Flags().StringVar(&intent, "intent", "", "mint for this intent's fork instead of the trunk")
	return cmd
}

// serveGitCredential answers one "get": a fresh read token for the project's
// trunk (or the intent's fork), only if git's request is for that remote.
func serveGitCredential(ctx context.Context, a *app, out io.Writer, req *gitCredential, project, intent string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	src, err := mintReadToken(ctx, c, project, intent)
	if api.IsCode(err, api.CodeUnauthorized) {
		// Git shows this to someone who typed "git fetch": say what to do.
		return fmt.Errorf("the Worker refused the stored Wayglass key (%w); run wayglass login, then fetch again", err)
	}
	if err != nil {
		return err
	}
	if err := req.matches(src.Remote); err != nil {
		return err
	}
	return writeGitCredential(out, src.Token)
}

// readSource is a repo a read token was minted for.
type readSource struct {
	Name   string
	Remote string
	Token  api.Token
	// Intent is set when the source is an intent's fork.
	Intent *api.Intent
}

// mintReadToken asks the Worker for a read-only token: the project's trunk
// (about 60 s) or, with intentID, that intent's fork (about 300 s).
func mintReadToken(ctx context.Context, c *api.Client, project, intentID string) (*readSource, error) {
	if intentID == "" {
		tok, err := c.TrunkReadToken(ctx, project)
		if err != nil {
			return nil, err
		}
		return &readSource{Name: tok.Trunk.Name, Remote: tok.Trunk.Remote, Token: tok.Token}, nil
	}
	tok, err := c.ReadToken(ctx, project, intentID)
	if err != nil {
		return nil, err
	}
	it := tok.Intent
	return &readSource{Name: tok.Fork.Repo, Remote: tok.Fork.Remote, Token: tok.Token, Intent: &it}, nil
}

// readGitCredential parses git's key=value lines up to a blank line or EOF.
func readGitCredential(r io.Reader) (*gitCredential, error) {
	var c gitCredential
	sc := bufio.NewScanner(io.LimitReader(r, credentialInputLimit))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errorf(CodeUsage, ExitUsage, "bad credential line from git: %q", line)
		}
		switch k {
		case "protocol":
			c.Protocol = v
		case "host":
			c.Host = v
		case "path":
			c.Path = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return &c, nil
}

// matches refuses to hand a token for remote to a request for another host
// or repo: git only calls the helper for URLs under the configured one (same
// protocol and host, path prefix on a "/" boundary), but the token is scoped
// to one repo and should go nowhere else. Git sends a path only with
// credential.useHttpPath; without one the host decides.
func (c *gitCredential) matches(remote string) error {
	u, err := url.Parse(remote)
	if err != nil {
		return fmt.Errorf("the Worker returned a remote that does not parse: %w", err)
	}
	if c.Protocol != "" && c.Protocol != u.Scheme {
		return errorf(CodeError, ExitError, "git asked for %s credentials; this helper serves %s", c.Protocol, u.Scheme)
	}
	if c.Host != "" && !strings.EqualFold(hostWithPort(c.Host, u.Scheme), hostWithPort(u.Host, u.Scheme)) {
		return errorf(CodeError, ExitError, "git asked for credentials for %s; this helper serves %s", c.Host, u.Host)
	}
	if c.Path != "" && !pathUnder("/"+strings.TrimPrefix(c.Path, "/"), u.Path) {
		return errorf(CodeError, ExitError, "git asked for credentials for %s; this helper serves %s", c.Path, u.Path)
	}
	return nil
}

// hostWithPort makes "host" and "host:<default port>" compare equal.
func hostWithPort(host, scheme string) string {
	if strings.Contains(host, ":") {
		return host
	}
	switch scheme {
	case "https":
		return host + ":443"
	case "http":
		return host + ":80"
	}
	return host
}

// pathUnder reports whether p is base or below it: "/a/b.git" and
// "/a/b.git/info/refs" are under "/a/b.git"; "/a/b.gitx", "/a/b.git.x" and
// anything with a ".." segment are not.
func pathUnder(p, base string) bool {
	base = strings.TrimSuffix(base, "/")
	rest, ok := strings.CutPrefix(p, base)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return false
	}
	return !slices.Contains(strings.Split(p, "/"), "..")
}

// writeGitCredential answers git. password_expiry_utc (git 2.41+) tells git
// the token's lifetime, so a caching helper later in the chain would not
// keep it longer; clone resets the chain anyway.
func writeGitCredential(w io.Writer, tok api.Token) error {
	if strings.ContainsFunc(tok.Value, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return gitx.ErrBadToken
	}
	if _, err := fmt.Fprintf(w, "username=%s\npassword=%s\n", credentialUsername, tok.Value); err != nil {
		return err
	}
	if exp := tokenExpiryUnix(tok); exp > 0 {
		if _, err := fmt.Fprintf(w, "password_expiry_utc=%d\n", exp); err != nil {
			return err
		}
	}
	return nil
}

// tokenExpiryUnix reads the expiry Artifacts appends to a token
// ("?expires=<unix>"); 0 when there is none.
func tokenExpiryUnix(tok api.Token) int64 {
	_, q, ok := strings.Cut(tok.Value, "?")
	if !ok {
		return 0
	}
	v, _ := strings.CutPrefix(q, "expires=")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// credentialHelperValue is the git config value that runs this binary as
// the credential helper for one project (or intent fork).
func credentialHelperValue(exe, project, intent string) string {
	v := "!" + shellQuote(exe) + " git-credential --project " + project
	if intent != "" {
		v += " --intent " + intent
	}
	return v
}

// helperExecutable decides how git finds wayglass: by name when the
// 'wayglass' on PATH is this binary (survives reinstalls), else by this
// binary's absolute path.
func helperExecutable() string {
	self, err := os.Executable()
	if err != nil {
		return "wayglass"
	}
	if onPath, err := exec.LookPath("wayglass"); err == nil && sameFile(onPath, self) {
		return "wayglass"
	}
	if abs, err := filepath.EvalSymlinks(self); err == nil {
		self = abs
	}
	// Git runs "!" helpers through sh, on Windows too: forward slashes
	// survive that shell, backslashes do not.
	return filepath.ToSlash(self)
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// shellQuote quotes s for the sh -c git uses to run a "!" helper.
func shellQuote(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:,@%", r)
	}
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool { return !safe(r) }) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// helperCommand extracts the executable from a "!..." helper config value
// (the inverse of credentialHelperValue), or "" for any other helper.
func helperCommand(value string) string {
	rest, ok := strings.CutPrefix(value, "!")
	if !ok {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "'") {
		end := strings.Index(rest[1:], "'")
		if end < 0 {
			return ""
		}
		return rest[1 : end+1]
	}
	exe, _, _ := strings.Cut(rest, " ")
	return exe
}

// configureCredentialHelper makes plain git in dir mint read tokens through
// this binary for remote only: the first empty helper resets any inherited
// helper (osxkeychain and the like would otherwise be offered the token to
// store), then ours answers.
func configureCredentialHelper(ctx context.Context, g gitx.Git, remote, helper string) error {
	key := "credential." + remote + ".helper"
	if _, err := g.Run(ctx, "config", "--local", "--add", key, ""); err != nil {
		return err
	}
	_, err := g.Run(ctx, "config", "--local", "--add", key, helper)
	return err
}

// configuredHelpers lists the helpers set for remote in dir's local config,
// in order, empty (reset) entries included; nil when none is set.
func configuredHelpers(ctx context.Context, g gitx.Git, remote string) ([]string, error) {
	// -z: NUL after each value, so a trailing empty entry survives the
	// newline trimming a plain listing gets.
	out, err := g.Run(ctx, "config", "--local", "-z", "--get-all", "credential."+remote+".helper")
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.ExitCode == 1 {
		return nil, nil // unset
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}
