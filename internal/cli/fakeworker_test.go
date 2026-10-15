package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/intent"
)

// fakeWorker is an in-memory stand-in for worker/src/index.ts used only by
// tests. Repos are local bare git repos, so push and fetch really happen;
// only the Artifacts control plane and the Durable Object are faked.
type fakeWorker struct {
	t   *testing.T
	dir string
	key string

	mu        sync.Mutex
	projects  map[string]*fakeProject
	seq       int
	tokens    []string        // every token handed out, to check none reach disk
	noTokenEP bool            // simulate a Worker without POST /v1/intents/:id/token
	noEvents  bool            // simulate a Worker without an events token
	noStack   bool            // simulate a Worker that predates stacked intents (ignores "on")
	aborts    int             // lands given back through /land/abort
	migrated  map[string]bool // projects whose residency was migrated (project_test.go)
	// forkFailures makes the next n declares fail like a transient Artifacts
	// fork error (seen live with several agents declaring at once).
	forkFailures int
	// forkKeepsNotes makes a session fork copy refs/notes/wayglass. Whether
	// real Artifacts forks do is unverified, so tests cover both.
	forkKeepsNotes bool
	// agentKeys are per-agent keys by plaintext (the real Worker keeps hashes).
	agentKeys map[string]*api.AgentKey
	noWhoami  bool // simulate a Worker that predates GET /v1/whoami
	// noReadToken refuses POST /v1/intents/:id/read-token; onReadToken runs
	// before a read token is handed out (e.g. to move the fork under a land).
	noReadToken bool
	onReadToken func(*api.Intent)
	readTokens  int // read tokens handed out
	// trunkReadTokens counts POST /v1/projects/:name/trunk/read-token tokens.
	trunkReadTokens int
	noRestack       bool // simulate a Worker that predates POST /v1/intents/:id/restack
	restacks        int
	// policyChanged makes land refuse like a Worker whose trunk policy moved
	// after the decision (policy_changed, before any trunk token).
	policyChanged bool
	// aiVerdict is what the fake AI reviewer answers on
	// POST /v1/intents/:id/ai-review: "" (the reviewer is off: ai_unavailable),
	// "approve" or "escalate". The real one is worker/src/aireview.ts.
	aiVerdict string
	aiReviews int
	// noAIAdvice simulates a Worker that predates POST /v1/intents/:id/ai-advice.
	noAIAdvice bool
}

const fakeAIReviewer = "workers-ai:@cf/meta/llama-4-scout-17b-16e-instruct"

type fakeProject struct {
	info      api.Project
	intents   []*api.Intent
	decisions []*api.Decision
	approvals map[string][]api.Approval // by decision id
	lands     map[string]*fakeLand      // issued lands by intent id
	audit     []api.AuditEvent
	sessions  []*api.Session
}

type fakeLand struct {
	trunkBefore string
}

func (p *fakeProject) log(kind, actor, intentID string, data map[string]any) {
	b, _ := json.Marshal(data)
	e := api.AuditEvent{Seq: len(p.audit) + 1, At: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Actor: actor, Data: b}
	if intentID != "" {
		e.IntentID = &intentID
	}
	p.audit = append(p.audit, e)
}

