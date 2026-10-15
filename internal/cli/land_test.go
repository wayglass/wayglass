package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/policy"
)

// twoAgents is a project with agent A in e.repo and agent B in its own clone,
// both declared on the same trunk commit.
type twoAgents struct {
	e        *flowEnv
	a, b     string
	trunk    string
	idA, idB string
}

func newTwoAgents(t *testing.T, pathsA, pathsB []string) *twoAgents {
	t.Helper()
	e := newFlow(t)
	e.write(e.repo, "docs/usage.md", "one\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "docs: usage")
	s := &twoAgents{e: e, a: e.repo}
	s.trunk = get(e.do(s.a, ExitOK, "init", "--project", "demo"), "trunk", "remote").(string)
	s.b = filepath.Join(t.TempDir(), "agent-b")
	e.git("", "clone", "--quiet", s.trunk, s.b)
	declare := func(dir, goal string, paths []string) string {
		args := []string{"intent", "declare", "--goal", goal, "--reason", "needed"}
		for _, p := range paths {
			args = append(args, "--path", p)
		}
		return get(e.do(dir, ExitOK, args...), "intent", "id").(string)
	}
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.idA = declare(s.a, "A", pathsA)
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	s.idB = declare(s.b, "B", pathsB)
	return s
}

// change commits files in dir as the given agent, pushes and proposes.
func (s *twoAgents) change(agent, dir string, files map[string]string, wantPropose int) {
	s.e.t.Helper()
	s.e.t.Setenv("WAYGLASS_AGENT", agent)
	for name, body := range files {
		s.e.write(dir, name, body)
	}
	s.e.do(dir, ExitOK, "push", "-m", "docs: change by "+agent)
	s.e.do(dir, wantPropose, "propose")
}

func (s *twoAgents) trunkHead() string {
	return strings.Fields(s.e.git(s.a, "ls-remote", s.trunk, "refs/heads/main"))[0]
}

// Lands on a trunk that moved after the intent forked: A lands first, then B
// lands onto A's change. B's approved commit is merged, never rewritten.
func TestLandOnMovedTrunk(t *testing.T) {
	for _, tc := range []struct {
		name       string
		a, b       map[string]string
		wantExit   int
		wantCode   string
		wantMerged []string
	}{
		{
			name: "different files merge",
			a:    map[string]string{"docs/a.md": "a\n"},
			b:    map[string]string{"docs/b.md": "b\n"},
		},
		{
			name:       "same file, separate hunks merge textually",
			a:          map[string]string{"docs/usage.md": "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n"},
			b:          map[string]string{"docs/usage.md": "one\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n"},
			wantMerged: []string{"docs/usage.md"},
		},
		{
			name:     "same lines conflict",
			a:        map[string]string{"docs/usage.md": "one\nA\nthree\nfour\nfive\nsix\nseven\n"},
			b:        map[string]string{"docs/usage.md": "one\nB\nthree\nfour\nfive\nsix\nseven\n"},
			wantExit: ExitError,
			wantCode: CodeLandConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
			s.change("agent-a", s.a, tc.a, ExitOK)
			s.change("agent-b", s.b, tc.b, ExitOK)
			t.Setenv("WAYGLASS_AGENT", "agent-a")
			s.e.do(s.a, ExitOK, "land")
			before := s.trunkHead()

			t.Setenv("WAYGLASS_AGENT", "agent-b")
			approved := s.e.git(s.b, "rev-parse", "HEAD")
			got := s.e.do(s.b, cmpOr(tc.wantExit, ExitOK), "land")
			if tc.wantCode != "" {
				if get(got, "error", "code") != tc.wantCode {
					t.Fatalf("land = %v", got)
				}
				if msg := get(got, "error", "message").(string); !strings.Contains(msg, "docs/usage.md") || !strings.Contains(msg, "nothing was pushed") || !strings.Contains(msg, "wayglass sync "+s.idB) {
					t.Fatalf("conflict message = %q", msg)
				}
				if s.trunkHead() != before {
					t.Fatal("a failed land moved the trunk")
				}
				if s.e.worker.aborts != 1 {
					t.Fatalf("aborts = %d, want the land given back once", s.e.worker.aborts)
				}
				// The slot is free at once: B can be fixed and landed without waiting 300 s.
				// Resolving in favour of B keeps its line, so the change still adds something.
				s.e.git(s.b, "pull", "--quiet", "--no-rebase", "-X", "ours", s.trunk, "main")
				s.e.do(s.b, ExitOK, "push")
				s.e.do(s.b, ExitOK, "propose")
				if l := s.e.do(s.b, ExitOK, "land"); get(l, "verified") != true {
					t.Fatalf("land after fixing the conflict = %v", l)
				}
				return
			}
			merged := get(got, "commit").(string)
			if get(got, "trunk_moved") != true || get(got, "fast_forward") != false || get(got, "verified") != true || get(got, "approved") != approved {
				t.Fatalf("land = %v", got)
			}
			if parents := strings.Fields(s.e.git(s.b, "log", "-1", "--format=%P", merged)); !slices.Equal(parents, []string{before, approved}) {
				t.Fatalf("merge parents = %v, want [%s %s]", parents, before, approved)
			}
			var mp []string
			for _, p := range get(got, "merged_paths").([]any) {
				mp = append(mp, p.(string))
			}
			if !slices.Equal(mp, tc.wantMerged) {
				t.Fatalf("merged_paths = %v, want %v", mp, tc.wantMerged)
			}
			// B's checkout was on the approved commit, which the merge contains.
			if get(got, "local_fast_forwarded") != true || get(got, "local_up_to_date") != false {
				t.Fatalf("local = %v / %v", get(got, "local_fast_forwarded"), get(got, "local_up_to_date"))
			}
		})
	}
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// A trunk rename moves the approved edit into a file the decision never
// covered: land stops before pushing and asks for a new proposal.
func TestLandRefusesPathsOutsideTheDecision(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.e.t.Setenv("WAYGLASS_AGENT", "agent-a")
	s.e.git(s.a, "mv", "docs/usage.md", "docs/guide.md")
	s.e.do(s.a, ExitOK, "push", "-m", "docs: rename usage to guide")
	s.e.do(s.a, ExitOK, "propose")
	s.e.do(s.a, ExitOK, "land")
	before := s.trunkHead()

	s.change("agent-b", s.b, map[string]string{"docs/usage.md": "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n"}, ExitOK)
	got := s.e.do(s.b, ExitError, "land")
	if get(got, "error", "code") != CodeRepropose || !strings.Contains(get(got, "error", "message").(string), "docs/guide.md") {
		t.Fatalf("land = %v", got)
	}
	if s.trunkHead() != before || s.e.worker.aborts != 1 {
		t.Fatalf("trunk moved or land not given back (aborts %d)", s.e.worker.aborts)
	}
}

// A Worker without /land/abort still fails the land cleanly and says the
// token runs out on its own.
func TestLandAbortUnsupportedWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail(w, 404, "not_found", "no route for "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	res := &landResult{}
	s := &session{c: api.New(srv.URL, "k"), w: &workspace{}, intent: &api.Intent{ID: "i0000000001"}}
	s.abortLand(t.Context(), res, errorf(CodeLandConflict, ExitError, "conflict"))
	if res.Aborted || len(res.Warns) != 1 || !strings.Contains(res.Warns[0], "cannot abort a land") {
		t.Fatalf("res = %+v", res)
	}
}

// Stacked intents: B builds on A's unlanded change, is classified on its own
// diff only, and lands only after A.
func TestStackedIntents(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo")
	idA := get(e.do(a, ExitOK, "intent", "declare", "--goal", "Guide", "--reason", "Users are lost", "--path", "docs/**"), "intent", "id").(string)
	if got := e.do(a, ExitError, "intent", "declare", "--goal", "too early", "--reason", "r", "--path", "web/**", "--on", idA); get(got, "error", "code") != "conflict" {
		t.Fatalf("stacking on an intent with nothing pushed = %v", got)
	}
	e.write(a, "docs/guide.md", "# Guide\n")
	e.do(a, ExitOK, "push", "-m", "docs: guide")
	e.do(a, ExitOK, "propose") // approved by policy, not landed yet
	headA := e.git(a, "rev-parse", "HEAD")

	db := e.do(a, ExitOK, "intent", "declare", "--goal", "Login limits", "--reason", "Brute force", "--path", "web/login.ts", "--on", idA)
	idB := get(db, "intent", "id").(string)
	if get(db, "intent", "parent_id") != idA || get(db, "intent", "base_commit") != headA || len(get(db, "warnings").([]any)) != 0 {
		t.Fatalf("declare --on = %v", db)
	}
	if len(get(db, "overlaps").([]any)) != 0 {
		t.Fatalf("a stack is not an overlap with its parent: %v", get(db, "overlaps"))
	}
	e.write(a, "web/login.ts", "export const maxAttempts = 5;\n")
	pb := e.do(a, ExitOK, "push", "-m", "feat: limit login attempts")
	if files := get(pb, "files").([]any); len(files) != 1 || files[0] != "web/login.ts" {
		t.Fatalf("stacked push files = %v, want only its own change", files)
	}
	prB := e.do(a, ExitNeedsHuman, "propose")
	if get(prB, "preview", "tags", "files_changed") != float64(1) || slices.Contains(get(prB, "decision", "paths").([]any), any("docs/guide.md")) {
		t.Fatalf("stacked propose classified the parent's change too: %v", prB)
	}
	t.Setenv("WAYGLASS_AGENT", "john")
	e.do(a, ExitOK, "approve", idB, "--reason", "Lockout window is fine")
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	if got := e.do(a, ExitError, "land", idB); !strings.Contains(get(got, "error", "message").(string), "land "+idA+" first") {
		t.Fatalf("land before parent = %v", got)
	}
	if got := e.do(a, ExitOK, "land", idA); get(got, "commit") != headA || get(got, "fast_forward") != true {
		t.Fatalf("land parent = %v", got)
	}
	lb := e.do(a, ExitOK, "land", idB)
	if get(lb, "verified") != true || get(lb, "fast_forward") != true || get(lb, "local_up_to_date") != true {
		t.Fatalf("land child = %v", lb)
	}
	// The approver's reason travels in the note: why answers it from git.
	w := e.do(a, ExitOK, "why", "web/login.ts:1")
	aps := get(w, "note", "approvals").([]any)
	if get(w, "source") != "note" || len(aps) != 1 || get(aps[0].(map[string]any), "reason") != "Lockout window is fine" {
		t.Fatalf("why = %v", w)
	}
	t.Chdir(a)
	// The admin key approved as "john": the record counts it as admin and keeps the name.
	if _, out, _ := run(t, "why", "web/login.ts:1"); !strings.Contains(out, "decided by admin (john) at") || !strings.Contains(out, "Approved: admin (john): Lockout window is fine") {
		t.Fatalf("human why = %q", out)
	}
}

// A Worker that predates stacking ignores --on; declare must not pretend.
func TestStackOnOldWorkerFails(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	idA := get(e.do(e.repo, ExitOK, "intent", "declare", "--goal", "g", "--reason", "r", "--path", "docs/**"), "intent", "id").(string)
	e.worker.noStack = true
	got := e.do(e.repo, ExitError, "intent", "declare", "--goal", "g2", "--reason", "r", "--path", "web/**", "--on", idA)
	if get(got, "error", "code") != CodeStackUnsupported {
		t.Fatalf("declare --on on an old Worker = %v", got)
	}
}

func TestProposeNextStep(t *testing.T) {
	for _, tc := range []struct {
		status string
		needed int
		dry    bool
		want   string
	}{
		{api.StatusApproved, 1, false, "allows this change to land"},
		{api.StatusPendingAgent, 1, false, "reviewer agent"},
		{api.StatusPendingHuman, 1, false, "a person to approve"},
		{api.StatusPendingHuman, 2, false, "2 people"},
		{api.StatusQuarantined, 1, false, "Quarantined"},
		{api.StatusBlocked, 1, false, "Blocked by policy"},
		{api.StatusRejected, 1, false, "Rejected"},
		{"", 1, true, "would choose human_approve"},
	} {
		r := proposeResult{Status: tc.status, Preview: policy.Decision{Action: policy.HumanApprove}}
		if !tc.dry {
			r.Decision = &api.Decision{Status: tc.status, ApprovalsNeeded: tc.needed}
		}
		got := nextStep(r, tc.dry)
		if !strings.Contains(got, tc.want) {
			t.Errorf("nextStep(%s, %d, dry=%v) = %q, want %q", tc.status, tc.needed, tc.dry, got, tc.want)
		}
		if tc.status != api.StatusApproved && strings.Contains(got, "allows") {
			t.Errorf("nextStep(%s) claims the change may land: %q", tc.status, got)
		}
	}
}

func TestLogCommand(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	id := get(e.do(e.repo, ExitOK, "intent", "declare", "--goal", "Guide", "--reason", "Users are lost", "--path", "docs/**"), "intent", "id").(string)
	e.write(e.repo, "web/login.ts", "changed\n")
	e.do(e.repo, ExitOK, "push", "-m", "feat: login")
	e.do(e.repo, ExitNeedsHuman, "propose")

	all := e.do(e.repo, ExitOK, "log")
	kinds := []string{}
	for _, ev := range get(all, "events").([]any) {
		kinds = append(kinds, ev.(map[string]any)["kind"].(string))
	}
	if !slices.Equal(kinds, []string{"project.created", "intent.declared", "intent.pushed", "decision.proposed"}) {
		t.Fatalf("log kinds = %v", kinds)
	}
	if get(all, "next") != float64(4) || get(all, "truncated") != false {
		t.Fatalf("log = %v", all)
	}
	if got := e.do(e.repo, ExitOK, "log", "--limit", "1"); len(get(got, "events").([]any)) != 1 ||
		get(get(got, "events").([]any)[0].(map[string]any), "kind") != "decision.proposed" {
		t.Fatalf("log --limit 1 = %v (want the most recent)", got)
	}
	if got := e.do(e.repo, ExitOK, "log", "--kind", "decision", "--intent", id); len(get(got, "events").([]any)) != 1 {
		t.Fatalf("log --kind decision = %v", got)
	}
	if got := e.do(e.repo, ExitOK, "log", "--since", "3"); len(get(got, "events").([]any)) != 1 {
		t.Fatalf("log --since 3 = %v", got)
	}
	if got := e.do(e.repo, ExitUsage, "log", "--limit", "0"); get(got, "error", "code") != CodeUsage {
		t.Fatalf("log --limit 0 = %v", got)
	}

	t.Chdir(e.repo)
	exit, out, stderr := run(t, "log")
	if exit != ExitOK || stderr != "" {
		t.Fatalf("exit %d stderr %q", exit, stderr)
	}
	for _, want := range []string{"EVENT", "decision.proposed", "▲ human_approve (pending_human)", "Guide", id} {
		if !strings.Contains(out, want) {
			t.Errorf("human log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-TTY log must be plain:\n%s", out)
	}
}

// An agent whose clone predates a land, or that stacks on another agent's
// unlanded intent, starts from the fork, not from its stale HEAD.
func TestDeclareFastForwardsToForkStart(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo")
	root := filepath.Dir(a)
	b, c, d := filepath.Join(root, "b"), filepath.Join(root, "c"), filepath.Join(root, "d")
	for _, dir := range []string{b, c, d} {
		e.git("", "clone", "--quiet", a, dir)
	}

	// Agent A lands a docs change; the trunk moves past b and c.
	idA := get(e.do(a, ExitOK, "intent", "declare", "--goal", "Guide", "--reason", "Users are lost", "--path", "docs/**"), "intent", "id").(string)
	e.write(a, "docs/guide.md", "# Guide\n")
	e.do(a, ExitOK, "push", "-m", "docs: guide")
	e.do(a, ExitOK, "propose")
	trunk := get(e.do(a, ExitOK, "land", idA), "commit").(string)

	db := e.do(b, ExitOK, "intent", "declare", "--goal", "Login", "--reason", "Brute force", "--path", "web/**")
	if get(db, "fast_forwarded") != trunk || len(get(db, "warnings").([]any)) != 0 {
		t.Fatalf("declare from a stale clone = %v, want fast_forwarded %s", db, trunk)
	}
	e.write(b, "web/login.ts", "export const maxAttempts = 5;\n")
	if files := get(e.do(b, ExitOK, "push", "-m", "feat: limit login attempts"), "files").([]any); len(files) != 1 || files[0] != "web/login.ts" {
		t.Fatalf("push after fast-forward: files = %v", files)
	}

	// Stacking from a third clone: the parent's commits come with the fork.
	idB := get(db, "intent", "id").(string)
	headB := e.git(b, "rev-parse", "HEAD")
	dc := e.do(c, ExitOK, "intent", "declare", "--goal", "Lockout copy", "--reason", "Say why", "--path", "web/lockout.ts", "--on", idB)
	if get(dc, "fast_forwarded") != headB {
		t.Fatalf("declare --on from another clone = %v, want fast_forwarded %s", dc, headB)
	}
	e.write(c, "web/lockout.ts", "export const lockoutMinutes = 15;\n")
	if files := get(e.do(c, ExitOK, "push", "-m", "feat: lockout window"), "files").([]any); len(files) != 1 || files[0] != "web/lockout.ts" {
		t.Fatalf("stacked push files = %v, want only its own change", files)
	}

	// A stale checkout with uncommitted work is never moved, only warned.
	e.write(d, "docs/more.md", "# More\n")
	dd := e.do(d, ExitOK, "intent", "declare", "--goal", "More", "--reason", "More docs", "--path", "docs/**")
	if get(dd, "fast_forwarded") != nil || len(get(dd, "warnings").([]any)) != 1 || e.git(d, "rev-parse", "HEAD") == trunk {
		t.Fatalf("dirty stale checkout: %v", dd)
	}
}

// A reviewer in its own clone sees exactly the change an agent pushed.
func TestDiffForReviewer(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo")
	rev := filepath.Join(filepath.Dir(a), "reviewer")
	e.git("", "clone", "--quiet", a, rev)

	id := get(e.do(a, ExitOK, "intent", "declare", "--goal", "Guide", "--reason", "Users are lost", "--path", "docs/**"), "intent", "id").(string)
	if got := e.do(rev, ExitError, "diff", id); get(got, "error", "code") != CodeNotPushed {
		t.Fatalf("diff before a push = %v", got)
	}
	e.write(a, "docs/guide.md", "# Guide\n\nStart here.\n")
	e.do(a, ExitOK, "push", "-m", "docs: guide")

	t.Setenv("WAYGLASS_AGENT", "reviewer")
	d := e.do(rev, ExitOK, "diff", id)
	files := get(d, "files").([]any)
	if u, _ := get(d, "untrusted").([]any); !slices.Contains(u, any("diff")) || !slices.Contains(u, any("goal")) {
		t.Fatalf("diff must mark the author's fields untrusted: %v", get(d, "untrusted"))
	}
	if len(files) != 1 || files[0] != "docs/guide.md" || !strings.Contains(get(d, "diff").(string), "+Start here.") {
		t.Fatalf("diff = %v", d)
	}
	if get(d, "goal") != "Guide" || get(d, "reason") != "Users are lost" || get(d, "head") != e.git(a, "rev-parse", "HEAD") {
		t.Fatalf("diff context = %v", d)
	}
	if got := e.git(rev, "rev-parse", reviewRef(id)); got != get(d, "head") {
		t.Fatal("diff did not keep the fetched head")
	}
}

// A transient Artifacts fork failure is retried; nothing was created by the
// failed attempt, so the retry declares exactly one intent.
func TestDeclareRetriesTransientForkFailure(t *testing.T) {
	old := declareBackoff
	declareBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { declareBackoff = old })
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")

	e.worker.forkFailures = 1
	d := e.do(e.repo, ExitOK, "intent", "declare", "--goal", "g", "--reason", "r", "--path", "docs/**")
	if w := get(d, "warnings").([]any); len(w) != 1 || !strings.Contains(w[0].(string), "retried") {
		t.Fatalf("warnings = %v", w)
	}
	if n := len(get(e.do(e.repo, ExitOK, "intent", "list"), "intents").([]any)); n != 1 {
		t.Fatalf("%d intents after one retried declare, want 1", n)
	}

	e.worker.forkFailures = 3
	if got := e.do(e.repo, ExitError, "intent", "declare", "--goal", "g", "--reason", "r", "--path", "docs/**"); get(got, "error", "code") != api.CodeUpstream {
		t.Fatalf("declare after retries ran out = %v", got)
	}
}

// The author's clone is gone: a person approves, and land runs from a fresh
// clone of the trunk. Land fetches the approved commit from the fork with a
// read token and lands it only if the fork head is the decided commit.
func TestLandFromFreshTrunkClone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		trunkMoved  bool // another change lands first, so this one is merged
		moveFork    bool // the fork moves after the land token was issued
		noReadToken bool
		wantExit    int
		wantCode    string
	}{
		{name: "fast-forward"},
		{name: "merge onto a moved trunk", trunkMoved: true},
		{name: "fork moved under the land", moveFork: true, wantExit: ExitError, wantCode: CodeForkMoved},
		{name: "fork unreadable", noReadToken: true, wantExit: ExitError, wantCode: CodeMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwoAgents(t, []string{"docs/**"}, []string{"web/login.ts"})
			if tc.trunkMoved {
				s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
				s.e.do(s.a, ExitOK, "land")
			}
			s.change("agent-b", s.b, map[string]string{"web/login.ts": "export const maxAttempts = 5;\n"}, ExitNeedsHuman)
			approved := s.e.git(s.b, "rev-parse", "HEAD")
			t.Setenv("WAYGLASS_AGENT", "john-tester")
			s.e.do(s.a, ExitOK, "approve", s.idB, "--reason", "Five tries matches the lockout policy")
			fork := s.e.worker.projects["demo"].intent(s.idB).ForkRemote
			if err := os.RemoveAll(s.b); err != nil { // the agent's clone is gone
				t.Fatal(err)
			}

			fresh := filepath.Join(t.TempDir(), "fresh")
			s.e.git("", "clone", "--quiet", s.trunk, fresh)
			s.e.worker.noReadToken = tc.noReadToken
			if tc.moveFork {
				other := filepath.Join(t.TempDir(), "other")
				s.e.worker.onReadToken = func(*api.Intent) {
					s.e.git("", "clone", "--quiet", fork, other)
					s.e.write(other, "web/login.ts", "export const maxAttempts = 50;\n")
					s.e.git(other, "commit", "--quiet", "-am", "sneak")
					s.e.git(other, "push", "--quiet", "origin", "HEAD:main")
				}
			}
			before := s.trunkHead()
			got := s.e.do(fresh, cmpOr(tc.wantExit, ExitOK), "land", s.idB)
			if tc.wantCode != "" {
				if get(got, "error", "code") != tc.wantCode {
					t.Fatalf("land = %v", got)
				}
				if s.trunkHead() != before || s.e.worker.aborts != 1 {
					t.Fatalf("trunk moved or land not given back (aborts %d)", s.e.worker.aborts)
				}
				return
			}
			if get(got, "fetched_from_fork") != true || get(got, "approved") != approved || get(got, "verified") != true ||
				get(got, "fast_forward") != !tc.trunkMoved || get(got, "local_fast_forwarded") != true {
				t.Fatalf("land = %v", got)
			}
			if s.trunkHead() != get(got, "commit") || s.e.worker.readTokens != 1 {
				t.Fatalf("trunk %s, land %v, read tokens %d", s.trunkHead(), get(got, "commit"), s.e.worker.readTokens)
			}
			w := s.e.do(fresh, ExitOK, "why", "web/login.ts:1")
			if aps := get(w, "note", "approvals").([]any); len(aps) != 1 || get(aps[0].(map[string]any), "reason") != "Five tries matches the lockout policy" {
				t.Fatalf("why = %v", w)
			}
		})
	}
}

