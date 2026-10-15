package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
)

const testKey = "test-admin-key"

// fakeWorker serves the four routes the app reads and decides through,
// with the Worker's own refusals (worker/src/index.ts, project.ts).
type fakeWorker struct {
	mu        sync.Mutex
	project   string
	oldWorker bool // no GET /v1/decisions, like a Worker deployed before it
	agentKey  bool // signed in with an agent key: the Worker refuses human-routed decisions
	// failIntents makes GET /v1/intents fail, like a Worker that is half up.
	failIntents bool
	// refuse, when set, is the code and message every decide gets: a
	// decision that changed between the window's poll and the click.
	refuse [2]string
	// aiVerdict is what the fake AI reviewer answers on ai-review: "" (the
	// reviewer is off: ai_unavailable), "approve" or "escalate". ai-advice is
	// off with it. noAIAdvice simulates a Worker without the advice route.
	aiVerdict  string
	noAIAdvice bool
	asks       int
	intents    []api.Intent
	decisions  []api.Decision
	events     []api.AuditEvent
	hits       map[string]int
	decided    []decideCall
}

type decideCall struct {
	Agent string
	Body  map[string]any
}

var placeholder = regexp.MustCompile(`(?i)^<[^<>]*>$|^(todo|tbd|n/a|none|-|\.+)$`)

const (
	fakeAIModel = "@cf/meta/llama-4-scout-17b-16e-instruct"
	fakeAI      = "workers-ai:" + fakeAIModel
)

func ptr(s string) *string { return &s }