// out runs git and returns its trimmed output, "" on failure.
func (f *fakeWorker) out(args ...string) string {
	o, err := (gitx.Git{}).Run(f.t.Context(), args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(o)
}

func newFakeWorker(t *testing.T) (*fakeWorker, *httptest.Server) {
	f := &fakeWorker{t: t, dir: t.TempDir(), key: "test-key", projects: map[string]*fakeProject{}, agentKeys: map[string]*api.AgentKey{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects", f.createProject)
	mux.HandleFunc("GET /v1/projects", f.listProjects)
	mux.HandleFunc("POST /v1/intents", f.declare)
	mux.HandleFunc("GET /v1/intents", f.list)
	mux.HandleFunc("GET /v1/cost", f.cost)
	mux.HandleFunc("POST /v1/intents/{id}/token", f.token)
	mux.HandleFunc("POST /v1/intents/{id}/read-token", f.readToken)
	mux.HandleFunc("POST /v1/intents/{id}/push", f.push)
	mux.HandleFunc("POST /v1/intents/{id}/propose", f.propose)
	mux.HandleFunc("GET /v1/why", f.why)
	mux.HandleFunc("GET /v1/decisions", f.decisions)
	mux.HandleFunc("POST /v1/intents/{id}/decide", f.decide)
	mux.HandleFunc("POST /v1/decisions/{id}", f.decide)
	mux.HandleFunc("POST /v1/intents/{id}/ai-review", f.aiReview)
	mux.HandleFunc("POST /v1/intents/{id}/ai-advice", f.aiAdvice)
	mux.HandleFunc("POST /v1/intents/{id}/land", f.land)
	mux.HandleFunc("POST /v1/intents/{id}/landed", f.landed)
	mux.HandleFunc("POST /v1/intents/{id}/land/abort", f.abort)
	mux.HandleFunc("POST /v1/intents/{id}/abandon", f.abandon)
	mux.HandleFunc("POST /v1/intents/{id}/restack", f.restack)
	mux.HandleFunc("GET /v1/audit", f.auditLog)
	mux.HandleFunc("POST /v1/intents/{id}/sessions", f.shareSession)
	mux.HandleFunc("GET /v1/projects/{project}/sessions/{sid}", f.getSession)
	mux.HandleFunc("POST /v1/projects/{project}/sessions/{sid}/fork", f.forkSession)
	mux.HandleFunc("GET /v1/whoami", f.whoami)
	mux.HandleFunc("POST /v1/keys", f.createKey)
	mux.HandleFunc("GET /v1/keys", f.listKeys)
	mux.HandleFunc("DELETE /v1/keys/{id}", f.revokeKey)
	mux.HandleFunc("POST /v1/projects/{name}/trunk/read-token", f.trunkReadToken)
	mux.HandleFunc("DELETE /v1/projects/{name}", f.deleteProject)
	mux.HandleFunc("POST /v1/projects/{name}/migrate-residency", f.migrateResidency)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.authenticate(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func ok(w http.ResponseWriter, status int, data any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ok": true, "data": data})
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ok": false, "error": map[string]string{"code": code, "message": msg}})
}

func (f *fakeWorker) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%010x", prefix, f.seq)
}

func (f *fakeWorker) mint() api.Token {
	f.seq++
	v := fmt.Sprintf("art_v1_%040x?expires=%d", f.seq, time.Now().Add(time.Hour).Unix())
	f.tokens = append(f.tokens, v)
	return api.Token{Value: v, Scope: "write", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
}

func (f *fakeWorker) git(args ...string) {
	f.t.Helper()
	if _, err := (gitx.Git{}).Run(f.t.Context(), args...); err != nil {
		f.t.Errorf("fake worker git: %v", err)
	}
}

func decode(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func (f *fakeWorker) project(w http.ResponseWriter, r *http.Request, body map[string]any) *fakeProject {
	name := r.URL.Query().Get("project")
	if name == "" && body != nil {
		name, _ = body["project"].(string)
	}
	if name == "" {
		fail(w, 400, "bad_request", "project is required")
		return nil
	}
	p := f.projects[name]
	if p == nil {
		fail(w, 404, "not_found", "project "+name+" not found")
	}
	return p
}

func (p *fakeProject) intent(id string) *api.Intent {
	for _, i := range p.intents {
		if i.ID == id {
			return i
		}
	}
	return nil
}

// inStack reports whether anc is child's parent at any depth: a stack is
// not an overlap, as in the Worker.
func (p *fakeProject) inStack(child, anc *api.Intent) bool {
	for id := deref(child.ParentID); id != ""; {
		if id == anc.ID {
			return true
		}
		next := p.intent(id)
		if next == nil {
			return false
		}
		id = deref(next.ParentID)
	}
	return false
}

func (p *fakeProject) overlaps(it *api.Intent) []api.Overlap {
	out := []api.Overlap{}
	for _, o := range p.intents {
		if o.ID == it.ID || !slices.Contains(activeStatuses, o.Status) || p.inStack(it, o) || p.inStack(o, it) {
			continue
		}
		pairs := intent.Intersect(intent.Paths(it.DeclaredPaths, it.ActualPaths), intent.Paths(o.DeclaredPaths, o.ActualPaths))
		if len(pairs) == 0 {
			continue
		}
		ov := api.Overlap{IntentID: o.ID, Goal: o.Goal, Agent: o.Agent, Status: o.Status}
		for _, pp := range pairs {
			ov.Paths = append(ov.Paths, api.PathOverlap{Mine: pp.Mine, Theirs: pp.Theirs})
		}
		out = append(out, ov)
	}
	return out
}

func (f *fakeWorker) createProject(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	name, _ := body["name"].(string)
	if f.projects[name] != nil {
		fail(w, 409, "conflict", "project "+name+" already exists")
		return
	}
	remote := filepath.Join(f.dir, name+".git")
	f.git("init", "--quiet", "--bare", "--initial-branch=main", remote)
	j, _ := body["jurisdiction"].(string)
	p := &fakeProject{approvals: map[string][]api.Approval{}, lands: map[string]*fakeLand{}, info: api.Project{Name: name, TrunkRepo: name, TrunkRemote: remote, Jurisdiction: cmp.Or(j, "us"), CreatedAt: time.Now().UTC().Format(time.RFC3339)}}
	f.projects[name] = p
	p.log("project.created", r.Header.Get("X-Wayglass-Agent"), "", map[string]any{"repo": name})
	events := api.TrunkEvents{Subscribed: true, SubscriptionID: "sub-" + name}
	if f.noEvents {
		events = api.TrunkEvents{Reason: "not_configured", Detail: "missing CF_EVENTS_API_TOKEN"}
	}
	public, _ := body["public"].(bool)
	ok(w, 201, map[string]any{"project": p.info, "public": public, "trunk": api.Repo{Name: name, Remote: remote, DefaultBranch: "main"}, "token": f.mint(), "events": events})
}

func (f *fakeWorker) declare(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	if f.forkFailures > 0 {
		f.forkFailures--
		fail(w, 502, "upstream_error", "artifacts fork failed (INTERNAL_ERROR): An internal error occurred.")
		return
	}
	source := p.info.TrunkRemote
	var parent *api.Intent
	if on, _ := body["on"].(string); on != "" && !f.noStack {
		if parent = p.intent(on); parent == nil {
			fail(w, 404, "not_found", "intent "+on+" not found")
			return
		}
		if !slices.Contains(activeStatuses, parent.Status) {
			fail(w, 409, "conflict", "intent "+on+" is "+parent.Status)
			return
		}
		if h := f.head(parent.ForkRemote, "main"); h == "" || h == deref(parent.BaseCommit) {
			fail(w, 409, "conflict", "intent "+on+" has nothing pushed yet")
			return
		}
		source = parent.ForkRemote
	}
	id := f.nextID("i")
	forkName := p.info.Name + "--" + id
	remote := filepath.Join(f.dir, forkName+".git")
	base := f.head(source, "main")
	f.git("clone", "--quiet", "--bare", source, remote)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	it := &api.Intent{
		ID: id, Goal: body["goal"].(string), Reason: body["reason"].(string), Agent: r.Header.Get("X-Wayglass-Agent"),
		DeclaredPaths: strs(body["paths"]), ActualPaths: []string{}, ForkRepo: forkName, ForkRemote: remote,
		Status: "open", CreatedAt: now, UpdatedAt: now,
	}
	if base != "" {
		it.BaseCommit = &base
	}
	if parent != nil {
		it.ParentID = &parent.ID
	}
	p.intents = append(p.intents, it)
	p.log("intent.declared", it.Agent, id, map[string]any{"goal": it.Goal, "stacked_on": deref(it.ParentID)})
	ok(w, 201, map[string]any{"intent": it, "fork": api.Repo{Name: forkName, Remote: remote, DefaultBranch: "main"}, "token": f.mint(), "overlaps": p.overlaps(it)})
}

func (f *fakeWorker) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.project(w, r, nil); p != nil {
		ok(w, 200, map[string]any{"intents": p.intents})
	}
}

// cost charges each intent one fork and one token, like a declare on the
// real Worker (whose full counts are tested in worker/test).
func (f *fakeWorker) cost(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.project(w, r, nil)
	if p == nil {
		return
	}
	intents := map[string]any{}
	landed := 0
	for _, in := range p.intents {
		intents[in.ID] = map[string]any{"ops": 2, "by_op": map[string]int{"fork": 1, "createToken": 1}, "tokens_minted": 1, "est_usd": 0.0003}
		if in.Status == "landed" {
			landed++
		}
	}
	n := len(p.intents)
	var perLanded any
	if landed > 0 {
		perLanded = map[string]any{"landed": landed, "ops": 2, "est_usd": 0.0003}
	}
	ok(w, 200, map[string]any{
		"scope":   "Worker-side Artifacts binding calls only (fake Worker)",
		"pricing": map[string]any{"usd_per_1000_ops": 0.15, "included_ops_per_month": 10000, "source": "https://developers.cloudflare.com/artifacts/platform/pricing/", "as_of": "2026-10-11"},
		"total":   map[string]any{"ops": 2 * n, "by_op": map[string]int{"fork": n, "createToken": n}, "tokens_minted": n, "est_usd": 0.0003 * float64(n)},
		"project": map[string]any{"ops": 0, "by_op": map[string]int{}, "tokens_minted": 0, "est_usd": 0},
		"intents": intents, "events": map[string]int{}, "per_landed_change": perLanded,
	})
}

func (f *fakeWorker) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noTokenEP {
		fail(w, 404, "not_found", "no route for "+r.URL.Path)
		return
	}
	p := f.project(w, r, decode(r))
	if p == nil {
		return
	}
	if p.intent(r.PathValue("id")) == nil {
		fail(w, 404, "not_found", "intent not found")
		return
	}
	ok(w, 200, map[string]any{"token": f.mint()})
}

func (f *fakeWorker) readToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.project(w, r, decode(r))
	if p == nil {
		return
	}
	it := p.intent(r.PathValue("id"))
	if it == nil {
		fail(w, 404, "not_found", "intent not found")
		return
	}
	if f.noReadToken {
		fail(w, 403, "forbidden", "read tokens are not available here")
		return
	}
	if f.onReadToken != nil {
		f.onReadToken(it)
	}
	f.readTokens++
	ok(w, 201, map[string]any{"intent": it, "fork": map[string]string{"repo": it.ForkRepo, "remote": it.ForkRemote}, "token": f.mint()})
}

func (f *fakeWorker) push(w http.ResponseWriter, r *http.Request) {
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
	paths := []string{}
	if body["paths"] != nil {
		paths = strs(body["paths"])
	}
	if !slices.Contains(activeStatuses, it.Status) {
		fail(w, 409, "conflict", "intent "+it.ID+" is "+it.Status)
		return
	}
	it.ActualPaths = intent.Paths(it.ActualPaths, paths)
	if c, _ := body["commit"].(string); c != "" {
		it.HeadCommit = &c
	}
	p.log("intent.pushed", r.Header.Get("X-Wayglass-Agent"), it.ID, map[string]any{"paths": paths, "commit": body["commit"]})
	ok(w, 200, map[string]any{"intent": it, "overlaps": p.overlaps(it)})
}

func (f *fakeWorker) propose(w http.ResponseWriter, r *http.Request) {
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
	action := body["action"].(string)
	status := map[string]string{
		"auto_merge": "approved", "agent_review": "pending_agent", "human_approve": "pending_human",
		"multi_approve": "pending_human", "quarantine": "quarantined", "block": "blocked",
	}[action]
	tags, _ := json.Marshal(body["tags"])
	commit, _ := body["commit"].(string)
	rule, _ := body["matched_rule"].(string)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Like the Worker (ownchange.ts), the decision covers what landing would
	// add to the trunk: the paths from the merge base with the trunk head to
	// the head, that also differ from the trunk head. Without a trunk, from
	// the fork base.
	var paths []string
	from := deref(it.BaseCommit)
	gd := "--git-dir=" + it.ForkRemote
	if deref(it.ParentID) == "" && f.head(p.info.TrunkRemote, "main") != "" {
		if _, err := (gitx.Git{}).Run(f.t.Context(), gd, "fetch", "--quiet", p.info.TrunkRemote, "+refs/heads/main:refs/fake/trunk"); err == nil {
			from = "refs/fake/trunk"
		}
	}
	if out := f.out(gd, "diff", "--name-only", "--no-renames", from+"..."+commit); out != "" {
		paths = strings.Fields(out)
	}
	if from == "refs/fake/trunk" {
		vsTrunk := strings.Fields(f.out(gd, "diff", "--name-only", "--no-renames", from, commit))
		paths = slices.DeleteFunc(paths, func(x string) bool { return !slices.Contains(vsTrunk, x) })
	}
	d := &api.Decision{ID: f.nextID("d"), IntentID: it.ID, CommitSHA: &commit, Tags: tags, MatchedRule: &rule,
		Action: action, Reason: body["reason"].(string), Status: status, ApprovalsNeeded: 1, Paths: paths, CreatedAt: now}
	if status == "approved" || status == "blocked" {
		by := "policy"
		d.DecidedBy, d.DecidedAt = &by, &now
	}
	// Like the Worker, record the trunk policy the decision was made under.
	pol := f.trunkPolicy(p)
	d.PolicySHA256 = &pol
	p.decisions = append(p.decisions, d)
	it.Status = "proposed"
	p.log("decision.proposed", r.Header.Get("X-Wayglass-Agent"), it.ID, map[string]any{"action": action, "status": status, "matched_rule": rule, "reason": d.Reason})
	ok(w, 201, map[string]any{"intent": it, "decision": d})
}

func (f *fakeWorker) decide(w http.ResponseWriter, r *http.Request) {
	caller, _ := f.identity(r) // takes f.mu itself
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	// POST /v1/intents/:id/decide names an intent, /v1/decisions/:id a decision.
	byDecision := strings.HasPrefix(r.URL.Path, "/v1/decisions/")
	id := r.PathValue("id")
	var d *api.Decision
	for _, x := range p.decisions {
		if (!byDecision && x.IntentID == id) || (byDecision && x.ID == id) {
			d = x
		}
	}
	if d == nil {
		fail(w, 409, "conflict", "intent "+id+" has no proposed change to decide")
		return
	}
	// Mirrors the Worker: a verdict by intent names the decision it is about,
	// and a decision the intent re-proposed past is refused.
	proposals := 0
	for _, x := range p.decisions {
		if x.IntentID == d.IntentID {
			proposals++
		}
	}
	if !byDecision {
		if want, _ := body["decision_id"].(string); want != "" && want != d.ID {
			fail(w, 409, api.CodeDecisionReplaced, "intent "+id+" was proposed again after decision "+want+": its latest decision is "+d.ID)
			return
		} else if want == "" && proposals > 1 {
			fail(w, 409, api.CodeDecisionReplaced, "intent "+id+" has been proposed more than once, so an approval must name what was reviewed")
			return
		}
	}
	verdictReason, _ := body["reason"].(string)
	if placeholderReason.MatchString(strings.TrimSpace(verdictReason)) || strings.TrimSpace(verdictReason) == "" {
		fail(w, 400, "bad_request", fmt.Sprintf("reason looks like a placeholder (%q); say what you checked or why", verdictReason))
		return
	}
	if fillerReason(verdictReason) {
		fail(w, 400, "bad_request", fmt.Sprintf("reason %q is a rubber stamp, not a why; the audit log keeps it forever, so say what you checked", verdictReason))
		return
	}
	if d.Status == api.StatusSuperseded {
		fail(w, 409, api.CodeSuperseded, "decision "+d.ID+" was superseded when intent "+d.IntentID+" was re-stacked; its author must propose again")
		return
	}
	// Mirrors decide in worker/src/project.ts: only a person decides a change
	// policy routed to a person.
	if (d.Status == api.StatusPendingHuman || d.Status == api.StatusQuarantined) && caller.kind() != api.KindHuman {
		fail(w, 403, api.CodeNeedsHuman, "policy routed decision "+d.ID+" to a person; an agent key cannot decide it")
		return
	}
	// The admin key's verdict counts as the one identity "admin"; the name
	// it reports is kept as claimed_by. A bound key decides as its name.
	by, claimed := "admin", r.Header.Get("X-Wayglass-Agent")
	if s, _ := body["by"].(string); s != "" {
		claimed = s
	}
	if caller.key != nil {
		by, claimed = caller.key.Agent, ""
	}
	if it := p.intent(d.IntentID); it != nil && (it.Agent == by || (claimed != "" && it.Agent == claimed)) {
		fail(w, 409, "conflict", "the agent that wrote a change cannot decide it")
		return
	}
	verdict, _ := body["verdict"].(string)
	reason, _ := body["reason"].(string)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	p.approvals[d.ID] = append(p.approvals[d.ID], api.Approval{DecisionID: d.ID, By: by, ClaimedBy: claimed, Verdict: verdict, Reason: reason, At: now})
	distinct := map[string]bool{}
	for _, a := range p.approvals[d.ID] {
		if a.Verdict == "approve" {
			distinct[a.By] = true
		}
	}
	var waiting any
	switch {
	case verdict == "reject":
		d.Status = api.StatusRejected
	case len(distinct) >= max(1, d.ApprovalsNeeded):
		d.Status = api.StatusApproved
	default:
		waiting = fmt.Sprintf("needs another approver: %d of %d distinct approvers so far", len(distinct), d.ApprovalsNeeded)
	}
	if waiting == nil {
		d.DecidedBy, d.DecidedAt = &by, &now
	}
	d.Approvals = p.approvals[d.ID]
	p.log("decision."+verdict, by, d.IntentID, map[string]any{"reason": reason, "status": d.Status, "claimed_by": claimed})
	ok(w, 200, map[string]any{"decision": d, "approvals": p.approvals[d.ID], "waiting_for": waiting})
}

// aiReview mirrors the AI reviewer's route (worker/src/aireview.ts): only an
// agent_review decision waiting for a reviewer, approve or escalate, nothing
// else; a change routed to a person is refused like for an agent key.
func (f *fakeWorker) aiReview(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	if f.aiVerdict == "" {
		fail(w, 503, api.CodeAIUnavailable, "the AI reviewer is off on this Worker (AI_REVIEW=off); a reviewer agent decides agent_review changes with 'wayglass review --for agent'")
		return
	}
	id := r.PathValue("id")
	var d *api.Decision
	for _, x := range p.decisions {
		if x.IntentID == id {
			d = x
		}
	}
	if d == nil {
		fail(w, 409, "conflict", "intent "+id+" has no proposed change to review; run wayglass propose first")
		return
	}
	if d.Status == api.StatusPendingHuman || d.Status == api.StatusQuarantined {
		fail(w, 403, api.CodeNeedsHuman, "policy routed decision "+d.ID+" to a person; the AI reviewer cannot decide it, only a person can")
		return
	}
	if d.Status != api.StatusPendingAgent {
		fail(w, 409, "conflict", "decision "+d.ID+" is "+d.Status+"; only an agent_review decision waiting for a reviewer can be AI-reviewed")
		return
	}
	f.aiReviews++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	gateway := "wayglass"
	review := api.AIVerdict{Verdict: f.aiVerdict, Source: "model", Reviewer: fakeAIReviewer, Provider: "workers-ai", Model: strings.TrimPrefix(fakeAIReviewer, "workers-ai:"), Family: "meta", Gateway: &gateway, LatencyMS: 1200, Tokens: &api.Tokens{Prompt: 321, Completion: 44}}
	by := fakeAIReviewer
	if f.aiVerdict == "approve" {
		review.Reason = "The diff does what the goal says and nothing else."
		d.Status = api.StatusApproved
		d.DecidedBy, d.DecidedAt = &by, &now
		p.approvals[d.ID] = append(p.approvals[d.ID], api.Approval{DecisionID: d.ID, By: by, Family: "meta", Verdict: "approve", Reason: review.Reason, At: now, IdentitySource: "ai"})
		d.Approvals = p.approvals[d.ID]
		p.log("decision.approve", by, d.IntentID, map[string]any{"reason": review.Reason, "status": d.Status, "family": "meta", "identity_source": "ai"})
	} else {
		review.Reason = "The change touches more than the goal says."
		review.Concerns = []string{"scope wider than the goal"}
		d.Detail.Escalation = &api.Escalation{By: by, Reason: review.Reason, Concerns: review.Concerns, FromAction: d.Action, RuleReason: d.Reason,
			Family: "workers-ai", IdentitySource: "ai", At: now, AI: &api.AIMeta{Model: review.Model, Gateway: &gateway, LatencyMS: 1200, Tokens: review.Tokens}}
		d.Action = "human_approve"
		d.Status = api.StatusPendingHuman
		d.Reason = "Escalated by AI reviewer: " + review.Reason
		p.log("decision.escalated", by, d.IntentID, map[string]any{"from_action": "agent_review", "reason": review.Reason, "concerns": review.Concerns, "status": d.Status, "identity_source": "ai", "ai": map[string]any{"model": review.Model}})
	}
	approvals := p.approvals[d.ID]
	if approvals == nil {
		approvals = []api.Approval{}
	}
	ok(w, 200, map[string]any{"intent_id": d.IntentID, "decision": d, "approvals": approvals, "review": review})
}

// aiAdvice is the AI reviewer's advisory route for a change routed to a
// person: it records detail.ai_advice and decides nothing. Off together with
// the reviewer (ai_unavailable); refused for anything not waiting for a person.
func (f *fakeWorker) aiAdvice(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noAIAdvice {
		fail(w, 404, "not_found", "no route for "+r.URL.Path)
		return
	}
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	if f.aiVerdict == "" {
		fail(w, 503, api.CodeAIUnavailable, "the AI reviewer is off on this Worker (AI_REVIEW=off), so there is no AI advice")
		return
	}
	id := r.PathValue("id")
	var d *api.Decision
	for _, x := range p.decisions {
		if x.IntentID == id {
			d = x
		}
	}
	if d == nil {
		fail(w, 409, "conflict", "intent "+id+" has no proposed change to advise on; run wayglass propose first")
		return
	}
	if d.Status == api.StatusPendingAgent {
		fail(w, 409, "conflict", "decision "+d.ID+" is in agent_review: it is reviewed, not advised on")
		return
	}
	if d.Status != api.StatusPendingHuman && d.Status != api.StatusQuarantined {
		fail(w, 409, "conflict", "decision "+d.ID+" is "+d.Status+"; advice is for a change waiting for a person")
		return
	}
	f.aiReviews++
	gateway := "wayglass"
	advice := api.AIAdvice{
		Summary:     "Makes login() return true unconditionally; the goal only says to add a login page.",
		MatchesGoal: "partly",
		Risks:       []string{"authentication bypass: login always succeeds"},
		Questions:   []string{"Is the unconditional true a placeholder?"},
		Warnings:    []string{},
		Source:      "model",
		Reviewer:    fakeAIReviewer,
		Provider:    "workers-ai",
		Model:       strings.TrimPrefix(fakeAIReviewer, "workers-ai:"),
		Family:      "meta",
		Gateway:     &gateway,
		LatencyMS:   900,
		Tokens:      &api.Tokens{Prompt: 410, Completion: 90},
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	stored := advice
	stored.By, stored.At = fakeAIReviewer, now
	d.Detail.AIAdvice = &stored
	p.log("decision.ai_advice", fakeAIReviewer, d.IntentID, map[string]any{"summary": advice.Summary, "matches_goal": advice.MatchesGoal, "status": d.Status, "identity_source": "ai"})
	ok(w, 200, map[string]any{"intent_id": d.IntentID, "decision": d, "advice": advice})
}

func (f *fakeWorker) decisions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.project(w, r, nil)
	if p == nil {
		return
	}
	out := make([]api.Decision, 0, len(p.decisions))
	for _, d := range p.decisions {
		c := *d
		if it := p.intent(d.IntentID); it != nil {
			c.IntentStatus = it.Status
		}
		out = append(out, c)
	}
	ok(w, 200, map[string]any{"decisions": out})
}

// abandon mirrors POST /v1/intents/:id/abandon: author only (the fake has no
// admin identity of its own), children re-stacked onto the intent's base.
func (f *fakeWorker) abandon(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	it := p.intent(r.PathValue("id"))
	switch {
	case it == nil:
		fail(w, 404, "not_found", "intent not found")
		return
	case !slices.Contains(activeStatuses, it.Status):
		fail(w, 409, "conflict", "intent "+it.ID+" is "+it.Status)
		return
	case r.Header.Get("X-Wayglass-Agent") != it.Agent:
		fail(w, 403, "forbidden", "only "+it.Agent+" (the author) or the admin may abandon intent "+it.ID)
		return
	}
	reason, _ := body["reason"].(string)
	by := it.Agent
	it.Status, it.AbandonedBy, it.AbandonedReason = "abandoned", &by, &reason
	restacked := []string{}
	for _, c := range p.intents {
		if deref(c.ParentID) == it.ID && slices.Contains(activeStatuses, c.Status) {
			c.ParentID, c.BaseCommit = it.ParentID, it.BaseCommit
			restacked = append(restacked, c.ID)
		}
	}
	p.log("intent.abandoned", by, it.ID, map[string]any{"reason": reason, "restacked": restacked})
	ok(w, 200, map[string]any{"intent": it, "restacked": restacked, "tokens_revoked": 1, "fork_kept": true})
}

// restack mirrors POST /v1/intents/:id/restack: author only, no cycles,
// parent active with something pushed; the intent moves to a new fork of
// the parent's fork and diffs from its head; the old fork is kept.
func (f *fakeWorker) restack(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noRestack {
		fail(w, 404, "not_found", "no route for "+r.URL.Path)
		return
	}
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	it := p.intent(r.PathValue("id"))
	on, _ := body["on"].(string)
	reason, _ := body["reason"].(string)
	parent := p.intent(on)
	switch {
	case it == nil || parent == nil:
		fail(w, 404, "not_found", "intent not found")
		return
	case placeholderReason.MatchString(strings.TrimSpace(reason)) || fillerReason(reason) || strings.TrimSpace(reason) == "":
		fail(w, 400, "bad_request", fmt.Sprintf("reason %q is not a why", reason))
		return
	case !slices.Contains(activeStatuses, it.Status):
		fail(w, 409, "conflict", "intent "+it.ID+" is "+it.Status)
		return
	case r.Header.Get("X-Wayglass-Agent") != it.Agent:
		fail(w, 403, "forbidden", "only "+it.Agent+" (the author) or the admin may restack intent "+it.ID)
		return
	case !slices.Contains(activeStatuses, parent.Status):
		fail(w, 409, "conflict", "intent "+on+" is "+parent.Status)
		return
	case p.inStack(parent, it) || parent.ID == it.ID:
		fail(w, 409, "conflict", "intent "+on+" is stacked on "+it.ID+"; stacking "+it.ID+" on it would make a cycle")
		return
	}
	head := f.head(parent.ForkRemote, "main")
	if head == "" || head == deref(parent.BaseCommit) {
		fail(w, 409, "conflict", "intent "+on+" has nothing pushed yet; push it before stacking on it")
		return
	}
	f.restacks++
	prev := map[string]any{"parent_id": it.ParentID, "base_commit": it.BaseCommit, "fork_repo": it.ForkRepo, "fork_remote": it.ForkRemote, "fork_kept": true, "tokens_revoked": 1}
	name := fmt.Sprintf("%s--%s--r%d", p.info.Name, it.ID, f.restacks)
	remote := filepath.Join(f.dir, name+".git")
	f.git("clone", "--quiet", "--bare", parent.ForkRemote, remote)
	var superseded *string
	var latest *api.Decision
	for _, d := range p.decisions {
		if d.IntentID == it.ID {
			superseded, latest = &d.ID, d
		}
	}
	// Like the Worker, a pending or approved decision is superseded: nobody
	// may decide or land it. An approval keeps who gave it.
	if latest != nil && slices.Contains([]string{api.StatusPendingAgent, api.StatusPendingHuman, api.StatusQuarantined, api.StatusApproved}, latest.Status) {
		data := map[string]any{"reason": "restacked onto " + on + "; the change must be proposed again", "previous_status": latest.Status}
		if latest.Status == api.StatusApproved {
			data["previous_decided_by"] = deref(latest.DecidedBy)
		}
		latest.Status = api.StatusSuperseded
		p.log("decision.superseded", r.Header.Get("X-Wayglass-Agent"), it.ID, data)
	}
	it.ForkRepo, it.ForkRemote, it.ParentID, it.BaseCommit, it.HeadCommit = name, remote, &parent.ID, &head, nil
	p.log("intent.restacked", r.Header.Get("X-Wayglass-Agent"), it.ID, map[string]any{"reason": reason, "to_parent": on, "base_commit": head, "fork_repo": name})
	ok(w, 201, map[string]any{
		"intent": it, "fork": api.Repo{Name: name, Remote: remote, DefaultBranch: "main"}, "token": f.mint(),
		"previous": prev, "superseded_decision": superseded, "overlaps": p.overlaps(it),
	})
}

// head reads a bare repo's branch tip, "" when it has none.
func (f *fakeWorker) head(remote, branch string) string {
	out, err := (gitx.Git{}).Run(f.t.Context(), "--git-dir="+remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// trunkPolicy names the trunk's .wayglass/policy.yaml at its head by blob id
// ("absent" when there is none), standing in for the Worker's sha256 digest.
func (f *fakeWorker) trunkPolicy(p *fakeProject) string {
	return cmp.Or(f.out("--git-dir="+p.info.TrunkRemote, "rev-parse", "--verify", "--quiet", "refs/heads/main:.wayglass/policy.yaml"), "absent")
}

func (f *fakeWorker) land(w http.ResponseWriter, r *http.Request) {
	refusal := f.landRefusal(r)
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
	if msg := refusal(it); msg != "" {
		fail(w, 403, api.CodeNotAuthor, msg)
		return
	}
	var d *api.Decision
	for _, x := range p.decisions {
		if x.IntentID == it.ID {
			d = x
		}
	}
	if it.Status != "proposed" {
		fail(w, 409, "conflict", "intent "+it.ID+" is "+it.Status)
		return
	}
	if d != nil && d.Status == api.StatusSuperseded {
		fail(w, 409, "conflict", "decision "+d.ID+" was superseded when intent "+it.ID+" was re-stacked; run wayglass propose again so the decision covers the whole change")
		return
	}
	if d == nil || d.Status != "approved" {
		fail(w, 409, "conflict", "decision must be approved before landing")
		return
	}
	if pid := deref(it.ParentID); pid != "" && p.intent(pid).Status != "landed" {
		fail(w, 409, "conflict", "intent "+it.ID+" is stacked on "+pid+", which is "+p.intent(pid).Status+"; land "+pid+" first")
		return
	}
	if h := f.head(it.ForkRemote, "main"); h != *d.CommitSHA {
		fail(w, 409, "conflict", "fork moved after the decision")
		return
	}
	if f.policyChanged || (d.PolicySHA256 != nil && *d.PolicySHA256 != f.trunkPolicy(p)) {
		fail(w, 409, api.CodePolicyChanged, "the trunk's .wayglass/policy.yaml changed after decision "+d.ID+" was made, so it no longer covers the change; nothing was issued. Next: in intent "+it.ID+"'s clone run wayglass sync "+it.ID+" (merges the trunk in), then wayglass push and wayglass propose again so the change is decided under the current policy")
		return
	}
	for other := range p.lands {
		fail(w, 409, "conflict", "intent "+other+" is landing; try again after it finishes")
		return
	}
	before := f.head(p.info.TrunkRemote, "main")
	p.lands[it.ID] = &fakeLand{trunkBefore: before}
	now := time.Now().UTC()
	ok(w, 201, map[string]any{
		"trunk_before": before,
		"land":         api.Land{ID: f.nextID("l"), IntentID: it.ID, DecisionID: d.ID, Status: "issued", ExpiresAt: now.Add(5 * time.Minute).Format(time.RFC3339)},
		"decision":     d,
		"trunk":        map[string]string{"repo": p.info.TrunkRepo, "remote": p.info.TrunkRemote},
		"fork":         map[string]string{"repo": it.ForkRepo, "remote": it.ForkRemote, "commit": *d.CommitSHA},
		"token":        f.mint(),
	})
}

func (f *fakeWorker) landed(w http.ResponseWriter, r *http.Request) {
	refusal := f.landRefusal(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	it := p.intent(r.PathValue("id"))
	commit, _ := body["commit"].(string)
	if it == nil || p.lands[it.ID] == nil {
		fail(w, 409, "conflict", "no land in progress")
		return
	}
	if msg := refusal(it); msg != "" {
		fail(w, 403, api.CodeNotAuthor, msg)
		return
	}
	land := p.lands[it.ID]
	delete(p.lands, it.ID)
	var d *api.Decision
	for _, x := range p.decisions {
		if x.IntentID == it.ID {
			d = x
		}
	}
	if problem := f.verifyLanded(p.info.TrunkRemote, commit, *d.CommitSHA, d.Paths, land.trunkBefore); problem != "" {
		p.log("land.mismatch", r.Header.Get("X-Wayglass-Agent"), it.ID, map[string]any{"commit": commit, "problems": []string{problem}})
		fail(w, 409, "conflict", "trunk commit "+commit+" is not the approved change: "+problem)
		return
	}
	p.log("intent.landed", r.Header.Get("X-Wayglass-Agent"), it.ID, map[string]any{"commit": commit})
	d.LandedCommit = &commit
	it.Status = "landed"
	ok(w, 200, map[string]any{"intent": it, "decision": d, "land": api.Land{Status: "landed", LandedCommit: &commit}, "token_revoked": true})
}

// verifyLanded mirrors worker/src/index.ts verifyLanded with git: trunk is
// the approved head descending from trunkBefore, or a two-parent merge of it
// directly on trunkBefore that changes only decided paths.
func (f *fakeWorker) verifyLanded(trunk, commit, approved string, paths []string, before string) string {
	gd := "--git-dir=" + trunk
	if f.head(trunk, "main") != commit {
		return "trunk head is not " + commit
	}
	if commit == approved {
		if before != "" {
			if _, err := (gitx.Git{}).Run(f.t.Context(), gd, "merge-base", "--is-ancestor", before, approved); err != nil {
				return "fast-forward drops trunk commit " + before
			}
			for _, changed := range strings.Fields(f.out(gd, "diff", "--name-only", "--no-renames", before, commit)) {
				if len(paths) > 0 && !slices.Contains(paths, changed) {
					return "fast-forward changes paths outside the decision: " + changed
				}
			}
		}
		return ""
	}
	parents := strings.Fields(f.out(gd, "log", "-1", "--format=%P", commit))
	if len(parents) != 2 || parents[1] != approved {
		return "commit is neither " + approved + " nor a merge of it into trunk"
	}
	if before != "" && parents[0] != before {
		return "trunk moved during the land"
	}
	for _, changed := range strings.Fields(f.out(gd, "diff", "--name-only", "--no-renames", parents[0], commit)) {
		if len(paths) > 0 && !slices.Contains(paths, changed) {
			return "merge changes paths outside the decision: " + changed
		}
	}
	return ""
}

func (f *fakeWorker) abort(w http.ResponseWriter, r *http.Request) {
	refusal := f.landRefusal(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	body := decode(r)
	p := f.project(w, r, body)
	if p == nil {
		return
	}
	id := r.PathValue("id")
	if p.lands[id] == nil {
		fail(w, 409, "conflict", "intent "+id+" has no land in progress")
		return
	}
	if msg := refusal(p.intent(id)); msg != "" {
		fail(w, 403, api.CodeNotAuthor, msg)
		return
	}
	delete(p.lands, id)
	f.aborts++
	p.log("land.aborted", r.Header.Get("X-Wayglass-Agent"), id, map[string]any{"reason": body["reason"]})
	ok(w, 200, map[string]any{"land": api.Land{IntentID: id, Status: "cancelled"}, "token_revoked": true})
}

func (f *fakeWorker) auditLog(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.project(w, r, nil)
	if p == nil {
		return
	}
	since, limit := 0, 100
	_, _ = fmt.Sscan(cmp.Or(r.URL.Query().Get("since"), "0"), &since)
	_, _ = fmt.Sscan(cmp.Or(r.URL.Query().Get("limit"), "100"), &limit)
	events := []api.AuditEvent{}
	for _, e := range p.audit {
		if e.Seq > since && len(events) < limit {
			events = append(events, e)
		}
	}
	next := since
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	ok(w, 200, map[string]any{"events": events, "next": next})
}

func (f *fakeWorker) why(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.project(w, r, nil)
	if p == nil {
		return
	}
	c := r.URL.Query().Get("commit")
	for _, d := range slices.Backward(p.decisions) {
		if d.CommitSHA != nil && strings.HasPrefix(*d.CommitSHA, c) {
			approvals := p.approvals[d.ID]
			if approvals == nil {
				approvals = []api.Approval{}
			}
			ok(w, 200, map[string]any{"commit": *d.CommitSHA, "intent": p.intent(d.IntentID), "decision": d, "approvals": approvals})
			return
		}
	}
	fail(w, 404, "not_found", "no decision recorded for commit "+c)
}
