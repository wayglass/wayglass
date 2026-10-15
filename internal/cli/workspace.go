package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
)

// Error codes for setup problems. Worker failures keep the Worker's own code
// (unauthorized, conflict, not_found, ...).
const (
	CodeNotLoggedIn    = "not_logged_in"
	CodeNotRepo        = "not_a_repo"
	CodeNotInitialized = "not_initialized"
	CodeNoIntent       = "no_intent"
	CodeNotPushed      = "not_pushed"
	CodeNoToken        = "no_token"
	CodeNoWhy          = "no_why"
	CodePolicy         = "policy_invalid"
)

// Environment variables read by the CLI (api.Env* cover the login).
const (
	EnvProject = "WAYGLASS_PROJECT"
	EnvIntent  = "WAYGLASS_INTENT"
	// EnvForkToken is a fork write token the agent kept from `intent
	// declare --json`. Used only if the Worker cannot mint a fresh one.
	EnvForkToken = "WAYGLASS_FORK_TOKEN"
)

// Files under .wayglass/. policy.yaml and config.yaml are committed; state/
// is local and git-ignored. No token is ever written here.
const (
	dirName        = ".wayglass"
	policyFile     = "policy.yaml"
	configFile     = "config.yaml"
	ignoreFile     = ".gitignore"
	stateDir       = "state"
	currentFile    = "current"
	ignoreContents = "# Local Wayglass state. Never holds tokens.\nstate/\n"
)

func errorf(code string, exit int, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Exit: exit}
}

// projectConfig is .wayglass/config.yaml.
type projectConfig struct {
	Version     int    `yaml:"version" json:"version"`
	Project     string `yaml:"project" json:"project"`
	TrunkRepo   string `yaml:"trunk_repo,omitempty" json:"trunk_repo,omitempty"`
	TrunkRemote string `yaml:"trunk_remote,omitempty" json:"trunk_remote,omitempty"`
	// Jurisdiction of the Artifacts namespace holding the project; it tags
	// every change it classifies.
	Jurisdiction string `yaml:"jurisdiction" json:"jurisdiction"`
	// Mirror is a git remote (e.g. origin on GitHub) that `wayglass land`
	// pushes the trunk branch and notes to after landing. Empty: no mirror.
	Mirror string `yaml:"mirror,omitempty" json:"mirror,omitempty"`
}

// workspace is the git checkout a command runs in.
type workspace struct {
	root string
	git  gitx.Git
	cfg  projectConfig
}

func (w *workspace) path(parts ...string) string {
	return filepath.Join(append([]string{w.root, dirName}, parts...)...)
}

// openRepo finds the enclosing git checkout.
func openRepo(ctx context.Context) (*workspace, error) {
	root, err := (gitx.Git{}).TopLevel(ctx)
	if err != nil {
		return nil, errorf(CodeNotRepo, ExitError, "not inside a git repository")
	}
	return &workspace{root: root, git: gitx.Git{Dir: root}}, nil
}

