package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
)

// ---- fake Worker routes for sessions (mirror worker/src/index.ts) -----------

func (f *fakeWorker) sessionURL(r *http.Request, project, id string) string {
	return "http://" + r.Host + "/v1/projects/" + project + "/sessions/" + id
}

func (f *fakeWorker) shareSession(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	it := p.intent(r.PathValue("id"))
	if it == nil {
		fail(w, 404, "not_found", "intent not found")
		return
	}
	head := f.head(it.ForkRemote, "main")
	if c, _ := body["commit"].(string); c != "" && !strings.HasPrefix(head, c) {
		fail(w, 409, "conflict", "fork head is "+head+", not "+c+"; run wayglass push before sharing")
		return
	}
	snap := api.SessionSnapshot{Intent: *it, Decisions: []api.DecisionWithApprovals{}, Overlaps: p.overlaps(it), History: []api.AuditEvent{}}
	for _, d := range p.decisions {
		if d.IntentID == it.ID {
			snap.Decisions = append(snap.Decisions, api.DecisionWithApprovals{Decision: *d, Approvals: []api.Approval{}})
		}
	}
	notesText, _ := body["notes"].(string)
	s := &api.Session{ID: f.nextID("s"), IntentID: it.ID, SharedBy: r.Header.Get("X-Wayglass-Agent"), HeadCommit: &head,
		Notes: strings.TrimSpace(notesText), Snapshot: snap, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	p.sessions = append(p.sessions, s)
	ok(w, 201, map[string]any{"session": s, "url": f.sessionURL(r, p.info.Name, s.ID)})
}

func (f *fakeWorker) findSession(w http.ResponseWriter, r *http.Request) (*fakeProject, *api.Session) {
	p := f.projects[r.PathValue("project")]
	if p == nil {
		fail(w, 404, "not_found", "project not found")
		return nil, nil
	}
	for _, s := range p.sessions {
		if s.ID == r.PathValue("sid") {
			return p, s
		}
	}
	fail(w, 404, "not_found", "session not found")
	return nil, nil
}

func (f *fakeWorker) getSession(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, s := f.findSession(w, r)
	if s == nil {
		return
	}
	forks := []*api.Intent{}
	for _, it := range p.intents {
		if it.SessionID != nil && *it.SessionID == s.ID {
			forks = append(forks, it)
		}
	}
	ok(w, 200, map[string]any{"project": p.info.Name, "session": s, "forks": forks, "url": f.sessionURL(r, p.info.Name, s.ID)})
}

func (f *fakeWorker) forkSession(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, s := f.findSession(w, r)
	if s == nil {
		return
	}
	parent := p.intent(s.IntentID)
	// Stacked on the source like declare --on, or alongside it when the
	// source has pushed nothing yet (as in worker/src/index.ts forkIntent).
	base, parentID := parent.BaseCommit, parent.ParentID
	if h := f.head(parent.ForkRemote, "main"); h != "" && h != deref(parent.BaseCommit) {
		base, parentID = &h, &parent.ID
	}
	id := f.nextID("i")
	name := p.info.Name + "--" + id
	remote := filepath.Join(f.dir, name+".git")
	f.git("clone", "--quiet", "--bare", parent.ForkRemote, remote)
	if f.forkKeepsNotes {
		f.git("--git-dir="+remote, "fetch", "--quiet", parent.ForkRemote, "+refs/notes/wayglass:refs/notes/wayglass")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	sid, pid := s.ID, parent.ID
	it := &api.Intent{ID: id, Goal: parent.Goal, Reason: parent.Reason, Agent: r.Header.Get("X-Wayglass-Agent"),
		DeclaredPaths: parent.DeclaredPaths, ActualPaths: []string{}, ForkRepo: name, ForkRemote: remote,
		BaseCommit: base, ParentID: parentID, ParentIntentID: &pid, SessionID: &sid, Status: "open", CreatedAt: now, UpdatedAt: now}
	p.intents = append(p.intents, it)
	ovs := p.overlaps(it)
	for i := range ovs {
		if ovs[i].IntentID == parent.ID {
			ovs[i].Relation = "parent"
		}
	}
	ok(w, 201, map[string]any{
		"session": s, "url": f.sessionURL(r, p.info.Name, s.ID), "intent": it,
		"parent":   api.SessionParent{ID: parent.ID, Agent: parent.Agent, Status: parent.Status, ForkRepo: parent.ForkRepo},
		"fork":     api.Repo{Name: name, Remote: remote, DefaultBranch: "main"},
		"token":    f.mint(),
		"overlaps": ovs,
	})
}

// ---- tests ------------------------------------------------------------------

// TestSessionShareForkShow walks golden-path step 7: agent A shares its
// session; a second machine forks it into its own intent and fork, gets the
// code, notes and context, and keeps working.
func TestSessionShareForkShow(t *testing.T) {
	for _, keepNotes := range []bool{true, false} {
		t.Run(fmt.Sprintf("fork keeps notes=%v", keepNotes), func(t *testing.T) {
			e := newFlow(t)
			e.worker.forkKeepsNotes = keepNotes
			a := e.repo
			e.do(a, ExitOK, "init", "--project", "demo")

			da := e.do(a, ExitOK, "intent", "declare", "--goal", "Rate-limit login", "--reason", "Brute force in the audit log", "--path", "web/**")
			idA := get(da, "intent", "id").(string)
			e.write(a, "web/login.ts", "export function login() {}\nexport const maxAttempts = 5;\n")
			e.do(a, ExitOK, "push", "-m", "feat: limit login attempts")
			e.git(a, "notes", "--ref=refs/notes/wayglass", "add", "-m", `{"v":1,"intent_id":"`+idA+`"}`, "HEAD")
			pushed := e.do(a, ExitOK, "push")
			headA := get(pushed, "commit").(string)

			// An unpushed commit cannot be shared: the snapshot would point at code nobody has.
			e.write(a, "web/extra.ts", "x\n")
			e.git(a, "add", "-A")
			e.git(a, "commit", "--quiet", "-m", "wip")
			if got := e.do(a, ExitError, "session", "share"); get(got, "error", "code") != CodeNotPushed {
				t.Fatalf("share unpushed = %v", got)
			}
			e.git(a, "reset", "--quiet", "--hard", "HEAD~1")

			notesFile := filepath.Join(t.TempDir(), "handoff.md")
			e.write(filepath.Dir(notesFile), "handoff.md", "Lockout tests are missing.\nSee web/login.ts.\n")
			sh := e.do(a, ExitOK, "session", "share", "--note", "Limiter works.", "--notes-file", notesFile)
			url := get(sh, "url").(string)
			ref, err := api.ParseSessionURL(url)
			if err != nil || ref.Project != "demo" {
				t.Fatalf("share url %q: %v", url, err)
			}
			if get(sh, "session", "notes") != "Limiter works.\n\nLockout tests are missing.\nSee web/login.ts." ||
				get(sh, "session", "head_commit") != headA || get(sh, "session", "snapshot", "intent", "id") != idA {
				t.Fatalf("share = %v", sh)
			}

			// Second machine: a different agent, no checkout yet.
			t.Setenv("WAYGLASS_AGENT", "agent-vm")
			vm := t.TempDir()
			if got := e.do(vm, ExitError, "session", "show"); get(got, "error", "code") != CodeNotRepo {
				t.Fatalf("show outside a repo = %v", got)
			}
			fk := e.do(vm, ExitOK, "session", "fork", url, "--dir", "work")
			dir := filepath.Join(vm, "work")
			idB := get(fk, "intent", "id").(string)
			switch {
			case idB == idA, get(fk, "intent", "agent") != "agent-vm", get(fk, "intent", "parent_intent_id") != idA:
				t.Fatalf("fork intent = %v", get(fk, "intent"))
			case get(fk, "intent", "parent_id") != idA:
				t.Fatalf("fork must stack on its source: %v", get(fk, "intent"))
			case get(fk, "head") != headA || get(fk, "moved") != false || get(fk, "base") != headA:
				t.Fatalf("fork code = head %v moved %v base %v", get(fk, "head"), get(fk, "moved"), get(fk, "base"))
			case get(fk, "notes_fetched") != keepNotes:
				t.Fatalf("notes_fetched = %v, want %v", get(fk, "notes_fetched"), keepNotes)
			case get(fk, "token") != nil || get(fk, "token_scope") != "write":
				t.Fatalf("fork must not print the token: %v", fk)
			}
			if keepNotes {
				if n := e.git(dir, "notes", "--ref=refs/notes/wayglass", "show", headA); !strings.Contains(n, idA) {
					t.Fatalf("note = %q", n)
				}
			}
			if cur, _ := os.ReadFile(filepath.Join(dir, ".wayglass", "state", "current")); strings.TrimSpace(string(cur)) != idB {
				t.Fatalf("current intent = %q", cur)
			}
			if st := e.git(dir, "status", "--porcelain"); st != "" {
				t.Fatalf("fork left the checkout dirty (context must be git-ignored): %q", st)
			}

			// show: the context for the next agent, as JSON and as Markdown.
			shw := e.do(dir, ExitOK, "session", "show")
			if get(shw, "intent", "id") != idB || get(shw, "session", "notes") == nil {
				t.Fatalf("show = %v", shw)
			}
			t.Chdir(dir)
			code, out, _ := run(t, "session", "show")
			for _, want := range []string{"continuing intent **" + idB + "**", "Limiter works.", "Lockout tests are missing.", "Rate-limit login", "Wayglass-Intent: " + idB} {
				if code != ExitOK || !strings.Contains(out, want) {
					t.Fatalf("show markdown lacks %q (exit %d):\n%s", want, code, out)
				}
			}
			if md, _ := os.ReadFile(filepath.Join(dir, ".wayglass", "state", "session.md")); string(md) != out {
				t.Fatalf("session.md differs from show output")
			}
			if v := e.do(dir, ExitOK, "session", "show", url); get(v, "session", "id") != ref.ID || len(get(v, "forks").([]any)) != 1 {
				t.Fatalf("show url = %v", v)
			}

			// The new agent keeps working on its own fork, stacked on the
			// source: its change is only what it adds, not the source's commits.
			e.write(dir, "web/lockout_test.ts", "test('lockout')\n")
			pb := e.do(dir, ExitOK, "push", "-m", "test: cover the lockout")
			if files := fmt.Sprint(get(pb, "files")); files != "[web/lockout_test.ts]" || get(pb, "intent_id") != idB {
				t.Fatalf("push from fork = %v", pb)
			}
			if got := e.git(a, "ls-remote", get(fk, "fork", "remote").(string), "refs/heads/main"); !strings.HasPrefix(got, get(pb, "commit").(string)) {
				t.Fatalf("new fork main = %q", got)
			}
			// The source fork is untouched.
			if got := e.git(a, "ls-remote", get(da, "fork", "remote").(string), "refs/heads/main"); !strings.HasPrefix(got, headA) {
				t.Fatalf("source fork moved: %q", got)
			}

			assertNoTokens(t, dir, e.worker.tokens)
			assertNoTokens(t, a, e.worker.tokens)
		})
	}
}

// TestAbandonParentHandsCommitsToSessionFork: when the source of a session
// fork is abandoned, the fork takes over its commits. Its base moves back to
// the trunk commit the source started from, so its next push and proposal
// cover the source's work too.
func TestAbandonParentHandsCommitsToSessionFork(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo")
	trunkHead := e.git(a, "rev-parse", "HEAD")
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	da := e.do(a, ExitOK, "intent", "declare", "--goal", "Rate-limit login", "--reason", "Brute force in the audit log", "--path", "web/**")
	idA := get(da, "intent", "id").(string)
	e.write(a, "web/login.ts", "export const maxAttempts = 5;\n")
	e.do(a, ExitOK, "push", "-m", "feat: limit login attempts")
	url := get(e.do(a, ExitOK, "session", "share", "--note", "over to you"), "url").(string)

	t.Setenv("WAYGLASS_AGENT", "agent-b")
	vm := t.TempDir()
	fk := e.do(vm, ExitOK, "session", "fork", url, "--dir", "work")
	dir := filepath.Join(vm, "work")
	idB := get(fk, "intent", "id").(string)
	e.write(dir, "web/lockout.ts", "lockout\n")
	if pb := e.do(dir, ExitOK, "push", "-m", "feat: lockout"); fmt.Sprint(get(pb, "files")) != "[web/lockout.ts]" {
		t.Fatalf("stacked push = %v", pb)
	}

	// Only the author may abandon; a reason is required.
	if got := e.do(a, ExitError, "intent", "abandon", idA, "--reason", "agent-b carries it"); get(got, "error", "code") != "forbidden" {
		t.Fatalf("abandon by another agent = %v", got)
	}
	e.do(a, ExitUsage, "intent", "abandon", idA)
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	ab := e.do(a, ExitOK, "intent", "abandon", idA, "--reason", "agent-b carries it")
	switch {
	case get(ab, "intent", "status") != "abandoned", get(ab, "intent", "abandoned_reason") != "agent-b carries it":
		t.Fatalf("abandon = %v", ab)
	case fmt.Sprint(get(ab, "restacked")) != "["+idB+"]", get(ab, "fork_kept") != true, get(ab, "cleared_current") != true:
		t.Fatalf("abandon = %v", ab)
	}
	if got := e.do(a, ExitError, "push", idA); get(got, "error", "code") == nil {
		t.Fatalf("push after abandon = %v", got)
	}
	if l := e.do(a, ExitOK, "intent", "list"); len(get(l, "intents").([]any)) != 1 {
		t.Fatalf("list after abandon = %v", l)
	}

	// The fork now carries the abandoned commits: measured from trunk.
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	e.write(dir, "web/lockout.ts", "lockout v2\n")
	pb := e.do(dir, ExitOK, "push", "-m", "feat: lockout v2")
	if fmt.Sprint(get(pb, "files")) != "[web/lockout.ts web/login.ts]" || get(pb, "base") != trunkHead {
		t.Fatalf("push after restack = %v", pb)
	}
}

func TestSessionForkRefusals(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo")
	e.do(a, ExitOK, "intent", "declare", "--goal", "g", "--reason", "r", "--path", "web/**")
	sh := e.do(a, ExitOK, "session", "share")
	url := get(sh, "url").(string)

	vm := t.TempDir()
	tests := []struct {
		name string
		args []string
		exit int
		code string
	}{
		{"not a session URL", []string{"session", "fork", "https://wayglass.dev/s/abc"}, ExitUsage, CodeUsage},
		{"another Worker", []string{"session", "fork", "https://evil.example/v1/projects/demo/sessions/s0123456789"}, ExitError, CodeWrongWorker},
		{"unknown session", []string{"session", "fork", url[:strings.LastIndex(url, "/")+1] + "sffffffffff"}, ExitError, "not_found"},
		{"show another Worker", []string{"session", "show", "https://evil.example/v1/projects/demo/sessions/s0123456789"}, ExitError, CodeWrongWorker},
		{"show without a fork here", []string{"session", "show"}, ExitError, CodeNoSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := vm
			if tt.code == CodeNoSession {
				dir = a
			}
			if got := e.do(dir, tt.exit, tt.args...); get(got, "error", "code") != tt.code {
				t.Fatalf("%v = %v", tt.args, got)
			}
		})
	}

	// A non-empty --dir is refused before the Worker forks anything.
	e.write(vm, "busy/file", "x")
	before := len(e.worker.projects["demo"].intents)
	if got := e.do(vm, ExitUsage, "session", "fork", url, "--dir", "busy"); get(got, "error", "code") != CodeUsage {
		t.Fatalf("busy dir = %v", got)
	}
	if n := len(e.worker.projects["demo"].intents); n != before {
		t.Fatalf("a refused fork created an intent (%d -> %d)", before, n)
	}

	// The source pushes after the share: the fork gets the latest code and says so.
	e.write(a, "web/late.ts", "late\n")
	late := e.do(a, ExitOK, "push", "-m", "feat: late change")
	fk := e.do(vm, ExitOK, "session", "fork", url)
	if get(fk, "moved") != true || get(fk, "head") != get(late, "commit") || !strings.Contains(fmt.Sprint(get(fk, "warnings")), "moved") {
		t.Fatalf("moved fork = %v", fk)
	}
	if _, err := os.Stat(filepath.Join(vm, "demo-"+get(sh, "session", "id").(string))); err != nil {
		t.Fatalf("default --dir: %v", err)
	}
}

func TestReadNotes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "n.md")
	if err := os.WriteFile(file, []byte("\nfrom file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		list  []string
		file  string
		stdin string
		want  string
		err   bool
	}{
		{name: "none", want: ""},
		{name: "notes joined", list: []string{" one ", "", "two"}, want: "one\n\ntwo"},
		{name: "file", list: []string{"one"}, file: file, want: "one\n\nfrom file"},
		{name: "stdin", file: "-", stdin: "piped\n", want: "piped"},
		{name: "missing file", file: filepath.Join(t.TempDir(), "nope"), err: true},
		{name: "too long", list: []string{strings.Repeat("x", maxNotes+1)}, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readNotes(strings.NewReader(tt.stdin), tt.list, tt.file)
			if (err != nil) != tt.err || got != tt.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
