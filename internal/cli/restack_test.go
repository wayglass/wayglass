package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// The overlap re-plan "stack" in one command: B, which edits the same file
// as A, is moved onto A. Its own commits are merged (never rebased) onto A's
// head, pushed to the new fork, and from then on it is classified on its
// own change and lands after A.
func TestIntentRestack(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.write(s.a, "docs/usage.md", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	s.e.do(s.a, ExitOK, "push", "-m", "docs: shout one")
	headA := strings.TrimSpace(s.e.git(s.a, "rev-parse", "HEAD"))

	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.write(s.b, "docs/usage.md", "one\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n")
	pb := s.e.do(s.b, ExitOK, "push", "-m", "docs: shout seven")
	headB := strings.TrimSpace(s.e.git(s.b, "rev-parse", "HEAD"))
	ov := get(pb, "overlaps").([]any)
	if len(ov) != 1 {
		t.Fatalf("overlaps = %v", ov)
	}
	want := RestackCommandFor(s.idB, s.idA)
	if get(ov[0].(map[string]any), "replan", "action") != "stack" || get(ov[0].(map[string]any), "replan", "command") != want {
		t.Fatalf("push --json re-plan = %v, want stack naming %q", ov[0], want)
	}
	ol := s.e.do(s.b, ExitOK, "overlaps")
	if pairs := get(ol, "overlaps").([]any); len(pairs) != 1 || get(pairs[0].(map[string]any), "replan", "command") != want {
		t.Fatalf("overlaps --json = %v", ol)
	}
	s.e.do(s.b, ExitOK, "propose") // a decision the restack must supersede

	reason := "Both edit docs/usage.md; build on A instead of racing it"
	r := s.e.do(s.b, ExitOK, "intent", "restack", s.idB, "--on", s.idA, "--reason", reason)
	commit := get(r, "commit").(string)
	if get(r, "intent", "parent_id") != s.idA || get(r, "base") != headA || get(r, "carried") != headB ||
		get(r, "merged") != true || get(r, "pushed") != true || get(r, "must_propose") != true ||
		get(r, "superseded_decision") == nil || get(r, "local_fast_forwarded") != true || get(r, "fetched_head") != false {
		t.Fatalf("restack = %v", r)
	}
	if files := get(r, "files").([]any); len(files) != 1 || files[0] != "docs/usage.md" {
		t.Fatalf("restack files = %v", files)
	}
	if len(get(r, "overlaps").([]any)) != 0 {
		t.Fatalf("a stack is not an overlap: %v", get(r, "overlaps"))
	}
	// Merged, never rebased: B's commit is kept as the first parent.
	if parents := strings.Fields(s.e.git(s.b, "log", "-1", "--format=%P", commit)); !slices.Equal(parents, []string{headB, headA}) {
		t.Fatalf("merge parents = %v", parents)
	}
	if got := strings.TrimSpace(s.e.git(s.b, "rev-parse", "HEAD")); got != commit {
		t.Fatalf("B's checkout HEAD = %s, want %s", got, commit)
	}
	if !strings.Contains(s.e.git(s.b, "log", "-1", "--format=%B", commit), "Why: "+reason) {
		t.Fatal("the merge commit does not carry the why")
	}
	newFork := get(r, "intent", "fork_remote").(string)
	if remote := strings.Fields(s.e.git(s.b, "ls-remote", newFork, "refs/heads/main"))[0]; remote != commit {
		t.Fatalf("new fork head = %s, want %s", remote, commit)
	}
	if get(r, "previous", "fork_kept") != true || get(r, "previous", "fork_repo") == get(r, "intent", "fork_repo") {
		t.Fatalf("previous = %v", get(r, "previous"))
	}

	// Next push and propose cover only B's own change against A's head.
	s.e.write(s.b, "docs/b.md", "b\n")
	pb2 := s.e.do(s.b, ExitOK, "push", "-m", "docs: b")
	if get(pb2, "base") != headA {
		t.Fatalf("push base = %v, want A's head %s", get(pb2, "base"), headA)
	}
	pr := s.e.do(s.b, ExitOK, "propose")
	paths := get(pr, "decision", "paths").([]any)
	if !slices.Equal(paths, []any{"docs/b.md", "docs/usage.md"}) {
		t.Fatalf("decision paths = %v", paths)
	}
	if got := s.e.do(s.b, ExitError, "land"); !strings.Contains(get(got, "error", "message").(string), "land "+s.idA+" first") {
		t.Fatalf("land before A = %v", got)
	}
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.do(s.a, ExitOK, "propose")
	s.e.do(s.a, ExitOK, "land")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	if l := s.e.do(s.b, ExitOK, "land"); get(l, "verified") != true || get(l, "fast_forward") != true {
		t.Fatalf("land B after A = %v", l)
	}
	if got := s.e.git(s.b, "show", "main:docs/usage.md"); strings.TrimSpace(got) != "ONE\ntwo\nthree\nfour\nfive\nsix\nSEVEN" {
		t.Fatalf("trunk docs/usage.md = %q", got)
	}

	if got := s.e.do(s.b, ExitError, "intent", "restack", s.idB, "--on", s.idA, "--reason", reason); get(got, "error", "code") != "conflict" {
		t.Fatalf("restack of a landed intent = %v", got)
	}
}

// RestackCommandFor is the command the re-plan names (kept here so the test
// states the contract, not the helper).
func RestackCommandFor(then, first string) string {
	return `wayglass intent restack ` + then + ` --on ` + first + ` --reason "<why ` + then + ` builds on ` + first + `>"`
}

// A conflict is found before the Worker is asked: nothing changes.
func TestIntentRestackConflictChangesNothing(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.write(s.a, "docs/usage.md", "one\nA\nthree\nfour\nfive\nsix\nseven\n")
	s.e.do(s.a, ExitOK, "push", "-m", "docs: A")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.write(s.b, "docs/usage.md", "one\nB\nthree\nfour\nfive\nsix\nseven\n")
	s.e.do(s.b, ExitOK, "push", "-m", "docs: B")
	before := strings.TrimSpace(s.e.git(s.b, "rev-parse", "HEAD"))
	got := s.e.do(s.b, ExitError, "intent", "restack", s.idB, "--on", s.idA, "--reason", "both edit line two")
	if get(got, "error", "code") != CodeRestackConflict || !strings.Contains(get(got, "error", "message").(string), "nothing changed") {
		t.Fatalf("restack = %v", got)
	}
	if s.e.worker.restacks != 0 {
		t.Fatal("the Worker was asked to restack a conflicting change")
	}
	if after := strings.TrimSpace(s.e.git(s.b, "rev-parse", "HEAD")); after != before {
		t.Fatal("a refused restack moved the checkout")
	}
	if wt := s.e.git(s.b, "worktree", "list"); len(strings.Split(strings.TrimSpace(wt), "\n")) != 1 {
		t.Fatalf("temporary worktree left behind:\n%s", wt)
	}
}

// From a checkout that is not the intent's (here A's, as B's author would
// from a fresh clone), restack fetches the intent's pushed commits from its
// fork with a read token.
func TestIntentRestackFromAnotherCheckout(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/a.md"}, []string{"docs/b.md"})
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.write(s.a, "docs/a.md", "a\n")
	s.e.do(s.a, ExitOK, "push", "-m", "docs: a")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.write(s.b, "docs/b.md", "b\n")
	s.e.do(s.b, ExitOK, "push", "-m", "docs: b")
	headB := strings.TrimSpace(s.e.git(s.b, "rev-parse", "HEAD"))
	headA := strings.TrimSpace(s.e.git(s.a, "rev-parse", "HEAD"))

	t.Chdir(s.a)
	exit, out, _ := run(t, "intent", "restack", s.idB, "--on", s.idA, "--reason", "B documents what A adds")
	if exit != ExitOK || !strings.Contains(out, "Restacked "+s.idB+" on "+s.idA) || !strings.Contains(out, "propose") {
		t.Fatalf("human restack (exit %d):\n%s", exit, out)
	}
	r := s.e.do(s.a, ExitOK, "intent", "show", s.idB)
	if get(r, "intent", "parent_id") != s.idA || get(r, "intent", "base_commit") != headA {
		t.Fatalf("intent after restack from A's checkout = %v", r)
	}
	r = get(r, "intent").(map[string]any)
	r["commit"] = strings.Fields(s.e.git(s.a, "ls-remote", r["fork_remote"].(string), "refs/heads/main"))[0]
	if parents := strings.Fields(s.e.git(s.a, "log", "-1", "--format=%P", r["commit"].(string))); !slices.Equal(parents, []string{headB, headA}) {
		t.Fatalf("merge parents = %v (B's commits fetched from its fork?)", parents)
	}
	if got := strings.TrimSpace(s.e.git(s.a, "rev-parse", "HEAD")); got != headA {
		t.Fatal("restack moved a checkout that is not the intent's")
	}
	// B's own checkout catches up with a plain fast-forward from the new fork.
	s.e.git(s.b, "pull", "--quiet", "--ff-only", r["fork_remote"].(string), "main")
	if got := strings.TrimSpace(s.e.git(s.b, "rev-parse", "HEAD")); got != get(r, "commit") {
		t.Fatalf("B after pull = %s", got)
	}
}

func TestIntentRestackRefusals(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	// A has pushed nothing: there is nothing to stack on.
	if got := s.e.do(s.b, ExitError, "intent", "restack", s.idB, "--on", s.idA, "--reason", "B builds on A"); !strings.Contains(get(got, "error", "message").(string), "nothing pushed") {
		t.Fatalf("restack on an empty intent = %v", got)
	}
	// Someone else's intent.
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.write(s.a, "docs/a.md", "a\n")
	s.e.do(s.a, ExitOK, "push", "-m", "docs: a")
	if got := s.e.do(s.a, ExitError, "intent", "restack", s.idB, "--on", s.idA, "--reason", "B builds on A"); get(got, "error", "code") != "forbidden" {
		t.Fatalf("restack by another agent = %v", got)
	}
	// A Worker without the route says what to do instead.
	s.e.worker.noRestack = true
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	if got := s.e.do(s.b, ExitError, "intent", "restack", s.idB, "--on", s.idA, "--reason", "B builds on A"); get(got, "error", "code") != CodeStackUnsupported {
		t.Fatalf("restack on an old Worker = %v", got)
	}
}

// A decision still pending when the intent is re-stacked is superseded:
// nobody can approve it and it leaves the review queue until the author
// proposes again.
func TestIntentRestackSupersedesPendingDecision(t *testing.T) {
	s := newTwoAgents(t, []string{"web/**"}, []string{"web/**"})
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.write(s.a, "web/login.ts", "export function login() { /* A */ }\n")
	s.e.do(s.a, ExitOK, "push", "-m", "feat: login A")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.write(s.b, "web/login-b.ts", "export const b = 1\n")
	s.e.do(s.b, ExitOK, "push", "-m", "feat: login B")
	if p := s.e.do(s.b, ExitNeedsHuman, "propose"); get(p, "status") != "pending_human" {
		t.Fatalf("propose = %v", p)
	}
	if q := get(s.e.do(s.b, ExitOK, "review"), "queue").([]any); len(q) != 1 {
		t.Fatalf("queue before restack = %v", q)
	}

	r := s.e.do(s.b, ExitOK, "intent", "restack", s.idB, "--on", s.idA, "--reason", "B's login builds on A's login module")
	if get(r, "superseded_decision") == nil || get(r, "must_propose") != true {
		t.Fatalf("restack = %v", r)
	}
	if q := get(s.e.do(s.b, ExitOK, "review"), "queue").([]any); len(q) != 0 {
		t.Fatalf("queue after restack = %v", q)
	}
	t.Setenv("WAYGLASS_AGENT", "reviewer")
	got := s.e.do(s.a, ExitError, "approve", s.idB, "--reason", "checked the login flow")
	if get(got, "error", "code") != api.CodeSuperseded || !strings.Contains(get(got, "error", "message").(string), "wayglass propose") {
		t.Fatalf("approve superseded = %v", got)
	}

	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.do(s.b, ExitNeedsHuman, "propose")
	t.Setenv("WAYGLASS_AGENT", "reviewer")
	if d := s.e.do(s.a, ExitOK, "approve", s.idB, "--reason", "checked the login flow"); get(d, "decision", "status") != "approved" {
		t.Fatalf("approve after re-propose = %v", d)
	}
}

// An approval given before a restack is superseded too: it covered another
// base. The decision keeps who approved, the log shows both the approval and
// that it no longer counts, and land refuses until the author proposes again.
func TestIntentRestackSupersedesApprovedDecision(t *testing.T) {
	s := newTwoAgents(t, []string{"web/**"}, []string{"web/**"})
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.write(s.a, "web/login.ts", "export function login() { /* A */ }\n")
	s.e.do(s.a, ExitOK, "push", "-m", "feat: login A")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.e.write(s.b, "web/login-b.ts", "export const b = 1\n")
	s.e.do(s.b, ExitOK, "push", "-m", "feat: login B")
	s.e.do(s.b, ExitNeedsHuman, "propose")
	t.Setenv("WAYGLASS_AGENT", "reviewer")
	if d := s.e.do(s.a, ExitOK, "approve", s.idB, "--reason", "checked the login flow"); get(d, "decision", "status") != "approved" {
		t.Fatalf("approve = %v", d)
	}

	t.Setenv("WAYGLASS_AGENT", "agent-b")
	r := s.e.do(s.b, ExitOK, "intent", "restack", s.idB, "--on", s.idA, "--reason", "B's login builds on A's login module")
	if get(r, "superseded_decision") == nil || get(r, "must_propose") != true {
		t.Fatalf("restack = %v", r)
	}
	land := s.e.do(s.b, ExitError, "land")
	if msg, _ := get(land, "error", "message").(string); !strings.Contains(msg, "superseded") || !strings.Contains(msg, "propose") {
		t.Fatalf("land superseded = %v", land)
	}
	t.Setenv("WAYGLASS_AGENT", "reviewer")
	if got := s.e.do(s.a, ExitError, "approve", s.idB, "--reason", "checked the login flow"); get(got, "error", "code") != api.CodeSuperseded {
		t.Fatalf("approve superseded = %v", got)
	}

	var approve, superseded bool
	s.e.worker.mu.Lock()
	events := slices.Clone(s.e.worker.projects["demo"].audit)
	s.e.worker.mu.Unlock()
	for _, e := range events {
		// The admin key approved as "reviewer": recorded as admin, name kept as claimed_by.
		approve = approve || e.Kind == "decision.approve" && e.Actor == "admin" && strings.Contains(string(e.Data), `"claimed_by":"reviewer"`)
		if e.Kind == "decision.superseded" {
			row := logRow(e)
			superseded = strings.HasPrefix(row.Detail, "superseded approval by admin: restacked onto "+s.idA)
		}
	}
	if !approve || !superseded {
		t.Fatalf("audit keeps the approval (%v) and says it was superseded (%v)", approve, superseded)
	}
}
