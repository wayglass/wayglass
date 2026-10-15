package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/notes"
)

// existingRepo is a repository someone already works in: history on master,
// a tag, an origin remote (a local bare repo standing in for GitHub), a
// .github directory, a binary file and a dirty working tree.
func existingRepo(t *testing.T, e *flowEnv) (dir, origin string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "legacy")
	e.git("", "init", "--quiet", "--initial-branch=master", dir)
	e.write(dir, "README.md", "# legacy\n")
	e.write(dir, ".github/workflows/ci.yml", "on: push\n")
	e.write(dir, "assets/logo.png", "\x89PNG\r\n\x1a\n\x00binary\x00")
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "--quiet", "-m", "first")
	e.git(dir, "tag", "v0.1.0")
	e.write(dir, "src/app.go", "package app\n")
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "--quiet", "-m", "second")
	origin = filepath.Join(t.TempDir(), "github.git")
	e.git("", "init", "--quiet", "--bare", "--initial-branch=master", origin)
	e.git(dir, "remote", "add", "origin", origin)
	e.git(dir, "push", "--quiet", "origin", "master", "--tags")
	e.git(dir, "remote", "set-head", "origin", "master")
	e.write(dir, "README.md", "# legacy\n\nuncommitted edit\n")
	e.write(dir, "scratch.txt", "untracked\n")
	return dir, origin
}

func TestInitExistingRepo(t *testing.T) {
	e := newFlow(t)
	dir, origin := existingRepo(t, e)

	got := e.do(dir, ExitOK, "init", "--project", "legacy", "--mirror", "origin")
	if get(got, "branch") != "master" || get(got, "pushed") != "main" || get(got, "branch_renamed") != false || get(got, "tags_pushed") != false {
		t.Fatalf("init = %v", got)
	}
	if get(got, "remote") != "wayglass" || get(got, "mirror") != "origin" || get(got, "no_token_on_disk") != true || get(got, "notes_refspec") != notesFollowRefspec {
		t.Fatalf("init = %v", got)
	}
	warns := get(got, "warnings").([]any)
	if len(warns) != 1 || !strings.Contains(warns[0].(string), "trunk branch is main; your local branch is master") {
		t.Fatalf("warnings = %v", warns)
	}
	trunk := get(got, "trunk", "remote").(string)

	// The whole history went to the trunk's main; tags stayed local.
	head := e.git(dir, "rev-parse", "HEAD")
	if got := e.git(dir, "ls-remote", trunk, "refs/heads/main"); !strings.HasPrefix(got, head) {
		t.Fatalf("trunk main = %q, want %s", got, head)
	}
	if tags := e.git(dir, "ls-remote", "--tags", trunk); tags != "" {
		t.Fatalf("tags on the trunk without --tags: %q", tags)
	}
	if n := e.git(dir, "rev-list", "--count", "HEAD"); n != "3" {
		t.Fatalf("commits = %s, want the two old ones and the init commit", n)
	}
	// The init commit says why and touches only .wayglass; the dirty tree is untouched.
	msg := e.git(dir, "log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "chore: bring this repository under Wayglass") || !strings.Contains(msg, "every change carries its why") {
		t.Fatalf("init commit message = %q", msg)
	}
	if files := e.git(dir, "show", "--name-only", "--format=", "HEAD"); strings.Contains(files, "README.md") || !strings.Contains(files, ".wayglass/policy.yaml") {
		t.Fatalf("init commit files = %q", files)
	}
	if st := e.git(dir, "status", "--porcelain"); !strings.Contains(st, " M README.md") || !strings.Contains(st, "?? scratch.txt") {
		t.Fatalf("working tree changed: %q", st)
	}
	// origin is untouched, the trunk is remote wayglass with the helper and
	// notes refspec, and wayglass/main tracks the push.
	if u := e.git(dir, "remote", "get-url", "origin"); u != origin {
		t.Fatalf("origin = %q", u)
	}
	if u := e.git(dir, "remote", "get-url", "wayglass"); u != trunk {
		t.Fatalf("wayglass remote = %q, want %q", u, trunk)
	}
	if got := e.git(dir, "rev-parse", "refs/remotes/wayglass/main"); got != head {
		t.Fatalf("wayglass/main = %s, want %s", got, head)
	}
	helpers := e.git(dir, "config", "--local", "--get-all", "credential."+trunk+".helper")
	if !strings.HasSuffix(helpers, " git-credential --project legacy") || !strings.HasPrefix(helpers, "\n!") {
		t.Fatalf("helpers = %q", helpers)
	}
	if f := e.git(dir, "config", "--local", "--get-all", "remote.wayglass.fetch"); !strings.Contains(f, notesFollowRefspec) {
		t.Fatalf("wayglass fetch refspecs = %q", f)
	}
	if f := e.git(dir, "config", "--local", "--get-all", "remote.origin.fetch"); strings.Contains(f, "notes") {
		t.Fatalf("origin fetch refspecs changed: %q", f)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, ".wayglass", "config.yaml"))
	if !strings.Contains(string(cfg), "mirror: origin") || !strings.Contains(string(cfg), "project: legacy") {
		t.Fatalf("config = %s", cfg)
	}
	assertNoTokens(t, dir, e.worker.tokens)

	// doctor rates the helper on the wayglass remote, not origin.
	if c := checksByName(t, e.do(dir, ExitOK, "doctor"))["helper"]; c["status"] != checkOK || !strings.Contains(c["detail"].(string), "git fetch wayglass") {
		t.Fatalf("helper check = %v", c)
	}

	// A second init is refused; so is a dir argument without --from.
	if got := e.do(dir, ExitError, "init", "--project", "legacy"); get(got, "error", "code") != "conflict" {
		t.Fatalf("second init = %v", got)
	}
}