func newFake(t *testing.T) (*fakeWorker, *httptest.Server) {
	t.Helper()
	f := &fakeWorker{project: "demo", hits: map[string]int{}}
	f.intents = []api.Intent{
		{ID: "i0000000001", Agent: "claude-a", Goal: "Docs typo", Reason: "readers", DeclaredPaths: []string{"docs/**"}, Status: "landed", CreatedAt: "2026-10-13T10:00:00Z", UpdatedAt: "2026-10-13T10:05:00Z"},
		{ID: "i0000000002", Agent: "claude-b", Goal: "Add login rate limit", Reason: "brute force seen", DeclaredPaths: []string{"web/login.ts"}, ActualPaths: []string{"web/login.ts"}, Status: "proposed", CreatedAt: "2026-10-13T11:00:00Z", UpdatedAt: "2026-10-13T11:10:00Z"},
		{ID: "i0000000003", Agent: "john", Goal: "Rotate session cookie", Reason: "audit finding", DeclaredPaths: []string{"web/**"}, Status: "proposed", CreatedAt: "2026-10-13T11:30:00Z", UpdatedAt: "2026-10-13T11:40:00Z"},
		{ID: "i0000000004", Agent: "codex-c", Goal: "Refactor queue consumer", Reason: "simpler retries", DeclaredPaths: []string{"worker/src/queue.ts"}, Status: "proposed", CreatedAt: "2026-10-13T12:00:00Z", UpdatedAt: "2026-10-13T12:01:00Z"},
	}
	f.decisions = []api.Decision{
		{ID: "d01", IntentID: "i0000000001", Action: "auto_merge", Status: "approved", Reason: "docs only", Tags: json.RawMessage(`{"areas":["docs"],"data_classes":[],"risk":"low","jurisdiction":"us"}`), CreatedAt: "2026-10-13T10:04:00Z"},
		{ID: "d02", IntentID: "i0000000002", Action: "human_approve", Status: "pending_human", MatchedRule: ptr("rules[1]"), Reason: "auth changes need a person",
			Tags:   json.RawMessage(`{"areas":["auth"],"data_classes":["auth"],"risk":"high","jurisdiction":"us","evidence":[{"tag":"area","value":"auth","path":"web/login.ts"}]}`),
			Detail: api.DecisionDetail{Files: []api.FileStat{{Path: "web/login.ts", Added: 12, Deleted: 3}}}, CreatedAt: "2026-10-13T11:10:00Z"},
		{ID: "d03", IntentID: "i0000000003", Action: "quarantine", Status: "quarantined", Reason: "PII", Tags: json.RawMessage(`{"areas":["web"],"data_classes":["pii"],"risk":"medium","jurisdiction":"eu"}`), Paths: []string{"web/session.ts"}, CreatedAt: "2026-10-13T11:40:00Z"},
		{ID: "d04", IntentID: "i0000000004", Action: "agent_review", Status: "pending_agent", Reason: "worker code", Tags: json.RawMessage(`{"areas":["worker"],"data_classes":[],"risk":"medium","jurisdiction":"us"}`), CreatedAt: "2026-10-13T12:01:00Z"},
	}
	f.events = []api.AuditEvent{
		{Seq: 1, Kind: "intent.declared", Actor: "claude-a", IntentID: ptr("i0000000001"), Data: json.RawMessage(`{"goal":"Docs typo"}`), At: "2026-10-13T10:00:00Z"},
		{Seq: 2, Kind: "decision.proposed", Actor: "claude-b", Data: json.RawMessage(`{"action":"human_approve","matched_rule":"rules[1]","reason":"auth"}`), At: "2026-10-13T11:10:00Z"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects", f.projects)
	mux.HandleFunc("GET /v1/intents", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w, map[string]any{"intents": f.intents})
	})
	mux.HandleFunc("GET /v1/decisions", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w, map[string]any{"decisions": f.decisions})
	})
	mux.HandleFunc("GET /v1/audit", func(w http.ResponseWriter, r *http.Request) {
		var since int
		_ = json.Unmarshal([]byte(r.URL.Query().Get("since")), &since)
		out := []api.AuditEvent{}
		next := since
		for _, e := range f.events {
			if e.Seq > since {
				out = append(out, e)
				next = e.Seq
			}
		}
		writeOK(w, map[string]any{"events": out, "next": next})
	})
	mux.HandleFunc("POST /v1/intents/{id}/ai-review", func(w http.ResponseWriter, r *http.Request) {
		if f.aiVerdict == "" {
			writeFail(w, 503, "ai_unavailable", "the AI reviewer is off on this Worker (AI_REVIEW=off); a reviewer agent decides agent_review changes with 'wayglass review --for agent'")
			return
		}
		id := r.PathValue("id")
		for i := range f.decisions {
			d := &f.decisions[i]
			if d.IntentID != id {
				continue
			}
			if d.Status == "pending_human" || d.Status == "quarantined" {
				writeFail(w, 403, "needs_human", "policy routed decision "+d.ID+" to a person; the AI reviewer cannot decide it, only a person can")
				return
			}
			if d.Status != "pending_agent" {
				writeFail(w, 409, "conflict", "decision "+d.ID+" is "+d.Status)
				return
			}
			f.asks++
			by := fakeAI
			review := map[string]any{"verdict": f.aiVerdict, "source": "model", "model": fakeAIModel, "latency_ms": 900, "concerns": []string{}}
			if f.aiVerdict == "approve" {
				review["reason"] = "The diff does what the goal says and nothing else."
				d.Status, d.DecidedBy = "approved", &by
				d.Approvals = append(d.Approvals, api.Approval{DecisionID: d.ID, By: by, Family: "workers-ai", Verdict: "approve", Reason: "The diff does what the goal says and nothing else.", IdentitySource: "ai"})
			} else {
				review["reason"] = "The change touches more than the goal says."
				review["concerns"] = []string{"scope wider than the goal"}
				d.Detail.Escalation = &api.Escalation{By: by, Reason: "The change touches more than the goal says.", Concerns: []string{"scope wider than the goal"}, FromAction: d.Action, IdentitySource: "ai", AI: &api.AIMeta{Model: fakeAIModel}}
				d.Action, d.Status = "human_approve", "pending_human"
			}
			f.events = append(f.events, api.AuditEvent{Seq: len(f.events) + 1, Kind: "decision." + map[string]string{"approve": "approve", "escalate": "escalated"}[f.aiVerdict], Actor: by, IntentID: ptr(id), Data: json.RawMessage(`{"reason":"` + review["reason"].(string) + `","identity_source":"ai","concerns":["scope wider than the goal"]}`), At: "2026-10-14T12:00:00Z"})
			approvals := d.Approvals
			if approvals == nil {
				approvals = []api.Approval{}
			}
			writeOK(w, map[string]any{"intent_id": id, "decision": d, "approvals": approvals, "review": review})
			return
		}
		writeFail(w, 409, "conflict", "no proposed change")
	})
	mux.HandleFunc("POST /v1/intents/{id}/ai-advice", func(w http.ResponseWriter, r *http.Request) {
		if f.noAIAdvice {
			writeFail(w, 404, "not_found", "no route for "+r.URL.Path)
			return
		}
		if f.aiVerdict == "" {
			writeFail(w, 503, "ai_unavailable", "the AI reviewer is off on this Worker (AI_REVIEW=off)")
			return
		}
		id := r.PathValue("id")
		for i := range f.decisions {
			d := &f.decisions[i]
			if d.IntentID != id {
				continue
			}
			if d.Status != "pending_human" && d.Status != "quarantined" {
				writeFail(w, 409, "conflict", "decision "+d.ID+" is "+d.Status+"; the AI advises only on changes waiting for a person")
				return
			}
			f.asks++
			adv := api.AIAdvice{Summary: "Within its declared paths; the rule sends it to a person because of what it touches.\x1b[2J", Risks: []string{"No test covers the lockout reset"}, Questions: []string{"Is there a rollback?"}, By: fakeAI, Model: fakeAIModel}
			d.Detail.AIAdvice = &adv
			f.events = append(f.events, api.AuditEvent{Seq: len(f.events) + 1, Kind: "decision.ai_advice", Actor: fakeAI, IntentID: ptr(id), Data: json.RawMessage(`{"summary":"Within its declared paths"}`), At: "2026-10-14T12:00:00Z"})
			writeOK(w, map[string]any{"intent_id": id, "decision": d, "advice": adv})
			return
		}
		writeFail(w, 409, "conflict", "no proposed change")
	})
	mux.HandleFunc("POST /v1/intents/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		agent := r.Header.Get("X-Wayglass-Agent")
		f.decided = append(f.decided, decideCall{Agent: agent, Body: body})
		reason, _ := body["reason"].(string)
		if placeholder.MatchString(reason) {
			writeFail(w, 400, "bad_request", `reason looks like a placeholder ("`+reason+`"); say what you checked or why`)
			return
		}
		if f.refuse[0] != "" {
			writeFail(w, 409, f.refuse[0], f.refuse[1])
			return
		}
		if f.agentKey {
			writeFail(w, 403, "needs_human", "policy routed decision d02 to a person (human_approve, rules[1]); an agent key cannot decide it. A person decides with the admin key or a key minted with 'wayglass keys create --human'")
			return
		}
		id := r.PathValue("id")
		for i := range f.intents {
			if f.intents[i].ID == id && f.intents[i].Agent == agent {
				writeFail(w, 409, "conflict", "the agent that wrote a change cannot decide it")
				return
			}
		}
		for i := range f.decisions {
			d := &f.decisions[i]
			if d.IntentID != id {
				continue
			}
			d.Status = map[string]string{"approve": "approved", "reject": "rejected"}[body["verdict"].(string)]
			d.DecidedBy = &agent
			d.Approvals = append(d.Approvals, api.Approval{DecisionID: d.ID, By: agent, Verdict: body["verdict"].(string), Reason: reason})
			f.events = append(f.events, api.AuditEvent{Seq: len(f.events) + 1, Kind: "decision." + body["verdict"].(string), Actor: agent, Data: json.RawMessage(`{}`)})
			writeOK(w, map[string]any{"decision": d, "approvals": d.Approvals})
			return
		}
		writeFail(w, 409, "conflict", "no proposed change")
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits[r.Method+" "+r.URL.Path]++
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			writeFail(w, 401, "unauthorized", "invalid bearer token")
			return
		}
		if f.oldWorker && r.URL.Path == "/v1/decisions" {
			writeFail(w, 404, "not_found", "no route for /v1/decisions")
			return
		}
		if f.failIntents && r.URL.Path == "/v1/intents" {
			writeFail(w, 500, "internal", "intents are unavailable")
			return
		}
		if p := r.URL.Query().Get("project"); r.Method == http.MethodGet && r.URL.Path != "/v1/projects" && p != f.project {
			writeFail(w, 404, "not_found", "project "+p+" not found")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func writeOK(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ok": true, "data": data})
}

func writeFail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ok": false, "error": map[string]string{"code": code, "message": msg}})
}