// Only the intent's author or the admin key may land an approved change.
// Another agent's key is refused before any trunk token is issued; the
// author's own key lands from a fresh clone (the admin key's case is
// TestLandFromFreshTrunkClone).
func TestLandOnlyByAuthorOrAdmin(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"web/login.ts"})
	s.change("agent-b", s.b, map[string]string{"web/login.ts": "export const maxAttempts = 5;\n"}, ExitNeedsHuman)
	t.Setenv("WAYGLASS_AGENT", "john-tester")
	s.e.do(s.a, ExitOK, "approve", s.idB, "--reason", "Five tries matches the lockout policy")
	keyFor := func(agent string) string {
		t.Setenv("WAYGLASS_KEY", s.e.worker.key)
		return get(s.e.do(s.a, ExitOK, "keys", "create", "--agent", agent), "key").(string)
	}
	other, author := keyFor("agent-c"), keyFor("agent-b")

	fresh := filepath.Join(t.TempDir(), "fresh")
	s.e.git("", "clone", "--quiet", s.trunk, fresh)
	before := s.trunkHead()
	t.Setenv("WAYGLASS_KEY", other)
	t.Setenv("WAYGLASS_AGENT", "agent-b") // a bound key cannot borrow the author's name
	got := s.e.do(fresh, ExitError, "land", s.idB)
	if get(got, "error", "code") != api.CodeNotAuthor || !strings.Contains(get(got, "error", "message").(string), "only agent-b") {
		t.Fatalf("land by another agent = %v", got)
	}
	if s.trunkHead() != before || s.e.worker.aborts != 0 || len(s.e.worker.projects["demo"].lands) != 0 {
		t.Fatalf("a refused land touched the trunk or the land slot")
	}

	t.Setenv("WAYGLASS_KEY", author)
	got = s.e.do(fresh, ExitOK, "land", s.idB)
	if get(got, "verified") != true || s.trunkHead() != get(got, "commit") {
		t.Fatalf("land by the author = %v", got)
	}
}

