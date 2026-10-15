package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/gitx"
)

// flowEnv is a logged-in machine with a real git checkout, talking to the
// fake Worker over HTTP.
type flowEnv struct {
	t      *testing.T
	worker *fakeWorker
	repo   string
}

func newFlow(t *testing.T) *flowEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	hermetic(t)
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	f, srv := newFakeWorker(t)
	t.Setenv("WAYGLASS_URL", srv.URL)
	t.Setenv("WAYGLASS_KEY", f.key)
	t.Setenv("WAYGLASS_AGENT", "agent-a")

	repo := filepath.Join(t.TempDir(), "demo")
	e := &flowEnv{t: t, worker: f, repo: repo}
	e.git("", "init", "--quiet", "--initial-branch=main", repo)
	e.write(repo, "README.md", "# demo\n")
	e.write(repo, "web/login.ts", "export function login() {}\n")
	e.git(repo, "add", "-A")
	e.git(repo, "commit", "--quiet", "-m", "initial")
	return e
}

func (e *flowEnv) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := gitx.Git{Dir: dir}.Run(e.t.Context(), args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *flowEnv) write(dir, name, body string) {
	e.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// do runs wayglass --json in dir and decodes the envelope's data.
func (e *flowEnv) do(dir string, wantExit int, args ...string) map[string]any {
	e.t.Helper()
	e.t.Chdir(dir)
	exit, stdout, stderr := run(e.t, append(slices.Clone(args), "--json")...)
	env := checkEnvelope(e.t, stdout)
	if exit != wantExit {
		e.t.Fatalf("wayglass %s: exit %d, want %d\n%s", strings.Join(args, " "), exit, wantExit, stdout)
	}
	if stderr != "" {
		e.t.Fatalf("--json wrote to stderr: %q", stderr)
	}
	if !*env.OK {
		eb := map[string]any{"code": env.Error.Code, "message": env.Error.Message}
		if env.Error.Details != nil {
			var d map[string]any
			if err := json.Unmarshal(*env.Error.Details, &d); err != nil {
				e.t.Fatal(err)
			}
			eb["details"] = d
		}
		return map[string]any{"error": eb}
	}
	var data map[string]any
	if err := json.Unmarshal(*env.Data, &data); err != nil {
		e.t.Fatal(err)
	}
	return data
}

func get(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// TestGoldenPathFlow walks golden-path steps 1 to 4 and 6 against the fake
// Worker: init, two agents declare in parallel, push with a live overlap,
// propose routing docs to auto_merge, auth to a person and secrets to block,
// and why answering from the git note and from the Worker.
func TestGoldenPathFlow(t *testing.T) {
	e := newFlow(t)
	a := e.repo

	// 1. init: project, trunk, policy, committed and pushed.
	data := e.do(a, ExitOK, "init", "--project", "demo")
	if get(data, "policy_written") != true || get(data, "pushed") != "main" || get(data, "jurisdiction") != "us" {
		t.Fatalf("init = %v", data)
	}
	if get(data, "trunk_events", "subscribed") != true || len(get(data, "warnings").([]any)) != 0 {
		t.Fatalf("init trunk events = %v, warnings = %v", get(data, "trunk_events"), get(data, "warnings"))
	}
	trunk := get(data, "trunk", "remote").(string)
	if got := e.git(a, "ls-remote", trunk, "refs/heads/main"); !strings.HasPrefix(got, e.git(a, "rev-parse", "HEAD")) {
		t.Fatalf("trunk main = %q", got)
	}
	if _, err := os.Stat(filepath.Join(a, ".wayglass", "policy.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := e.do(a, ExitError, "init", "--project", "demo"); get(got, "error", "code") != "conflict" {
		t.Fatalf("second init = %v", got)
	}

	// Agent B works in its own clone of the trunk.
	b := filepath.Join(t.TempDir(), "agent-b")
	e.git(a, "clone", "--quiet", trunk, b)

	// 2. Two agents declare intents.
	da := e.do(a, ExitOK, "intent", "declare", "--goal", "Write the guide", "--reason", "New users are lost", "--path", "docs/**")
	idA := get(da, "intent", "id").(string)
	if v, _ := get(da, "token", "value").(string); !strings.HasPrefix(v, "art_v1_") {
		t.Fatalf("declare must return the fork token to agents: %v", da)
	}
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	db := e.do(b, ExitOK, "intent", "declare", "--goal", "Rate-limit login", "--reason", "Brute force in the audit log",
		"--path", "web/login.ts", "--path", "docs/guide.md")
	idB := get(db, "intent", "id").(string)
	if ov := get(db, "overlaps").([]any); len(ov) != 1 || get(ov[0].(map[string]any), "intent_id") != idA {
		t.Fatalf("declare overlaps = %v", get(db, "overlaps"))
	}

	list := e.do(a, ExitOK, "intent", "list")
	if n := len(get(list, "intents").([]any)); n != 2 {
		t.Fatalf("intent list has %d intents", n)
	}
	if got := e.do(a, ExitOK, "intent", "show", idB); get(got, "intent", "goal") != "Rate-limit login" {
		t.Fatalf("show = %v", got)
	}

	// 3. Agent A pushes a docs change; the overlap with B shows in --json.
	e.write(a, "docs/guide.md", "# Guide\n\nStart here.\n")
	pa := e.do(a, ExitOK, "push", "-m", "docs: add a getting-started guide")
	if get(pa, "committed") != true || get(pa, "intent_id") != idA {
		t.Fatalf("push = %v", pa)
	}
	if files := get(pa, "files").([]any); len(files) != 1 || files[0] != "docs/guide.md" {
		t.Fatalf("push files = %v (must be relative to the fork base)", files)
	}
	if ov := get(pa, "overlaps").([]any); len(ov) != 1 || get(ov[0].(map[string]any), "intent_id") != idB {
		t.Fatalf("push overlaps = %v", get(pa, "overlaps"))
	}
	fork := get(da, "fork", "remote").(string)
	if got := e.git(a, "ls-remote", fork, "refs/heads/main"); !strings.HasPrefix(got, get(pa, "commit").(string)) {
		t.Fatalf("fork main = %q, want %v", got, get(pa, "commit"))
	}
	if msg := e.git(a, "log", "-1", "--format=%B"); !strings.Contains(msg, "Wayglass-Intent: "+idA) {
		t.Fatalf("commit message lacks the intent trailer: %q", msg)
	}

	ov := e.do(a, ExitOK, "overlaps")
	if pairs := get(ov, "overlaps").([]any); len(pairs) != 1 {
		t.Fatalf("overlaps = %v", ov)
	}
	if ov := e.do(a, ExitOK, "overlaps", idB); len(get(ov, "overlaps").([]any)) != 1 ||
		get(get(ov, "overlaps").([]any)[0].(map[string]any), "a", "intent_id") != idB {
		t.Fatalf("overlaps %s = %v", idB, ov)
	}

	// 4a. Docs change auto-merges (exit 0) and gets a why note.
	pr := e.do(a, ExitOK, "propose")
	if get(pr, "decision", "action") != "auto_merge" || get(pr, "decision", "status") != "approved" || get(pr, "agrees") != true {
		t.Fatalf("propose docs = %v", pr)
	}
	if get(pr, "note_written") != true || get(pr, "preview", "rule", "index") == nil {
		t.Fatalf("propose docs note/rule = %v", pr)
	}
	if u := get(pr, "undeclared_paths").([]any); len(u) != 0 {
		t.Fatalf("docs change stayed inside docs/**, undeclared = %v", u)
	}

	// 6. why answers from the note in git.
	w := e.do(a, ExitOK, "why", "docs/guide.md:1")
	if get(w, "source") != "note" || get(w, "note", "intent_id") != idA || get(w, "note", "action") != "auto_merge" || get(w, "verified") != "worker" {
		t.Fatalf("why = %v", w)
	}

	// 4b. Auth change routes to a person (exit 4).
	e.write(b, "web/login.ts", "export function login() {}\nexport const maxAttempts = 5;\n")
	pb := e.do(b, ExitOK, "push", idB, "-m", "feat: limit login attempts")
	if files := get(pb, "files").([]any); len(files) != 1 || files[0] != "web/login.ts" {
		t.Fatalf("push b files = %v", files)
	}
	prb := e.do(b, ExitNeedsHuman, "propose", idB)
	if get(prb, "decision", "status") != "pending_human" || get(prb, "exit") != float64(ExitNeedsHuman) {
		t.Fatalf("propose auth = %v", prb)
	}
	if !slices.Contains(get(prb, "preview", "tags", "areas").([]any), any("auth")) {
		t.Fatalf("auth area missing: %v", get(prb, "preview", "tags"))
	}
	// No note for an undecided change: why falls back to the Worker.
	wb := e.do(b, ExitOK, "why", "web/login.ts:2")
	if get(wb, "source") != "worker" || get(wb, "worker", "decision", "action") != "human_approve" {
		t.Fatalf("why via worker = %v", wb)
	}
	if got := e.do(b, ExitError, "why", "README.md:1"); get(got, "error", "code") != CodeNoWhy {
		t.Fatalf("why without any record = %v", got)
	}

	// 5. The person policy routed it to approves, with a reason. The agent
	// that wrote the change cannot approve it itself.
	if got := e.do(b, ExitError, "approve", idB, "--reason", "self-approval"); get(got, "error", "code") != "conflict" {
		t.Fatalf("self-approval = %v", got)
	}
	// A verdict is bound to the decision it is about: the author proposing
	// again in between leaves the earlier decision undecidable
	// (decision_replaced), so a person never approves a change unread.
	firstDecision := get(prb, "decision", "id").(string)
	e.do(b, ExitNeedsHuman, "propose", idB)
	t.Setenv("WAYGLASS_AGENT", "john")
	if got := e.do(b, ExitError, "approve", idB, "--decision", firstDecision, "--reason", "Checked the lockout window"); get(got, "error", "code") != "decision_replaced" {
		t.Fatalf("approve of a replaced decision = %v", got)
	}
	// Without --decision the CLI reads the latest decision first and names it.
	ap := e.do(b, ExitOK, "approve", idB, "--reason", "Checked the lockout window")
	if ap["decision"].(map[string]any)["id"] == firstDecision {
		t.Fatalf("approve decided the replaced decision: %v", ap)
	}
	if get(ap, "decision", "status") != "approved" || get(ap, "approvals").([]any)[0].(map[string]any)["reason"] != "Checked the lockout window" {
		t.Fatalf("approve = %v", ap)
	}

	// 4c. A secret is blocked (exit 3). This Worker cannot mint fork
	// tokens, so push uses the one the agent kept from declare.
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	dc := e.do(a, ExitOK, "intent", "declare", "--goal", "Wire the mailer", "--reason", "Signup emails", "--path", "web/mail.ts")
	e.worker.noTokenEP = true
	e.write(a, "web/mail.ts", "const api_key = 'x';\n")
	if got := e.do(a, ExitError, "push", "-m", "feat: mailer"); get(got, "error", "code") != CodeNoToken {
		t.Fatalf("push without any token = %v", got)
	}
	t.Setenv(EnvForkToken, get(dc, "token", "value").(string))
	e.do(a, ExitOK, "push")
	dry := e.do(a, ExitBlocked, "propose", "--dry-run")
	if get(dry, "dry_run") != true || get(dry, "decision") != nil || get(dry, "preview", "action") != "block" {
		t.Fatalf("dry run = %v", dry)
	}
	prc := e.do(a, ExitBlocked, "propose")
	if get(prc, "decision", "status") != "blocked" {
		t.Fatalf("propose secret = %v", prc)
	}

	// Tokens are never written to disk in either checkout.
	for _, dir := range []string{a, b} {
		assertNoTokens(t, dir, e.worker.tokens)
	}
}

func assertNoTokens(t *testing.T, root string, tokens []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.Contains(p, string(filepath.Separator)+"objects"+string(filepath.Separator)) {
			return nil // compressed git objects; the test content holds no tokens
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, tok := range tokens {
			base, _, _ := strings.Cut(tok, "?")
			if strings.Contains(string(b), base) {
				t.Errorf("%s contains a repo token", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPushHumanOutputWarnsOnOverlap(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	e.do(e.repo, ExitOK, "intent", "declare", "--goal", "one", "--reason", "r", "--path", "web/**")
	e.do(e.repo, ExitOK, "intent", "declare", "--goal", "two", "--reason", "r", "--path", "web/login.ts")
	e.write(e.repo, "web/login.ts", "changed\n")
	t.Chdir(e.repo)
	exit, stdout, stderr := run(t, "push", "-m", "web: change")
	if exit != ExitOK {
		t.Fatalf("exit %d: %s %s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "Pushed") || !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "overlaps") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestProposeWarnsOnUndeclaredPaths(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	e.do(e.repo, ExitOK, "intent", "declare", "--goal", "Write the guide", "--reason", "r", "--path", "docs/**")
	e.write(e.repo, "docs/guide.md", "# Guide\n")
	e.write(e.repo, "web/app.ts", "export {}\n")
	e.do(e.repo, ExitOK, "push", "-m", "docs: guide and a stray file")
	pr := e.do(e.repo, ExitOK, "propose", "--dry-run")
	if u := get(pr, "undeclared_paths").([]any); len(u) != 1 || u[0] != "web/app.ts" {
		t.Fatalf("undeclared = %v", get(pr, "undeclared_paths"))
	}
	if w := get(pr, "warnings").([]any); len(w) != 1 || !strings.Contains(w[0].(string), "web/app.ts") {
		t.Fatalf("warnings = %v", get(pr, "warnings"))
	}
}

func TestProposeBeforePush(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	e.do(e.repo, ExitOK, "intent", "declare", "--goal", "g", "--reason", "r", "--path", "docs/**")
	if got := e.do(e.repo, ExitError, "propose"); get(got, "error", "code") != CodeNotPushed {
		t.Fatalf("propose before push = %v", got)
	}
}

func TestLoginStoresCredentials(t *testing.T) {
	e := newFlow(t)
	t.Setenv("WAYGLASS_URL", "")
	t.Setenv("WAYGLASS_KEY", "")
	srvURL := ""
	{
		_, srv := newFakeWorker(t)
		srvURL = srv.URL
	}
	t.Chdir(e.repo)
	var stdout, stderr strings.Builder
	root := NewRoot(BuildInfo{})
	root.SetArgs([]string{"login", "--worker", srvURL, "--key-stdin", "--json"})
	root.SetIn(strings.NewReader("test-key\n"))
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("login: %v %s", err, stdout.String())
	}
	if strings.Contains(stdout.String(), "test-key") {
		t.Fatalf("login echoed the key: %s", stdout.String())
	}
	var env struct {
		Data loginResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Data.Verified {
		t.Fatalf("login = %+v", env.Data)
	}
	st, err := os.Stat(env.Data.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode = %v, %v", st, err)
	}
	// A wrong key is refused and nothing is stored over the good one.
	root = NewRoot(BuildInfo{})
	root.SetArgs([]string{"login", "--worker", srvURL, "--key-stdin", "--json"})
	root.SetIn(strings.NewReader("wrong\n"))
	root.SetOut(&stdout)
	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Fatal("login with a wrong key succeeded")
	}
	// The stored login now drives commands.
	if got := e.do(e.repo, ExitOK, "init", "--project", "demo2"); get(got, "project", "name") != "demo2" {
		t.Fatalf("init after login = %v", got)
	}
}

func TestProjectNameFrom(t *testing.T) {
	for in, want := range map[string]string{
		"Wayglass":       "wayglass",
		"my--repo":       "my-repo",
		"My Repo (copy)": "my-repo-copy",
		"_x_":            "x",
	} {
		if got := projectNameFrom(in); got != want || !validProjectName(got) {
			t.Errorf("projectNameFrom(%q) = %q, want %q", in, got, want)
		}
	}
	if validProjectName("a--b") || validProjectName("-a") || validProjectName("") {
		t.Error("validProjectName accepted a bad name")
	}
}

// A Worker that could not subscribe the trunk still creates the project, and
// init says so instead of failing.
func TestInitWarnsWhenTrunkEventsAreNotSubscribed(t *testing.T) {
	e := newFlow(t)
	e.worker.noEvents = true
	data := e.do(e.repo, ExitOK, "init", "--project", "demo")
	if get(data, "trunk_events", "reason") != "not_configured" {
		t.Fatalf("trunk_events = %v", get(data, "trunk_events"))
	}
	warnings := get(data, "warnings").([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "trunk push events not subscribed") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// init --public asks the Worker to opt the project in to the public view at
// creation; without the flag nothing is made public.
func TestInitPublic(t *testing.T) {
	e := newFlow(t)
	if data := e.do(e.repo, ExitOK, "init", "--project", "demo-pub", "--public"); get(data, "public") != true {
		t.Fatalf("public = %v", get(data, "public"))
	}
	other := newFlow(t)
	if data := other.do(other.repo, ExitOK, "init", "--project", "demo-priv"); get(data, "public") != false {
		t.Fatalf("public without the flag = %v", get(data, "public"))
	}
}

// Land: an auto-merged docs change fast-forwards the trunk; a change that was
// approved later merges onto the moved trunk. Both carry their why note to the
// trunk and the mirror, and the agent that wrote a change cannot approve it.
func TestLandFlow(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	trunk := get(e.do(a, ExitOK, "init", "--project", "demo"), "trunk", "remote").(string)
	mirror := filepath.Join(t.TempDir(), "github.git")
	e.git("", "init", "--quiet", "--bare", "--initial-branch=main", mirror)
	e.git(a, "push", "--quiet", mirror, "main")
	cfg := filepath.Join(a, ".wayglass", "config.yaml")
	b0, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.write(a, ".wayglass/config.yaml", string(b0)+"mirror: "+mirror+"\n")
	e.git(a, "commit", "--quiet", "-am", "chore: mirror landed changes to GitHub")
	e.git(a, "push", "--quiet", trunk, "main")
	e.git(a, "push", "--quiet", mirror, "main")
	b := filepath.Join(t.TempDir(), "agent-b")
	e.git("", "clone", "--quiet", trunk, b)

	// Both intents fork the same trunk commit.
	idA := get(e.do(a, ExitOK, "intent", "declare", "--goal", "Guide", "--reason", "Users are lost", "--path", "docs/**"), "intent", "id").(string)
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	idB := get(e.do(b, ExitOK, "intent", "declare", "--goal", "Rate-limit login", "--reason", "Brute force", "--path", "web/login.ts"), "intent", "id").(string)

	// A: docs auto-merge, land fast-forwards.
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	e.write(a, "docs/guide.md", "# Guide\n")
	e.do(a, ExitOK, "push", "-m", "docs: guide")
	e.do(a, ExitOK, "propose")
	la := e.do(a, ExitOK, "land")
	headA := e.git(a, "rev-parse", "HEAD")
	if get(la, "fast_forward") != true || get(la, "verified") != true || get(la, "commit") != headA ||
		get(la, "mirrored") != true || get(la, "note_written") != true ||
		get(la, "local_fast_forwarded") != false || get(la, "local_up_to_date") != true || get(la, "trunk_moved") != false {
		t.Fatalf("land A = %v", la)
	}
	for _, remote := range []string{trunk, mirror} {
		if got := e.git(a, "ls-remote", remote, "refs/heads/main"); !strings.HasPrefix(got, headA) {
			t.Fatalf("%s main = %q, want %s", remote, got, headA)
		}
	}
	if got := e.do(a, ExitError, "land"); get(got, "error", "code") != "conflict" {
		t.Fatalf("second land = %v", got)
	}

	// B: auth change waits for a person, then lands as a merge onto the moved trunk.
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	e.write(b, "web/login.ts", "export const maxAttempts = 5;\n")
	e.do(b, ExitOK, "push", "-m", "feat: limit login attempts")
	e.do(b, ExitNeedsHuman, "propose")
	if got := e.do(b, ExitError, "land"); get(got, "error", "code") != "conflict" {
		t.Fatalf("land before approval = %v", got)
	}
	t.Setenv("WAYGLASS_AGENT", "john")
	e.do(b, ExitOK, "approve", idB, "--reason", "Lockout window is fine")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	lb := e.do(b, ExitOK, "land")
	merged := get(lb, "commit").(string)
	if get(lb, "fast_forward") != false || get(lb, "verified") != true || get(lb, "mirrored") != true {
		t.Fatalf("land B = %v", lb)
	}
	if parents := strings.Fields(e.git(b, "log", "-1", "--format=%P", merged)); len(parents) != 2 || parents[0] != headA {
		t.Fatalf("merge parents = %v, want first parent %s", parents, headA)
	}
	if msg := e.git(b, "log", "-1", "--format=%B", merged); !strings.Contains(msg, "Why: Brute force") || !strings.Contains(msg, "Wayglass-Intent: "+idB) {
		t.Fatalf("merge message = %q", msg)
	}
	if got := e.git(b, "ls-remote", mirror, "refs/heads/main"); !strings.HasPrefix(got, merged) {
		t.Fatalf("mirror main = %q, want %s", got, merged)
	}

	// The trunk's notes hold both intents' whys: a fresh clone answers from git.
	c := filepath.Join(t.TempDir(), "fresh")
	e.git("", "clone", "--quiet", mirror, c)
	e.git(c, "fetch", "--quiet", mirror, "+refs/notes/wayglass:refs/notes/wayglass")
	for commit, want := range map[string]string{headA: idA, merged: idB} {
		if got := e.git(c, "notes", "--ref=wayglass", "show", commit); !strings.Contains(got, want) {
			t.Fatalf("note on %s = %q, want intent %s", commit, got, want)
		}
	}
	_ = idA
	assertNoTokens(t, a, e.worker.tokens)
	assertNoTokens(t, b, e.worker.tokens)
}

// Golden-path step 4: personal data in a project created in the eu namespace
// is quarantined; the same change in a us project is not.
func TestPIIQuarantinedInEUProject(t *testing.T) {
	for _, tc := range []struct {
		jurisdiction string
		exit         int
		action       string
	}{
		{"eu", ExitNeedsHuman, "quarantine"},
		{"us", ExitOK, "agent_review"},
	} {
		t.Run(tc.jurisdiction, func(t *testing.T) {
			e := newFlow(t)
			in := e.do(e.repo, ExitOK, "init", "--project", "demo", "--jurisdiction", tc.jurisdiction)
			if get(in, "jurisdiction") != tc.jurisdiction || get(in, "project", "jurisdiction") != tc.jurisdiction {
				t.Fatalf("init = %v", in)
			}
			e.do(e.repo, ExitOK, "intent", "declare", "--goal", "Profile page", "--reason", "Users asked", "--path", "web/profile.ts")
			e.write(e.repo, "web/profile.ts", "save({ date_of_birth: dob })\n")
			e.do(e.repo, ExitOK, "push", "-m", "feat: profile")
			pr := e.do(e.repo, tc.exit, "propose")
			if get(pr, "decision", "action") != tc.action {
				t.Fatalf("propose = %v", pr)
			}
			tags := get(pr, "preview", "tags").(map[string]any)
			if tags["jurisdiction"] != tc.jurisdiction || !slices.Contains(tags["data_classes"].([]any), any("pii")) {
				t.Fatalf("tags = %v", tags)
			}
		})
	}
}
