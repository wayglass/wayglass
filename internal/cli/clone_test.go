package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/notes"
)

// The camera take: a fresh checkout made with `wayglass clone` lands another
// agent's approved change (merged onto a moved trunk, with commit signing
// configured but broken) and answers why from git.
func TestCloneThenLandAndWhy(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n"}, ExitOK)
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.do(s.a, ExitOK, "land")

	parent := t.TempDir()
	tokensBefore := len(s.e.worker.tokens)
	got := s.e.do(parent, ExitOK, "clone", "demo")
	dir := filepath.Join(parent, "demo")
	if get(got, "dir") != dir || get(got, "project") != "demo" || get(got, "head") != s.trunkHead() || get(got, "branch") != "main" {
		t.Fatalf("clone = %v", got)
	}
	if get(got, "notes_fetched") != true || get(got, "config_written") != false || get(got, "no_token_on_disk") != true || get(got, "token_scope") != "read" {
		t.Fatalf("clone = %v", got)
	}
	if len(s.e.worker.tokens) != tokensBefore+1 {
		t.Fatalf("clone minted %d tokens, want 1", len(s.e.worker.tokens)-tokensBefore)
	}
	assertNoTokens(t, dir, s.e.worker.tokens)
	if remote := s.e.git(dir, "remote", "get-url", "origin"); remote != s.trunk {
		t.Fatalf("origin = %q, want the plain trunk remote", remote)
	}

	// Signing is asked for but cannot work (no key reachable, as for agents).
	s.e.git(dir, "config", "commit.gpgsign", "true")
	s.e.git(dir, "config", "gpg.program", "false")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	l := s.e.do(dir, ExitOK, "land", s.idB)
	if get(l, "verified") != true || get(l, "fetched_from_fork") != true || get(l, "trunk_moved") != true || get(l, "merge_unsigned") != true {
		t.Fatalf("land from clone = %v", l)
	}
	if w := get(l, "warnings").([]any); len(w) == 0 || !strings.Contains(w[0].(string), "unsigned") {
		t.Fatalf("warnings = %v", w)
	}
	if s.trunkHead() != get(l, "commit") {
		t.Fatal("trunk does not hold the land merge")
	}

	w := s.e.do(dir, ExitOK, "why", "docs/b.md:1")
	if get(w, "source") != "note" || get(w, "note", "intent_id") != s.idB {
		t.Fatalf("why = %v", w)
	}
	assertNoTokens(t, dir, s.e.worker.tokens)
}

// With working signing (here: none configured) the land merge is made as
// before and not reported unsigned.
func TestLandMergeNotMarkedUnsignedWithoutSigning(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n"}, ExitOK)
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.do(s.a, ExitOK, "land")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	l := s.e.do(s.b, ExitOK, "land")
	if get(l, "trunk_moved") != true || get(l, "merge_unsigned") != nil {
		t.Fatalf("land = %v", l)
	}
}

func TestCloneCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(e *flowEnv, parent string) // after init (unless noInit)
		noInit   bool
		initArgs []string
		args     []string
		wantExit int
		wantCode string
		check    func(t *testing.T, e *flowEnv, got map[string]any)
	}{
		{
			name: "into a named directory",
			args: []string{"clone", "demo", "review-copy"},
			check: func(t *testing.T, _ *flowEnv, got map[string]any) {
				if !strings.HasSuffix(get(got, "dir").(string), "review-copy") || get(got, "notes_fetched") != false {
					t.Fatalf("clone = %v", got)
				}
			},
		},
		{
			name:     "empty trunk",
			initArgs: []string{"--no-push"},
			args:     []string{"clone", "demo"},
			check: func(t *testing.T, _ *flowEnv, got map[string]any) {
				if get(got, "head") != "" || get(got, "config_written") != true {
					t.Fatalf("clone = %v", got)
				}
				dir := get(got, "dir").(string)
				b, err := os.ReadFile(filepath.Join(dir, ".wayglass", "config.yaml"))
				if err != nil || !strings.Contains(string(b), "project: demo") {
					t.Fatalf("config = %q, %v", b, err)
				}
			},
		},
		{
			name: "non-empty target is refused before a token is minted",
			setup: func(e *flowEnv, parent string) {
				e.write(parent, "demo/keep.txt", "mine\n")
			},
			args:     []string{"clone", "demo"},
			wantExit: ExitUsage,
			wantCode: "usage",
			check: func(t *testing.T, e *flowEnv, _ map[string]any) {
				if n := len(e.worker.tokens); n != 1 { // only init's
					t.Fatalf("%d tokens minted", n)
				}
			},
		},
		{
			name:     "unknown project",
			args:     []string{"clone", "nope"},
			wantExit: ExitError,
			wantCode: "not_found",
		},
		{
			name:     "invalid project name",
			noInit:   true,
			args:     []string{"clone", "Bad--Name"},
			wantExit: ExitUsage,
			wantCode: "usage",
		},
		{
			name:     "missing project argument",
			noInit:   true,
			args:     []string{"clone"},
			wantExit: ExitUsage,
			wantCode: "usage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlow(t)
			if !tc.noInit {
				e.do(e.repo, ExitOK, append([]string{"init", "--project", "demo"}, tc.initArgs...)...)
			}
			parent := t.TempDir()
			if tc.setup != nil {
				tc.setup(e, parent)
			}
			got := e.do(parent, cmpOr(tc.wantExit, ExitOK), tc.args...)
			if tc.wantCode != "" && get(got, "error", "code") != tc.wantCode {
				t.Fatalf("clone = %v, want %s", got, tc.wantCode)
			}
			if tc.check != nil {
				tc.check(t, e, got)
			}
			assertNoTokens(t, parent, e.worker.tokens)
		})
	}
}