func (f *fakeWorker) hit(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[route]
}

// fakeNative records what the app asked of the window.
type fakeNative struct {
	mu       sync.Mutex
	emits    map[string]int
	notes    []string
	titles   []string
	snapshot Snapshot
	poll     PollStatus
	opened   []string
	copied   []string
}

func (n *fakeNative) Emit(event string, data ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.emits == nil {
		n.emits = map[string]int{}
	}
	n.emits[event]++
	switch v := data[0].(type) {
	case Snapshot:
		n.snapshot = v
	case PollStatus:
		n.poll = v
	}
}

func (n *fakeNative) Notify(title, body string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notes = append(n.notes, title+": "+body)
	return nil
}

func (n *fakeNative) SetTitle(title string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.titles = append(n.titles, title)
}

func (n *fakeNative) OpenURL(url string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.opened = append(n.opened, url)
}

func newApp(t *testing.T, srv *httptest.Server, agent string, n Native) (*App, string) {
	t.Helper()
	t.Setenv("WAYGLASS_PROJECT", "")
	path := filepath.Join(t.TempDir(), "desktop.yaml")
	if err := savePrefs(path, prefs{Projects: []string{"demo"}, Current: "demo"}); err != nil {
		t.Fatal(err)
	}
	a := New(Options{
		Creds:     api.Credentials{URL: srv.URL, Key: testKey, Agent: agent},
		PrefsPath: path, Interval: 10 * time.Millisecond, HTTP: srv.Client(), Native: n,
	})
	return a, path
}

func TestNewSession(t *testing.T) {
	t.Setenv("WAYGLASS_PROJECT", "")
	def := api.DefaultAgent() // user@host, the same default as the CLI
	tests := []struct {
		name     string
		creds    api.Credentials
		loggedIn bool
		identity string
		problem  string
	}{
		{"no login", api.Credentials{}, false, def, "Not logged in"},
		{"plain http to a remote host", api.Credentials{URL: "http://example.com", Key: "k"}, false, def, "must use https"},
		{"login, default identity", api.Credentials{URL: "https://api.example.dev", Key: "k"}, true, def, ""},
		{"identity from login or WAYGLASS_AGENT", api.Credentials{URL: "https://api.example.dev", Key: "k", Agent: "alice"}, true, "alice", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Options{Creds: tt.creds})
			s := a.Session()
			if s.LoggedIn != tt.loggedIn || s.Identity != tt.identity || !strings.Contains(s.Problem, tt.problem) {
				t.Fatalf("session = %+v", s)
			}
			if s.Projects == nil {
				t.Fatal("projects must be [] not null")
			}
		})
	}
}

func TestSnapshot(t *testing.T) {
	_, srv := newFake(t)
	a, _ := newApp(t, srv, "john", &fakeNative{})
	s, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c := s.Counters
	if c.Agents != 4 || c.ActiveAgents != 3 || c.Intents != 4 || c.OpenIntents != 3 || c.Landed != 1 || c.Decisions != 4 {
		t.Errorf("counters = %+v", c)
	}
	if len(c.ByAction) != 6 || c.ByAction["auto_merge"] != 1 || c.ByAction["quarantine"] != 1 || c.ByAction["block"] != 0 {
		t.Errorf("by action = %v", c.ByAction)
	}
	// Two changes wait for a person; one is john's own, so only one is his to decide.
	if len(s.NeedsYou) != 2 || c.NeedsYou != 1 {
		t.Fatalf("needs you = %d items, count %d", len(s.NeedsYou), c.NeedsYou)
	}
	first := s.NeedsYou[0]
	if first.Intent.ID != "i0000000002" || first.Own || first.Decision.MatchedRule != "rules[1]" ||
		first.Decision.Added != 12 || first.Decision.Deleted != 3 || first.Decision.Tags.Risk != "high" ||
		len(first.Decision.Tags.Evidence) != 1 {
		t.Errorf("first needs-you = %+v", first)
	}
	if !s.NeedsYou[1].Own || s.NeedsYou[1].Decision.Files[0].Path != "web/session.ts" {
		t.Errorf("second needs-you = %+v", s.NeedsYou[1])
	}
	// web/login.ts sits inside web/**: the older intent lands first.
	if len(s.Overlaps) != 1 || s.Overlaps[0].Older != "i0000000002" || s.Overlaps[0].Newer != "i0000000003" {
		t.Errorf("overlaps = %+v", s.Overlaps)
	}
	if s.Intents[0].Status == "landed" || s.Intents[len(s.Intents)-1].Status != "landed" {
		t.Errorf("active intents should sort first: %v", s.Intents)
	}
	if len(s.Events) != 2 || s.Events[0].Seq != 2 || !strings.Contains(s.Events[0].Summary, "human_approve by rules[1]") {
		t.Errorf("events = %+v", s.Events)
	}
}