// Only a person decides a change policy routed to a person: an agent key is
// refused with needs_human (exit 4) and never sees the change in its review
// queue; a key minted with --human, like the admin key, decides it.
func TestOnlyAPersonDecidesHumanRoutedChange(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"web/login.ts"})
	s.change("agent-b", s.b, map[string]string{"web/login.ts": "export const maxAttempts = 5;\n"}, ExitNeedsHuman)
	keyFor := func(args ...string) string {
		t.Setenv("WAYGLASS_KEY", s.e.worker.key)
		return get(s.e.do(s.a, ExitOK, append([]string{"keys", "create"}, args...)...), "key").(string)
	}
	agent, person := keyFor("--agent", "reviewer-bot"), keyFor("--agent", "alice@laptop", "--human")
	t.Setenv("WAYGLASS_KEY", s.e.worker.key)
	kinds := map[string]string{}
	for _, k := range get(s.e.do(s.a, ExitOK, "keys", "list"), "keys").([]any) {
		kinds[k.(map[string]any)["agent"].(string)] = k.(map[string]any)["kind"].(string)
	}
	if kinds["reviewer-bot"] != api.KindAgent || kinds["alice@laptop"] != api.KindHuman {
		t.Fatalf("key kinds = %v", kinds)
	}

	t.Setenv("WAYGLASS_KEY", agent)
	if q := get(s.e.do(s.a, ExitOK, "review", "--for", "agent"), "queue").([]any); len(q) != 0 {
		t.Fatalf("agent queue lists a change routed to a person: %v", q)
	}
	for _, verb := range []string{"approve", "reject"} {
		got := s.e.do(s.a, ExitNeedsHuman, verb, s.idB, "--reason", "Read the login change and its tests")
		if get(got, "error", "code") != api.CodeNeedsHuman || !strings.Contains(get(got, "error", "message").(string), "keys create --human") {
			t.Fatalf("%s with an agent key = %v", verb, got)
		}
	}
	if d := s.e.worker.projects["demo"].decisions; d[len(d)-1].Status != api.StatusPendingHuman {
		t.Fatalf("a refused decision changed the status to %s", d[len(d)-1].Status)
	}

	t.Setenv("WAYGLASS_KEY", person)
	got := s.e.do(s.a, ExitOK, "approve", s.idB, "--reason", "Five tries matches the lockout policy")
	if get(got, "decision", "status") != api.StatusApproved || get(got, "decision", "decided_by") != "alice@laptop" {
		t.Fatalf("approve with a human key = %v", got)
	}
}

// A land refused because the trunk policy changed keeps the Worker's code,
// exits 1, names the next step, and pushes nothing.
func TestLandPolicyChanged(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
	s.e.worker.policyChanged = true
	before := s.trunkHead()
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	got := s.e.do(s.a, ExitError, "land")
	if get(got, "error", "code") != api.CodePolicyChanged {
		t.Fatalf("land = %v", got)
	}
	msg := get(got, "error", "message").(string)
	for _, want := range []string{"policy.yaml changed", "nothing was pushed", "wayglass sync " + s.idA, "wayglass propose"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	if s.trunkHead() != before || len(s.e.worker.projects["demo"].lands) != 0 {
		t.Fatalf("a refused land touched the trunk or the land slot")
	}
}