func TestInitPushesTags(t *testing.T) {
	e := newFlow(t)
	dir, _ := existingRepo(t, e)
	got := e.do(dir, ExitOK, "init", "--project", "legacy", "--tags")
	if get(got, "tags_pushed") != true {
		t.Fatalf("init = %v", got)
	}
	trunk := get(got, "trunk", "remote").(string)
	if tags := e.git(dir, "ls-remote", "--tags", trunk); !strings.Contains(tags, "refs/tags/v0.1.0") {
		t.Fatalf("tags on the trunk = %q", tags)
	}
}

// Everything init refuses, it refuses before the project exists on the
// Worker, naming what to fix.
func TestInitRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, e *flowEnv, dir string)
		args     []string
		wantExit int
		wantCode string
		wantMsg  string
	}{
		{
			name:     "detached HEAD",
			setup:    func(_ *testing.T, e *flowEnv, dir string) { e.git(dir, "checkout", "--quiet", "--detach", "HEAD") },
			wantExit: ExitError, wantCode: CodeDetachedHead, wantMsg: "git switch",
		},
		{
			name: "file over the Artifacts limit anywhere in the history",
			setup: func(t *testing.T, e *flowEnv, dir string) {
				old := maxBlobSize
				maxBlobSize = 64
				t.Cleanup(func() { maxBlobSize = old })
				e.write(dir, "vendor/model.bin", strings.Repeat("x", 200))
				e.git(dir, "add", "-A")
				e.git(dir, "commit", "--quiet", "-m", "add a model")
				e.git(dir, "rm", "--quiet", "vendor/model.bin")
				e.git(dir, "commit", "--quiet", "-m", "drop the model")
			},
			wantExit: ExitError, wantCode: CodeFileTooLarge, wantMsg: "vendor/model.bin (0.0 MB)",
		},
		{
			name:     "mirror remote that does not exist",
			args:     []string{"--mirror", "github"},
			wantExit: ExitUsage, wantCode: CodeUsage, wantMsg: "no such git remote",
		},
		{
			name:     "mirror is the trunk remote name",
			args:     []string{"--mirror", "wayglass"},
			wantExit: ExitUsage, wantCode: CodeUsage, wantMsg: "trunk itself",
		},
		{
			name:     "dir argument without --from",
			args:     []string{"somewhere"},
			wantExit: ExitUsage, wantCode: CodeUsage, wantMsg: "--from",
		},
		{
			name:     "from a URL with a token in it",
			args:     []string{"--from", "https://x:token@github.com/a/b.git"},
			wantExit: ExitUsage, wantCode: CodeBadSource, wantMsg: "username or password",
		},
		{
			name:     "from a remote helper",
			args:     []string{"--from", "ext::sh -c id"},
			wantExit: ExitUsage, wantCode: CodeBadSource, wantMsg: "remote helper",
		},
		{
			name:     "from a local path without --allow-local",
			args:     []string{"--from", "/srv/git/x.git"},
			wantExit: ExitUsage, wantCode: CodeBadSource, wantMsg: "--allow-local",
		},
		{
			name:     "from into a directory that is not empty",
			args:     []string{"--from", "https://github.com/a/b.git", "."},
			wantExit: ExitUsage, wantCode: CodeUsage, wantMsg: "not empty",
		},
		{
			name:     "bad project name",
			args:     []string{"--project", "Bad--Name"},
			wantExit: ExitUsage, wantCode: CodeUsage,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlow(t)
			dir, _ := existingRepo(t, e)
			if tc.setup != nil {
				tc.setup(t, e, dir)
			}
			got := e.do(dir, tc.wantExit, append([]string{"init", "--project", "legacy"}, tc.args...)...)
			if get(got, "error", "code") != tc.wantCode {
				t.Fatalf("init = %v, want %s", got, tc.wantCode)
			}
			if msg, _ := get(got, "error", "message").(string); !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("message = %q, want %q", msg, tc.wantMsg)
			}
			if len(e.worker.projects) != 0 {
				t.Fatalf("a refused init created a project: %v", e.worker.projects)
			}
			if _, err := os.Stat(filepath.Join(dir, ".wayglass", "config.yaml")); err == nil {
				t.Fatal("a refused init wrote config.yaml")
			}
			if strings.Contains(tc.name, "token") {
				if out, _ := get(got, "error", "message").(string); strings.Contains(out, "x:token@") {
					t.Fatalf("the refusal repeats the credential: %s", out)
				}
			}
		})
	}
}