func TestOldWorkerWithoutDecisions(t *testing.T) {
	f, srv := newFake(t)
	f.oldWorker = true
	a, _ := newApp(t, srv, "john", &fakeNative{})
	s, err := a.Snapshot()
	if err != nil {
		t.Fatalf("a missing decisions route must degrade, not fail: %v", err)
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "predates GET /v1/decisions") {
		t.Errorf("warnings = %v", s.Warnings)
	}
	if s.Counters.Intents != 4 || len(s.Overlaps) != 1 || len(s.Events) != 2 || len(s.NeedsYou) != 0 || s.Counters.Decisions != 0 {
		t.Errorf("snapshot = %+v", s.Counters)
	}
	got := a.Approve("i0000000002", "", "checked it")
	if got.OK || got.Code != "not_supported" || f.hit("POST /v1/intents/i0000000002/decide") != 0 {
		t.Errorf("approve on an old Worker = %+v", got)
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name     string
		identity string
		intent   string
		verdict  string
		reason   string
		wantOK   bool
		wantCode string
		wantMsg  string
		requests int // decide requests that reach the Worker
	}{
		{"no reason", "john", "i0000000002", "approve", "   ", false, "reason_required", "Write why", 0},
		{"own change", "john", "i0000000003", "approve", "looks right", false, "self_approval", "cannot decide it", 0},
		{"own change, other case", "JOHN", "i0000000003", "reject", "looks right", false, "self_approval", "", 0},
		{"not waiting for a person", "john", "i0000000004", "approve", "fine", false, "not_waiting", "pending agent", 0},
		{"landed", "john", "i0000000001", "approve", "fine", false, "not_waiting", "approved", 0},
		{"unknown intent", "john", "i00000000ff", "approve", "fine", false, "not_found", "", 0},
		{"placeholder is the Worker's call", "john", "i0000000002", "approve", "<what you checked>", false, "bad_request", "looks like a placeholder", 1},
		{"approve", "john", "i0000000002", "approve", "Checked the rate limit window and the tests", true, "", "", 1},
		{"reject", "john", "i0000000002", "reject", "Locks out shared office IPs", true, "", "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFake(t)
			a, _ := newApp(t, srv, tt.identity, &fakeNative{})
			var got DecideOutcome
			if tt.verdict == "approve" {
				got = a.Approve(tt.intent, "", tt.reason)
			} else {
				got = a.Reject(tt.intent, "", tt.reason)
			}
			if got.OK != tt.wantOK || got.Code != tt.wantCode || !strings.Contains(got.Message, tt.wantMsg) {
				t.Fatalf("outcome = %+v", got)
			}
			if n := f.hit("POST /v1/intents/" + tt.intent + "/decide"); n != tt.requests {
				t.Fatalf("decide requests = %d, want %d", n, tt.requests)
			}
			if !tt.wantOK {
				return
			}
			call := f.decided[0]
			if call.Agent != tt.identity || call.Body["verdict"] != tt.verdict || call.Body["project"] != "demo" || call.Body["by"] != nil {
				t.Errorf("request = %+v", call)
			}
			// The verdict names the decision the window showed, so a proposal
			// made while the person read it is refused by the Worker, not approved.
			if id, _ := call.Body["decision_id"].(string); !strings.HasPrefix(id, "d") {
				t.Errorf("decision_id = %v, want the decision shown", call.Body["decision_id"])
			}
			want := map[string]string{"approve": "approved", "reject": "rejected"}[tt.verdict]
			if got.Decision == nil || got.Decision.Status != want || got.Decision.Approvals[0].By != tt.identity {
				t.Errorf("decision = %+v", got.Decision)
			}
		})
	}
}

// The card the person read showed an earlier decision; the author proposed
// again since. The app refuses before any request: the new proposal must be
// read first.
func TestDecideRefusesReplacedDecision(t *testing.T) {
	f, srv := newFake(t)
	a, _ := newApp(t, srv, "john", &fakeNative{})
	got := a.Approve("i0000000002", "d0000000bad", "Checked the rate limit window and the tests")
	if got.OK || got.Code != api.CodeDecisionReplaced || !strings.Contains(got.Message, "proposed this change again") {
		t.Fatalf("outcome = %+v", got)
	}
	if n := f.hit("POST /v1/intents/i0000000002/decide"); n != 0 {
		t.Fatalf("decide requests = %d, want none", n)
	}
}