// openWorkspace finds the checkout and loads .wayglass/config.yaml.
// WAYGLASS_PROJECT overrides the project name.
func openWorkspace(ctx context.Context) (*workspace, error) {
	w, err := openRepo(ctx)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(w.path(configFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if p := os.Getenv(EnvProject); p != "" {
			w.cfg = projectConfig{Version: 1, Project: p}
			return w, nil
		}
		return nil, errorf(CodeNotInitialized, ExitError, "no %s/%s here; run 'wayglass init' first", dirName, configFile)
	case err != nil:
		return nil, err
	}
	if err := yaml.Unmarshal(b, &w.cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", w.path(configFile), err)
	}
	if p := os.Getenv(EnvProject); p != "" {
		w.cfg.Project = p
	}
	if w.cfg.Project == "" {
		return nil, errorf(CodeNotInitialized, ExitError, "%s has no project", w.path(configFile))
	}
	return w, nil
}

func (w *workspace) saveConfig() error {
	b, err := yaml.Marshal(w.cfg)
	if err != nil {
		return err
	}
	return w.write(append([]byte("# Written by wayglass init. Safe to commit: holds no secrets.\n"), b...), configFile)
}

// CodeUnsafePath: a file wayglass would write under the checkout sits behind
// a symlink or a non-directory. A cloned fork can commit `.wayglass/state` as
// a symlink to anywhere; writing through it would land session files outside
// the checkout.
const CodeUnsafePath = "unsafe_path"

// write puts b at .wayglass/<parts...> without ever leaving the checkout:
// the write is rooted at w.root (os.Root refuses paths that escape it) and
// refuses every symlink on the way, even one that stays inside the root.
func (w *workspace) write(b []byte, parts ...string) error {
	return writeUnder(w.root, filepath.Join(append([]string{dirName}, parts...)...), b)
}

// writeUnder writes rel (slash- or OS-separated, relative) below root as
// w.write does. Directories are created as needed. A symlink anywhere in
// rel, or a non-directory where a directory is needed, is refused.
func writeUnder(root, rel string, b []byte) error {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errorf(CodeUnsafePath, ExitError, "refusing to write %s: not inside %s", rel, root)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		sub := filepath.Join(parts[:i+1]...)
		fi, err := r.Lstat(sub)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return errorf(CodeUnsafePath, ExitError, "refusing to write through symlink %s in %s (committed by the fork?); remove it first", filepath.ToSlash(sub), root)
		case i < len(parts)-1 && !fi.IsDir():
			return errorf(CodeUnsafePath, ExitError, "refusing to write %s: %s is not a directory", filepath.ToSlash(rel), filepath.ToSlash(sub))
		}
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return r.WriteFile(rel, b, 0o644)
}

// writeFileNoFollow writes a file at a user-chosen absolute path (not under
// a workspace root), refusing to follow a symlink in its final component.
func writeFileNoFollow(path string, b []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return errorf(CodeUnsafePath, ExitError, "refusing to write through symlink %s", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|nofollow, 0o644) //nolint:gosec // the user's own instructions file
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// currentIntent reads the intent id this checkout last declared.
func (w *workspace) currentIntent() string {
	b, err := os.ReadFile(w.path(stateDir, currentFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (w *workspace) setCurrentIntent(id string) error {
	if err := w.write([]byte(ignoreContents), ignoreFile); err != nil {
		return err
	}
	return w.write([]byte(id+"\n"), stateDir, currentFile)
}

// resolveIntent picks the intent: the argument, then WAYGLASS_INTENT, then
// the one this checkout last declared.
func (w *workspace) resolveIntent(args []string) (string, error) {
	switch {
	case len(args) > 0 && args[0] != "":
		return args[0], nil
	case os.Getenv(EnvIntent) != "":
		return os.Getenv(EnvIntent), nil
	}
	if id := w.currentIntent(); id != "" {
		return id, nil
	}
	return "", errorf(CodeNoIntent, ExitUsage, "no intent given; pass an intent id, set %s, or run 'wayglass intent declare'", EnvIntent)
}

// baseRef holds the fork's tip before this checkout first pushed to it, so
// changed files are always measured from the same point.
func baseRef(intentID string) string { return "refs/wayglass/" + intentID + "/base" }

// client builds a Worker client from the stored login and environment.
func (*app) client() (*api.Client, error) {
	creds, err := api.LoadCredentials()
	if err != nil {
		return nil, err
	}
	if creds.URL == "" || creds.Key == "" {
		return nil, errorf(CodeNotLoggedIn, ExitError, "not logged in; run 'wayglass login --worker <url>' or set %s and %s", api.EnvURL, api.EnvKey)
	}
	c := api.New(creds.URL, creds.Key)
	c.Agent = creds.Identity()
	f, ok := api.NormalizeFamily(os.Getenv(api.EnvFamily))
	if !ok {
		return nil, UsageError("%s=%q is not one of %s", api.EnvFamily, os.Getenv(api.EnvFamily), strings.Join(api.Families, ", "))
	}
	c.Family = f
	return c, nil
}

var projectNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// projectNameFrom turns a directory name into a valid project name: lower
// case, only [a-z0-9._-], no "--" (it separates project and intent in fork
// names), at most 63 characters.
func projectNameFrom(dir string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(dir) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-._")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-._")
	}
	return s
}

func validProjectName(s string) bool {
	return projectNameRe.MatchString(s) && !strings.Contains(s, "--")
}
