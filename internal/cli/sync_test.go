package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// pushToTrunk commits files straight to the trunk from a scratch clone, the
// way another intent's land (or a person) moves it.
func (s *twoAgents) pushToTrunk(files map[string]string) string {
	s.e.t.Helper()
	dir := filepath.Join(s.e.t.TempDir(), "trunk-edit")
	s.e.git("", "clone", "--quiet", s.trunk, dir)
	for name, body := range files {
		s.e.write(dir, name, body)
	}
	s.e.git(dir, "add", "-A")
	s.e.git(dir, "commit", "--quiet", "-m", "trunk: move")
	s.e.git(dir, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	return s.trunkHead()
}

func TestSync(t *testing.T) {
	for _, tc := range []struct {
		name          string
		trunk         map[string]string // pushed to the trunk after B's change; nil: trunk unchanged
		mine          map[string]string
		wantExit      int
		wantCode      string
		wantMerged    bool
		wantUpToDate  bool
		wantConflicts []string
	}{
		{
			name:       "clean merge",
			trunk:      map[string]string{"docs/a.md": "a\n"},
			mine:       map[string]string{"docs/b.md": "b\n"},
			wantMerged: true,
		},
		{
			name:       "same file, separate hunks",
			trunk:      map[string]string{"docs/usage.md": "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n"},
			mine:       map[string]string{"docs/usage.md": "one\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n"},
			wantMerged: true,
		},
		{
			name:         "trunk unchanged",
			mine:         map[string]string{"docs/b.md": "b\n"},
			wantUpToDate: true,
		},
		{
			name:          "conflict is left for the agent",
			trunk:         map[string]string{"docs/usage.md": "one\nA\nthree\nfour\nfive\nsix\nseven\n"},
			mine:          map[string]string{"docs/usage.md": "one\nB\nthree\nfour\nfive\nsix\nseven\n"},
			wantExit:      ExitError,
			wantCode:      CodeSyncConflict,
			wantConflicts: []string{"docs/usage.md"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
			s.change("agent-b", s.b, tc.mine, ExitOK)
			before := s.e.git(s.b, "rev-parse", "HEAD")
			trunkHead := s.trunkHead()
			if tc.trunk != nil {
				trunkHead = s.pushToTrunk(tc.trunk)
			}

			t.Setenv("WAYGLASS_AGENT", "agent-b")
			got := s.e.do(s.b, cmpOr(tc.wantExit, ExitOK), "sync")
			data := got
			if tc.wantCode != "" {
				if get(got, "error", "code") != tc.wantCode {
					t.Fatalf("sync = %v", got)
				}
				msg := get(got, "error", "message").(string)
				for _, want := range append(slices.Clone(tc.wantConflicts), "git commit --no-edit", "wayglass push "+s.idB, "wayglass propose "+s.idB) {
					if !strings.Contains(msg, want) {
						t.Fatalf("message %q lacks %q", msg, want)
					}
				}
				data = get(got, "error", "details").(map[string]any)
				if _, err := os.Stat(filepath.Join(s.b, ".git", "MERGE_HEAD")); err != nil {
					t.Fatalf("the conflicted merge was not left in progress: %v", err)
				}
			}
			if get(data, "trunk_head") != trunkHead || get(data, "merged") != tc.wantMerged || get(data, "up_to_date") != tc.wantUpToDate {
				t.Fatalf("sync data = %v (trunk %s)", data, trunkHead)
			}
			var conflicts []string
			for _, c := range get(data, "conflicts").([]any) {
				conflicts = append(conflicts, c.(string))
			}
			if !slices.Equal(conflicts, tc.wantConflicts) {
				t.Fatalf("conflicts = %v, want %v", conflicts, tc.wantConflicts)
			}
			if next := get(data, "next").(string); !strings.Contains(next, "wayglass push "+s.idB) || !strings.Contains(next, "wayglass propose "+s.idB) {
				t.Fatalf("next = %q", next)
			}
			if get(data, "no_token_on_disk") != true {
				t.Fatalf("no_token_on_disk = %v", get(data, "no_token_on_disk"))
			}
			assertNoTokens(t, filepath.Join(s.b, ".git"), s.e.worker.tokens)

			head := s.e.git(s.b, "rev-parse", "HEAD")
			switch {
			case tc.wantMerged:
				parents := strings.Fields(s.e.git(s.b, "log", "-1", "--format=%P", "HEAD"))
				if !slices.Equal(parents, []string{strings.TrimSpace(before), trunkHead}) {
					t.Fatalf("merge parents = %v, want [%s %s] (merged, never rebased)", parents, strings.TrimSpace(before), trunkHead)
				}
				if get(data, "head") != strings.TrimSpace(head) {
					t.Fatalf("head = %v, HEAD is %s", get(data, "head"), head)
				}
			case head != before:
				t.Fatalf("HEAD moved from %s to %s without a merge", before, head)
			}
		})
	}
}

// Sync refuses, before minting a token, a checkout that is not the intent's
// own clone, a dirty one and an intent with nothing to sync.
func TestSyncRefusals(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n"}, ExitOK)
	t.Setenv("WAYGLASS_AGENT", "agent-b")

	other := filepath.Join(t.TempDir(), "trunk-copy")
	s.e.git("", "clone", "--quiet", s.trunk, other)
	if got := s.e.do(other, ExitUsage, "sync"); get(got, "error", "code") != CodeNoIntent {
		t.Fatalf("sync in a trunk clone with no intent = %v", got)
	}
	got := s.e.do(other, ExitUsage, "sync", s.idB)
	if get(got, "error", "code") != CodeNotForkClone || !strings.Contains(get(got, "error", "message").(string), "checkout that declared") {
		t.Fatalf("sync of another checkout's intent = %v", got)
	}

	s.e.write(s.b, "docs/usage.md", "edited, not committed\n")
	if got := s.e.do(s.b, ExitError, "sync"); get(got, "error", "code") != CodeDirty {
		t.Fatalf("sync with uncommitted changes = %v", got)
	}
	s.e.git(s.b, "checkout", "--", "docs/usage.md")

	s.e.do(s.b, ExitOK, "intent", "abandon", s.idB, "--reason", "Superseded by a smaller change")
	if got := s.e.do(s.b, ExitError, "sync", s.idB); get(got, "error", "code") != api.CodeConflict {
		t.Fatalf("sync of an abandoned intent = %v", got)
	}
	if n := s.e.worker.trunkReadTokens; n != 0 {
		t.Fatalf("refused syncs minted %d trunk read tokens", n)
	}
}

// The policy_changed recovery end to end: the trunk's policy moves after B's
// change was approved, land refuses, and sync, push and propose get it
// landed. The merged policy edit is the trunk's, not B's, so the decision
// covers only B's docs change and policy decides it again.
func TestPolicyChangedRecoveryWithSync(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n"}, ExitOK)
	policy, err := os.ReadFile(filepath.Join(s.a, ".wayglass", "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s.pushToTrunk(map[string]string{".wayglass/policy.yaml": string(policy) + "# reviewed Oct 14\n"})

	t.Setenv("WAYGLASS_AGENT", "agent-b")
	got := s.e.do(s.b, ExitError, "land")
	if get(got, "error", "code") != api.CodePolicyChanged || !strings.Contains(get(got, "error", "message").(string), "wayglass sync "+s.idB) {
		t.Fatalf("land under a changed policy = %v", got)
	}

	if got := s.e.do(s.b, ExitOK, "sync"); get(got, "merged") != true {
		t.Fatalf("sync = %v", got)
	}
	s.e.do(s.b, ExitOK, "push")
	prop := s.e.do(s.b, ExitOK, "propose")
	if rule := get(prop, "decision", "matched_rule"); rule != "rules[4]" {
		t.Fatalf("propose after sync matched %v, want the docs rule (the merged policy edit is not B's change)", rule)
	}
	if paths := get(prop, "decision", "paths"); fmt.Sprint(paths) != "[docs/b.md]" {
		t.Fatalf("decision paths after sync = %v, want only B's own file", paths)
	}

	landed := s.e.do(s.b, ExitOK, "land")
	if get(landed, "verified") != true || get(landed, "fast_forward") != true {
		t.Fatalf("land after sync = %v", landed)
	}
	assertNoTokens(t, filepath.Join(s.b, ".git"), s.e.worker.tokens)
}

// After a sync, push and propose count only B's own change: the trunk files
// the merge brought in are not B's, while a resolution edit to a trunk file
// is. The scale run of Oct 14 sent a one-file docs change to a person because
// the ~50 synced docs counted as its change.
func TestSyncedTrunkFilesAreNotTheIntentsChange(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n"}, ExitOK)
	landed := map[string]string{}
	for i := range 45 {
		landed[fmt.Sprintf("docs/landed-%d.md", i)] = "landed\n"
	}
	s.pushToTrunk(landed)

	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.do(s.b, ExitOK, "sync")
	if files := get(s.e.do(s.b, ExitOK, "push"), "files"); fmt.Sprint(files) != "[docs/b.md]" {
		t.Fatalf("push after sync reported %v, want only B's file", files)
	}
	prop := s.e.do(s.b, ExitOK, "propose")
	if n := get(prop, "preview", "tags", "files_changed"); n != float64(1) {
		t.Fatalf("preview files_changed = %v, want 1", n)
	}
	if paths := get(prop, "decision", "paths"); fmt.Sprint(paths) != "[docs/b.md]" {
		t.Fatalf("decision paths = %v, want only B's file", paths)
	}

	// Editing a merged trunk file is B's change.
	s.e.write(s.b, "docs/landed-7.md", "edited by B\n")
	if files := get(s.e.do(s.b, ExitOK, "push", "-m", "docs: fix a landed page"), "files"); fmt.Sprint(files) != "[docs/b.md docs/landed-7.md]" {
		t.Fatalf("push after editing a trunk file reported %v", files)
	}
	prop = s.e.do(s.b, ExitOK, "propose")
	if paths := get(prop, "decision", "paths"); fmt.Sprint(paths) != "[docs/b.md docs/landed-7.md]" {
		t.Fatalf("decision paths = %v, want B's file and the edited trunk file", paths)
	}
	if l := s.e.do(s.b, ExitOK, "land"); get(l, "verified") != true || get(l, "fast_forward") != true {
		t.Fatalf("land = %v", l)
	}
}

// Land refuses a fast-forward that would change a path the decision does not
// cover, the same check as for a merge.
func TestLandFastForwardOutsideDecision(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-b", s.b, map[string]string{"docs/b.md": "b\n", "docs/c.md": "c\n"}, ExitOK)
	// A decision that missed a file (as the old fork-base diff could).
	for _, p := range s.e.worker.projects {
		for _, d := range p.decisions {
			d.Paths = []string{"docs/b.md"}
		}
	}
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	got := s.e.do(s.b, ExitError, "land")
	if get(got, "error", "code") != CodeRepropose || !strings.Contains(get(got, "error", "message").(string), "docs/c.md") {
		t.Fatalf("land = %v", got)
	}
}