// The app's own decision_replaced refusal promises "the new proposal is
// shown now", so it must push a snapshot with the new decision instead of
// leaving the stale card until the next poll.
func TestReplacedDecisionRefusalRefreshes(t *testing.T) {
	f, srv := newFake(t)
	n := &fakeNative{}
	a, _ := newApp(t, srv, "john", n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startup(ctx)
	waitFor(t, "first snapshot", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventSnapshot] > 0
	})
	a.shutdown(ctx) // stop the poller so the next snapshot is the refusal's
	// The author proposed again: a newer decision for the same intent.
	f.mu.Lock()
	f.decisions = append(f.decisions, api.Decision{ID: "d0000000new", IntentID: "i0000000002", Action: "human_approve", Status: api.StatusPendingHuman, Reason: "auth", CommitSHA: ptr("6130391db48505c7b9db7a0d833679c6e4e7c04d"), CreatedAt: "2026-10-13T12:30:00Z"})
	f.events = append(f.events, api.AuditEvent{Seq: 3, Kind: "decision.proposed", IntentID: ptr("i0000000002"), Data: json.RawMessage(`{}`)})
	f.mu.Unlock()
	n.mu.Lock()
	before := n.emits[EventSnapshot]
	n.mu.Unlock()
	got := a.Approve("i0000000002", "d02", "Checked the rate limit window and the tests")
	if got.OK || got.Code != api.CodeDecisionReplaced {
		t.Fatalf("outcome = %+v", got)
	}
	if n := f.hit("POST /v1/intents/i0000000002/decide"); n != 0 {
		t.Fatalf("decide requests = %d, want none", n)
	}
	waitFor(t, "a refresh after the refusal", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventSnapshot] > before
	})
	n.mu.Lock()
	defer n.mu.Unlock()
	var ids []string
	for _, x := range n.snapshot.NeedsYou {
		if x.Intent.ID == "i0000000002" {
			ids = append(ids, x.Decision.ID)
		}
	}
	if len(ids) != 1 || ids[0] != "d0000000new" {
		t.Errorf("needs-you decisions for i0000000002 = %v, want the new one only", ids)
	}
}

// Signed in with an agent key, the app shows the Worker's needs_human
// refusal as-is and records nothing.
func TestDecideWithAgentKey(t *testing.T) {
	f, srv := newFake(t)
	f.agentKey = true
	a, _ := newApp(t, srv, "reviewer-bot", &fakeNative{})
	got := a.Approve("i0000000002", "", "Checked the rate limit window and the tests")
	if got.OK || got.Code != "needs_human" || !strings.Contains(got.Message, "keys create --human") {
		t.Fatalf("outcome = %+v", got)
	}
	if f.decisions[1].Status != "pending_human" {
		t.Fatalf("status = %s", f.decisions[1].Status)
	}
}

// A decision superseded by a restack leaves Needs you and cannot be decided;
// the app says why without asking the Worker.
func TestSupersededDecision(t *testing.T) {
	f, srv := newFake(t)
	f.decisions[1].Status = api.StatusSuperseded
	a, _ := newApp(t, srv, "reviewer", &fakeNative{})
	s, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range s.NeedsYou {
		if n.Intent.ID == "i0000000002" {
			t.Fatalf("superseded decision still in Needs you: %+v", n)
		}
	}
	got := a.Approve("i0000000002", "", "Checked the rate limit window and the tests")
	if got.OK || got.Code != api.CodeSuperseded || !strings.Contains(got.Message, "proposes again") {
		t.Fatalf("outcome = %+v", got)
	}
	if len(f.decided) != 0 {
		t.Fatalf("the Worker was asked to decide: %+v", f.decided)
	}
}

func TestProjects(t *testing.T) {
	_, srv := newFake(t)
	a, path := newApp(t, srv, "john", &fakeNative{})
	if _, err := a.AddProject("Not A Name!"); err == nil {
		t.Error("an invalid name was accepted")
	}
	if _, err := a.AddProject("nope"); err == nil || !strings.Contains(err.Error(), "wayglass init") {
		t.Errorf("unknown project: %v", err)
	}
	if _, err := a.SelectProject("nope"); err == nil {
		t.Error("selected a project not in the list")
	}
	s, err := a.AddProject("  demo ")
	if err != nil || s.Current != "demo" || len(s.Projects) != 1 {
		t.Fatalf("add = %+v, %v", s, err)
	}
	if s, _ = a.SetNotifications(false); s.Notifications {
		t.Error("notifications still on")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testKey) || !strings.Contains(string(b), "demo") {
		t.Errorf("desktop.yaml = %q", b)
	}
	// Windows has no POSIX permission bits (Go reports 0666 for every file).
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("desktop.yaml mode = %v", st.Mode())
	}
	if s, _ = a.RemoveProject("demo"); s.Current != "" || len(s.Projects) != 0 {
		t.Errorf("remove = %+v", s)
	}
	reopened := New(Options{Creds: api.Credentials{URL: srv.URL, Key: testKey}, PrefsPath: path})
	if s := reopened.Session(); len(s.Projects) != 0 || s.Notifications {
		t.Errorf("reloaded = %+v", s)
	}
}

func TestDisplayName(t *testing.T) {
	f, srv := newFake(t)
	a, path := newApp(t, srv, "agent-cli", &fakeNative{})
	if s := a.Session(); s.Identity != "agent-cli" || s.DefaultIdentity != "agent-cli" || s.DisplayName != "" {
		t.Fatalf("start = %+v", s)
	}
	for _, bad := range []string{strings.Repeat("x", 129), "two\nlines"} {
		if _, err := a.SetDisplayName(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	s, err := a.SetDisplayName("  Ada Reviewer ")
	if err != nil || s.Identity != "Ada Reviewer" || s.DisplayName != "Ada Reviewer" || s.DefaultIdentity != "agent-cli" {
		t.Fatalf("set = %+v, %v", s, err)
	}
	// Approvals go out under the new name.
	if out := a.Approve("i0000000002", "", "Checked the rate limit window and the tests"); !out.OK {
		t.Fatalf("approve = %+v", out)
	}
	f.mu.Lock()
	got := f.decided[len(f.decided)-1].Agent
	f.mu.Unlock()
	if got != "Ada Reviewer" {
		t.Errorf("X-Wayglass-Agent = %q", got)
	}
	// It lives in desktop.yaml, not the credentials file, and survives a restart.
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "display_name: Ada Reviewer") {
		t.Errorf("desktop.yaml = %q", b)
	}
	reopened := New(Options{Creds: api.Credentials{URL: srv.URL, Key: testKey, Agent: "agent-cli"}, PrefsPath: path})
	if s := reopened.Session(); s.Identity != "Ada Reviewer" {
		t.Errorf("reloaded = %+v", s)
	}
	// Clearing it goes back to the CLI's identity.
	if s, _ := a.SetDisplayName(""); s.Identity != "agent-cli" || s.DisplayName != "" {
		t.Errorf("cleared = %+v", s)
	}
}

func TestDiscoverProjects(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".wayglass"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".wayglass", "config.yaml"), []byte("version: 1\nproject: from-repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYGLASS_PROJECT", "from-env")
	if got := discoverProjects(sub); !slices.Equal(got, []string{"from-env", "from-repo"}) {
		t.Errorf("discovered %v", got)
	}
}