// --from: clone a repository (a local bare repo stands in for GitHub) with
// the person's own git credentials, bring the clone under Wayglass, land a
// change, and publish the trunk and its why-notes back to origin.
func TestInitFromThenPublish(t *testing.T) {
	e := newFlow(t)
	src, origin := existingRepo(t, e)
	// The source repo's own clone is the stand-in server; origin is bare.
	parent := t.TempDir()
	got := e.do(parent, ExitOK, "init", "--from", origin, "--allow-local", "--tags")
	dir := filepath.Join(parent, "github")
	if get(got, "dir") != dir || get(got, "source") != origin || get(got, "project", "name") != "github" {
		t.Fatalf("init --from = %v", got)
	}
	if get(got, "branch") != "master" || get(got, "pushed") != "main" || get(got, "branch_renamed") != true || get(got, "tags_pushed") != true {
		t.Fatalf("init --from = %v", got)
	}
	if b := e.git(dir, "symbolic-ref", "--short", "HEAD"); b != "main" {
		t.Fatalf("local branch = %s, want main (renamed to match the trunk)", b)
	}
	if u := e.git(dir, "remote", "get-url", "origin"); u != origin {
		t.Fatalf("origin = %q, want the source", u)
	}
	trunk := get(got, "trunk", "remote").(string)
	if u := e.git(dir, "remote", "get-url", "wayglass"); u != trunk {
		t.Fatalf("wayglass = %q", u)
	}
	if n := e.git(dir, "rev-list", "--count", "HEAD"); n != "3" {
		t.Fatalf("history = %s commits", n)
	}
	assertNoTokens(t, dir, e.worker.tokens)
	_ = src

	// Publishing before any land: origin/master is behind the trunk by the
	// init commit only, so it fast-forwards; no notes yet.
	pub := e.do(dir, ExitOK, "publish")
	if get(pub, "remote") != "origin" || get(pub, "branch") != "master" || get(pub, "notes_pushed") != false || get(pub, "commit") != e.git(dir, "rev-parse", "HEAD") {
		t.Fatalf("publish = %v", pub)
	}
	if got := e.git(dir, "ls-remote", origin, "refs/heads/master"); !strings.HasPrefix(got, e.git(dir, "rev-parse", "HEAD")) {
		t.Fatalf("origin master = %q", got)
	}

	// Land a docs change through an intent, then publish: origin gets the
	// landed head and refs/notes/wayglass.
	e.do(dir, ExitOK, "intent", "declare", "--goal", "Document the build", "--reason", "Asked twice this week", "--path", "docs/**")
	e.write(dir, "docs/build.md", "go build ./...\n")
	e.do(dir, ExitOK, "push", "-m", "docs: how to build")
	e.do(dir, ExitOK, "propose")
	landed := e.do(dir, ExitOK, "land", "--no-mirror")
	commit := get(landed, "commit").(string)
	pub = e.do(dir, ExitOK, "publish", "--json")
	if get(pub, "commit") != commit || get(pub, "notes_pushed") != true || get(pub, "no_token_on_disk") != true {
		t.Fatalf("publish after land = %v", pub)
	}
	if got := e.git(dir, "ls-remote", origin, "refs/heads/master"); !strings.HasPrefix(got, commit) {
		t.Fatalf("origin master = %q, want %s", got, commit)
	}
	if got := e.git(dir, "ls-remote", origin, notes.Ref); !strings.Contains(got, notes.Ref) {
		t.Fatalf("origin has no why-notes: %q", got)
	}
	assertNoTokens(t, dir, e.worker.tokens)

	// Someone pushed to origin directly: publish refuses rather than force.
	other := filepath.Join(t.TempDir(), "other")
	e.git("", "clone", "--quiet", origin, other)
	e.write(other, "HOTFIX.md", "hotfix\n")
	e.git(other, "add", "-A")
	e.git(other, "commit", "--quiet", "-m", "hotfix on GitHub")
	e.git(other, "push", "--quiet", "origin", "HEAD:master")
	if got := e.do(dir, ExitError, "publish"); get(got, "error", "code") != CodePublishRejected {
		t.Fatalf("publish onto a moved origin = %v", got)
	}

	// Refusals that touch nothing.
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"--remote", "wayglass"}, CodeUsage},
		{[]string{"--remote", "nope"}, CodeUsage},
		{[]string{"--remote", "--upload-pack=x"}, CodeUsage},
		{[]string{"--branch", "refs/heads/x"}, CodeUsage},
	} {
		if got := e.do(dir, ExitUsage, append([]string{"publish"}, tc.args...)...); get(got, "error", "code") != tc.code {
			t.Fatalf("publish %v = %v", tc.args, got)
		}
	}
	// publish in a checkout with no trunk: not initialised.
	if got := e.do(t.TempDir(), ExitError, "publish"); get(got, "error", "code") != CodeNotRepo {
		t.Fatalf("publish outside a repo = %v", got)
	}
}