func TestTokenInGitDir(t *testing.T) {
	dir := t.TempDir()
	tok := "art_v1_" + strings.Repeat("ab", 20) + "?expires=1"
	if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Compressed objects are skipped; config is not.
	if err := os.WriteFile(filepath.Join(dir, "objects", "pack", "x"), []byte(tok), 0o644); err != nil {
		t.Fatal(err)
	}
	if found, err := tokenInGitDir(dir, tok); err != nil || found != "" {
		t.Fatalf("found %q, %v", found, err)
	}
	base, _, _ := strings.Cut(tok, "?")
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("extraHeader = Bearer "+base), 0o644); err != nil {
		t.Fatal(err)
	}
	if found, err := tokenInGitDir(dir, tok); err != nil || found != filepath.Join(dir, "config") {
		t.Fatalf("found %q, %v", found, err)
	}
}

// A wayglass clone follows the why-notes with plain git: fetch brings
// refs/notes/wayglass along once there are notes (and does not fail while
// there are none), and git log shows the why under each commit, so tig,
// lazygit and the like show it too. The intent fork clone gets the same.
func TestCloneFollowsNotesWithPlainGit(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	parent := t.TempDir()
	got := s.e.do(parent, ExitOK, "clone", "demo")
	dir := filepath.Join(parent, "demo")
	if get(got, "notes_fetched") != false || get(got, "notes_refspec") != notesFollowRefspec {
		t.Fatalf("clone = %v", got)
	}
	checkFollow := func(dir string) {
		t.Helper()
		if specs := s.e.git(dir, "config", "--local", "--get-all", "remote.origin.fetch"); !strings.HasSuffix(specs, "\n"+notesFollowRefspec) {
			t.Fatalf("remote.origin.fetch = %q, want the notes refspec after the branches", specs)
		}
		if ref := s.e.git(dir, "config", "--local", "notes.displayRef"); ref != notes.Ref {
			t.Fatalf("notes.displayRef = %q", ref)
		}
		// No notes on the remote yet: a plain fetch and pull still work.
		s.e.git(dir, "fetch", "--quiet", "origin")
		s.e.git(dir, "pull", "--quiet", "--ff-only")
	}
	checkFollow(dir)

	// Something lands with its why; a plain pull brings the note.
	s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
	s.e.do(s.a, ExitOK, "land")
	s.e.git(dir, "pull", "--quiet", "--ff-only")
	if head := s.e.git(dir, "rev-parse", "HEAD"); head != s.trunkHead() {
		t.Fatalf("HEAD %s, want the landed trunk %s", head, s.trunkHead())
	}
	want := strings.Fields(s.e.git(s.a, "ls-remote", s.trunk, notes.Ref))[0]
	if have := s.e.git(dir, "rev-parse", notes.Ref); have != want {
		t.Fatalf("%s = %s after pull, want the trunk's %s", notes.Ref, have, want)
	}
	log := s.e.git(dir, "log", "-1")
	if !strings.Contains(log, "Notes (wayglass):") || !strings.Contains(log, s.idA) {
		t.Fatalf("git log -1 does not show the why:\n%s", log)
	}
	if w := s.e.do(dir, ExitOK, "why", "docs/a.md:1"); get(w, "source") != "note" || get(w, "note", "intent_id") != s.idA {
		t.Fatalf("why = %v", w)
	}

	// The intent fork clone follows notes the same way (the fork has none).
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n"}, ExitOK)
	fk := s.e.do(parent, ExitOK, "clone", "demo", "--intent", s.idB)
	if get(fk, "notes_refspec") != notesFollowRefspec {
		t.Fatalf("clone --intent = %v", fk)
	}
	checkFollow(filepath.Join(parent, "demo-"+s.idB))
}