func TestIdlePollReadsOnlyTheAudit(t *testing.T) {
	f, srv := newFake(t)
	a, _ := newApp(t, srv, "john", &fakeNative{})
	ctx := context.Background()
	if _, changed, err := a.refresh(ctx, false); err != nil || !changed {
		t.Fatalf("first refresh: changed=%v err=%v", changed, err)
	}
	if _, changed, err := a.refresh(ctx, false); err != nil || changed {
		t.Fatalf("idle refresh: changed=%v err=%v", changed, err)
	}
	if n := f.hit("GET /v1/intents"); n != 1 {
		t.Errorf("intents read %d times while idle", n)
	}
	if n := f.hit("GET /v1/audit"); n != 2 {
		t.Errorf("audit read %d times", n)
	}
}

func TestPollerNotifiesOncePerNewChange(t *testing.T) {
	f, srv := newFake(t)
	n := &fakeNative{}
	a, _ := newApp(t, srv, "john", n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startup(ctx)
	defer a.shutdown(ctx)

	waitFor(t, "first snapshot", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventSnapshot] > 0 && len(n.titles) > 0
	})
	n.mu.Lock()
	if len(n.notes) != 0 || n.titles[len(n.titles)-1] != "Wayglass · 1 needs you" {
		t.Errorf("on open: notes=%v titles=%v", n.notes, n.titles)
	}
	n.mu.Unlock()

	f.mu.Lock()
	f.intents = append(f.intents, api.Intent{ID: "i0000000005", Agent: "codex-d", Goal: "Store user emails", DeclaredPaths: []string{"db/**"}, Status: "proposed", CreatedAt: "2026-10-13T13:00:00Z"})
	f.decisions = append(f.decisions, api.Decision{ID: "d05", IntentID: "i0000000005", Action: "quarantine", Status: "quarantined", Reason: "PII", CreatedAt: "2026-10-13T13:01:00Z"})
	f.events = append(f.events, api.AuditEvent{Seq: 3, Kind: "decision.proposed", Data: json.RawMessage(`{}`)})
	f.mu.Unlock()

	waitFor(t, "notification", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return len(n.notes) == 1
	})
	time.Sleep(50 * time.Millisecond) // a few more polls must not ring again
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.notes) != 1 || !strings.Contains(n.notes[0], "Quarantined") || !strings.Contains(n.notes[0], "Store user emails") {
		t.Errorf("notes = %v", n.notes)
	}
	if n.titles[len(n.titles)-1] != "Wayglass · 2 need you" {
		t.Errorf("title = %q", n.titles[len(n.titles)-1])
	}
}

func TestPollerReportsErrors(t *testing.T) {
	_, srv := newFake(t)
	n := &fakeNative{}
	a := New(Options{Creds: api.Credentials{URL: srv.URL, Key: "wrong"}, HTTP: srv.Client(), Interval: 10 * time.Millisecond, Native: n})
	a.prefs = prefs{Projects: []string{"demo"}, Current: "demo"}
	a.syncSession()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startup(ctx)
	defer a.shutdown(ctx)
	waitFor(t, "a failed poll", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventPoll] > 0
	})
	if _, err := a.Snapshot(); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("snapshot with a bad key: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOverlapsSkipStackedPairs(t *testing.T) {
	parent := "i0000000001"
	all := []api.Intent{
		{ID: parent, Status: "open", CreatedAt: "2026-10-13T09:00:00Z", DeclaredPaths: []string{"docs/**"}, ActualPaths: []string{"docs/usage.md"}},
		{ID: "i0000000002", Status: "open", CreatedAt: "2026-10-13T09:01:00Z", DeclaredPaths: []string{"docs/**"}, ParentID: &parent},
	}
	if got := overlaps(all); len(got) != 0 {
		t.Fatalf("a stacked child overlapped its parent: %+v", got)
	}
	all[1].ParentID = nil
	got := overlaps(all)
	if len(got) != 1 || got[0].Older != parent || got[0].Suggestion == "" || strings.HasPrefix(strings.ToLower(got[0].Suggestion), "suggested") {
		t.Fatalf("overlaps = %+v", got)
	}
}

func TestStaleAndAbandonedIntents(t *testing.T) {
	g, p := "i0000000001", "i0000000002"
	reason, by := "superseded by i0000000004", "agent-a"
	all := []api.Intent{
		{ID: g, Agent: "agent-a", Status: "open", CreatedAt: "2026-10-13T09:00:00Z", DeclaredPaths: []string{"web/**"}},
		{ID: p, Agent: "agent-a", Status: "proposed", CreatedAt: "2026-10-13T09:01:00Z", DeclaredPaths: []string{"web/**"}, ParentID: &g, Stale: true},
		{ID: "i0000000003", Agent: "agent-b", Status: "open", CreatedAt: "2026-10-13T09:02:00Z", DeclaredPaths: []string{"web/**"}, ParentID: &p},
		{ID: "i0000000005", Agent: "agent-a", Status: "abandoned", CreatedAt: "2026-10-13T09:03:00Z", DeclaredPaths: []string{"web/**"},
			Stale: true, AbandonedReason: &reason, AbandonedBy: &by},
	}
	s, err := build("demo", "john", all, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Overlaps) != 0 {
		t.Fatalf("a three-deep stack and an abandoned intent are no overlaps: %+v", s.Overlaps)
	}
	views := map[string]IntentView{}
	for _, v := range s.Intents {
		views[v.ID] = v
	}
	if !views[p].Stale || views[g].Stale {
		t.Fatalf("stale = %v/%v", views[p].Stale, views[g].Stale)
	}
	if a := views["i0000000005"]; a.Stale || a.Active || a.AbandonedReason != reason || a.AbandonedBy != by {
		t.Fatalf("abandoned view = %+v", a)
	}
}

// A decision proposed before the Worker counted lines has paths only: the
// view must say the counts are unknown, not show +0 -0.
func TestDecisionViewWithoutLineCounts(t *testing.T) {
	d := api.Decision{ID: "d1", IntentID: "i1", Tags: json.RawMessage(`{}`), Action: "human_approve", Status: "pending_human",
		Paths: []string{"docs/auth.md", "src/auth/login.ts"}}
	v, err := decisionView(d)
	if err != nil {
		t.Fatal(err)
	}
	if !v.LinesUnknown || len(v.Files) != 2 || v.Added != 0 {
		t.Fatalf("paths-only view = %+v", v)
	}
	d.Detail.Files = []api.FileStat{{Path: "src/auth/login.ts", Added: 11}}
	if v, _ := decisionView(d); v.LinesUnknown || v.Added != 11 {
		t.Fatalf("counted view = %+v", v)
	}
}

// The Worker is down: every surface says so in words, nothing hangs, and a
// click records nothing.
func TestWorkerDown(t *testing.T) {
	_, srv := newFake(t)
	n := &fakeNative{}
	a, _ := newApp(t, srv, "john", n)
	srv.Close()
	if _, err := a.Snapshot(); err == nil {
		t.Fatal("snapshot succeeded against a closed server")
	}
	got := a.Approve("i0000000002", "", "Checked the rate limit window and the tests")
	if got.OK || got.Code != "network" || !strings.Contains(got.Message, "Could not reach the Worker") {
		t.Fatalf("outcome = %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startup(ctx)
	defer a.shutdown(ctx)
	waitFor(t, "a failed poll", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventPoll] > 0
	})
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.poll.OK || n.poll.Error == "" || n.poll.Failures < 1 || n.poll.NextMS <= 0 {
		t.Errorf("poll status = %+v", n.poll)
	}
	if n.emits[EventSnapshot] != 0 || len(n.notes) != 0 {
		t.Errorf("a failed poll emitted a snapshot or rang: %+v", n)
	}
}

// A login the Worker no longer accepts reads as "log in again", not as the
// Worker being down.
func TestUnauthorizedSaysLogIn(t *testing.T) {
	_, srv := newFake(t)
	n := &fakeNative{}
	a := New(Options{Creds: api.Credentials{URL: srv.URL, Key: "wrong"}, HTTP: srv.Client(), Interval: 10 * time.Millisecond, Native: n})
	a.prefs = prefs{Projects: []string{"demo"}, Current: "demo"}
	a.syncSession()
	if _, err := a.Snapshot(); err == nil || !strings.Contains(err.Error(), "wayglass login") {
		t.Errorf("snapshot error = %v", err)
	}
	if got := a.Approve("i0000000002", "", "Checked the rate limit window and the tests"); got.OK || got.Code != api.CodeUnauthorized || !strings.Contains(got.Message, "wayglass login") {
		t.Errorf("outcome = %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startup(ctx)
	defer a.shutdown(ctx)
	waitFor(t, "a failed poll", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventPoll] > 0
	})
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.poll.OK || !strings.Contains(n.poll.Error, "wayglass login") {
		t.Errorf("poll status = %+v", n.poll)
	}
}

// A stored human session past its expiry is not a login: the window says
// what to run instead of polling into 401s.
func TestExpiredHumanSession(t *testing.T) {
	t.Setenv("WAYGLASS_PROJECT", "")
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	mk := func(exp time.Time) Session {
		c := api.Credentials{URL: "https://api.example.dev", Key: "k", Source: "access", ExpiresAt: exp.Format(time.RFC3339)}
		return New(Options{Creds: c, Now: func() time.Time { return now }}).Session()
	}
	if s := mk(now.Add(-time.Minute)); s.LoggedIn || !strings.Contains(s.Problem, "session expired") || !strings.Contains(s.Problem, "login --human") {
		t.Errorf("expired = %+v", s)
	}
	if s := mk(now.Add(time.Hour)); !s.LoggedIn || s.Problem != "" {
		t.Errorf("live = %+v", s)
	}
	// No expiry recorded (admin or agent key): nothing to check.
	if s := New(Options{Creds: api.Credentials{URL: "https://api.example.dev", Key: "k"}}).Session(); !s.LoggedIn {
		t.Errorf("key without expiry = %+v", s)
	}
}

// What the window sends as an intent id is checked before any request:
// nothing but a Worker intent id reaches the client or the URL.
func TestDecideRejectsMalformedIntentID(t *testing.T) {
	f, srv := newFake(t)
	a, _ := newApp(t, srv, "john", &fakeNative{})
	for _, id := range []string{"", "../keys", "I0000000002", "i0000000002/decide", "d02"} {
		got := a.Approve(id, "", "Checked the rate limit window and the tests")
		if got.OK || got.Code != api.CodeBadRequest {
			t.Errorf("%q: outcome = %+v", id, got)
		}
	}
	if n := f.hit("GET /v1/intents") + f.hit("GET /v1/decisions"); n != 0 {
		t.Errorf("%d requests made for malformed ids", n)
	}
}

// multi_approve counts distinct approvers: approving twice is refused in
// the window, a reject still goes through, and another person's approval
// completes it.
func TestMultiApproveOncePerPerson(t *testing.T) {
	setup := func(t *testing.T) (*fakeWorker, *httptest.Server) {
		f, srv := newFake(t)
		f.decisions[1].Action = "multi_approve"
		f.decisions[1].ApprovalsNeeded = 2
		f.decisions[1].Approvals = []api.Approval{{DecisionID: "d02", By: "John", Verdict: "approve", Reason: "Checked the window"}}
		return f, srv
	}
	f, srv := setup(t)
	a, _ := newApp(t, srv, "john", &fakeNative{})
	s, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if d := s.NeedsYou[0].Decision; !d.AwaitsHuman || d.Action != "multi_approve" || len(d.Approvals) != 1 || d.Approvals[0].By != "John" {
		t.Fatalf("needs you = %+v", d)
	}
	got := a.Approve("i0000000002", "", "Checked it again")
	if got.OK || got.Code != "already_approved" || len(f.decided) != 0 {
		t.Fatalf("second approval = %+v, decided %v", got, f.decided)
	}
	if got := a.Reject("i0000000002", "", "Found a lockout bug on second look"); !got.OK || got.Decision.Status != api.StatusRejected {
		t.Fatalf("reject after approve = %+v", got)
	}

	_, srv = setup(t)
	b, _ := newApp(t, srv, "ada", &fakeNative{})
	if got := b.Approve("i0000000002", "", "Checked the retry path"); !got.OK || len(got.Decision.Approvals) != 2 {
		t.Fatalf("second approver = %+v", got)
	}
}

// When the Worker refuses because the decision changed under the window
// (superseded or decided elsewhere since the last poll), the refusal shows
// in the Worker's words and the window is refreshed without waiting.
func TestWorkerRefusalRefreshes(t *testing.T) {
	f, srv := newFake(t)
	n := &fakeNative{}
	a, _ := newApp(t, srv, "john", n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startup(ctx)
	waitFor(t, "first snapshot", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventSnapshot] > 0
	})
	a.shutdown(ctx) // stop the poller so the next snapshot is the refusal's
	f.mu.Lock()
	f.refuse = [2]string{api.CodeSuperseded, "decision d02 was superseded when intent i0000000002 was re-stacked, so it no longer covers the change; nothing was recorded. Next: its author must propose again"}
	f.mu.Unlock()
	n.mu.Lock()
	before := n.emits[EventSnapshot]
	n.mu.Unlock()
	got := a.Approve("i0000000002", "", "Checked the rate limit window and the tests")
	if got.OK || got.Code != api.CodeSuperseded || !strings.Contains(got.Message, "propose again") {
		t.Fatalf("outcome = %+v", got)
	}
	waitFor(t, "a refresh after the refusal", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.emits[EventSnapshot] > before
	})
}

// Audit events read in a poll whose other reads failed are not lost: the
// next poll rebuilds the snapshot even though the audit log has no news.
func TestEventsSurviveFailedRefresh(t *testing.T) {
	f, srv := newFake(t)
	a, _ := newApp(t, srv, "john", &fakeNative{})
	ctx := context.Background()
	if _, _, err := a.refresh(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = append(f.events, api.AuditEvent{Seq: 3, Kind: "intent.pushed", Actor: "claude-b", Data: json.RawMessage(`{"paths":["web/login.ts"]}`), At: "2026-10-13T11:20:00Z"})
	f.failIntents = true
	f.mu.Unlock()
	if _, _, err := a.refresh(ctx, false); err == nil {
		t.Fatal("refresh succeeded while intents were unavailable")
	}
	f.mu.Lock()
	f.failIntents = false
	f.mu.Unlock()
	s, changed, err := a.refresh(ctx, false)
	if err != nil || !changed {
		t.Fatalf("after recovery: changed=%v err=%v", changed, err)
	}
	if len(s.Events) != 3 || s.Events[0].Seq != 3 {
		t.Errorf("events = %+v", s.Events)
	}
	if _, changed, _ := a.refresh(ctx, false); changed {
		t.Error("an idle poll after recovery still rebuilt")
	}
}

// Agent-written text reaches the notification centre as one short plain
// line and the window without escape sequences or bidi controls.
func TestAgentTextIsCleaned(t *testing.T) {
	long := strings.Repeat("x", 200)
	body := notificationBody(NeedsYou{Intent: IntentView{ID: "i0000000009", Agent: "bot\u202e", Goal: "line one\nline two \x1b]0;owned\x07" + long}})
	if strings.ContainsAny(body, "\n\x1b\x07\u202e") || !strings.HasPrefix(body, "bot · i0000000009: line one line two ") || !strings.HasSuffix(body, "…") {
		t.Errorf("notification body = %q", body)
	}
	if r := []rune(body); len(r) > 160 {
		t.Errorf("notification body is %d runes", len(r))
	}

	f, srv := newFake(t)
	f.intents[1].Goal = "Add \x1b[2Jrate \u202elimit"
	f.intents[1].Agent = "claude\x1b[31m-b"
	f.decisions[1].Reason = "auth\u2066 changes"
	a, _ := newApp(t, srv, "john", &fakeNative{})
	s, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	in := s.NeedsYou[0]
	if in.Intent.Goal != "Add rate limit" || in.Intent.Agent != "claude-b" || in.Decision.Reason != "auth changes" {
		t.Errorf("cleaned = %q %q %q", in.Intent.Goal, in.Intent.Agent, in.Decision.Reason)
	}
}
