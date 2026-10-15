package cli

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/intent"
	"github.com/wayglass/wayglass/internal/tui"
)

// TestWatchAndReviewLive runs watch and review against the fake Worker: the
// counters, the overlap with its suggested order, a person's queue, and the
// agent_review queue a reviewer agent reads.
func TestWatchAndReviewLive(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	trunk := get(e.do(a, ExitOK, "init", "--project", "demo"), "trunk", "remote").(string)
	clone := func(name string) string {
		dir := filepath.Join(t.TempDir(), name)
		e.git(a, "clone", "--quiet", trunk, dir)
		return dir
	}
	b, c := clone("agent-b"), clone("agent-c")

	// A: a docs change that auto-merges.
	idA := get(e.do(a, ExitOK, "intent", "declare", "--goal", "Write the guide", "--reason", "r", "--path", "docs/**"), "intent", "id").(string)
	// B: an auth change for a person, overlapping A on docs/guide.md.
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	idB := get(e.do(b, ExitOK, "intent", "declare", "--goal", "Rate-limit login", "--reason", "r",
		"--path", "web/login.ts", "--path", "docs/guide.md"), "intent", "id").(string)
	e.write(b, "web/login.ts", "export const maxAttempts = 5;\n")
	e.do(b, ExitOK, "push", "-m", "feat: limit login attempts")
	e.do(b, ExitNeedsHuman, "propose")
	// C: a low-risk change for a reviewer agent.
	t.Setenv("WAYGLASS_AGENT", "agent-c")
	idC := get(e.do(c, ExitOK, "intent", "declare", "--goal", "Tidy", "--reason", "r", "--path", "internal/**"), "intent", "id").(string)
	e.write(c, "internal/x.go", "package x\n")
	e.do(c, ExitOK, "push", "-m", "chore: tidy")
	e.do(c, ExitOK, "propose")
	t.Setenv("WAYGLASS_AGENT", "agent-a")
	e.write(a, "docs/guide.md", "# Guide\n")
	pushed := e.do(a, ExitOK, "push", "-m", "docs: guide")
	// B is already proposed, so the suggestion is to land B first.
	if ovs := get(pushed, "overlaps").([]any); len(ovs) != 1 ||
		get(ovs[0].(map[string]any), "intent_id") != idB ||
		get(ovs[0].(map[string]any), "replan", "action") != intent.ReplanLandFirst ||
		get(ovs[0].(map[string]any), "replan", "first") != idB {
		t.Fatalf("push overlaps = %v", get(pushed, "overlaps"))
	}
	if ovs := get(e.do(a, ExitOK, "overlaps"), "overlaps").([]any); len(ovs) != 1 ||
		get(ovs[0].(map[string]any), "replan", "first") != idB {
		t.Fatalf("overlaps = %v", ovs)
	}
	e.do(a, ExitOK, "propose")

	w := e.do(a, ExitOK, "watch")
	if get(w, "sample") != false || get(w, "project") != "demo" {
		t.Fatalf("watch = %v", w)
	}
	want := map[string]float64{"agents": 3, "intents": 3, "decisions": 3, "auto_landed": 2, "human_routed": 1, "awaiting_human": 1, "overlaps": 1}
	for k, v := range want {
		if got := get(w, "counters", k); got != v {
			t.Errorf("counters.%s = %v, want %v", k, got, v)
		}
	}
	// Cost comes from the Worker, with its basis; never computed client-side.
	if get(w, "counters", "cost", "ops") != float64(6) || get(w, "counters", "cost", "tokens_minted") != float64(3) ||
		!strings.Contains(get(w, "counters", "cost", "basis").(string), "Worker-side") {
		t.Errorf("counters.cost = %v", get(w, "counters", "cost"))
	}
	shown := e.do(a, ExitOK, "intent", "show", idA)
	if get(shown, "cost", "ops") != float64(2) || get(shown, "cost", "by_op", "fork") != float64(1) {
		t.Errorf("intent show cost = %v", get(shown, "cost"))
	}
	ov := get(w, "overlaps").([]any)[0].(map[string]any)
	if rp := ov["replan"].(map[string]any); ov["a"] != idA || ov["b"] != idB ||
		rp["action"] != intent.ReplanLandFirst || rp["first"] != idA || rp["then"] != idB || rp["suggested"] != true {
		t.Fatalf("overlap = %v", ov)
	}

	q := e.do(a, ExitOK, "review")
	if items := get(q, "queue").([]any); len(items) != 1 || get(items[0].(map[string]any), "intent", "id") != idB {
		t.Fatalf("human queue = %v", q)
	}
	aq := e.do(a, ExitOK, "review", "--for", "agent")
	items := get(aq, "queue").([]any)
	if len(items) != 1 || get(items[0].(map[string]any), "intent", "id") != idC ||
		get(items[0].(map[string]any), "decision", "status") != tui.StatusPendingAgent {
		t.Fatalf("agent queue = %v", aq)
	}
	if u, _ := get(aq, "untrusted").([]any); !slices.Contains(u, any("queue[].intent.goal")) || !slices.Contains(u, any("queue[].intent.reason")) {
		t.Fatalf("agent queue must mark agent-written fields untrusted: %v", get(aq, "untrusted"))
	}

	// The reviewer agent approves; the agent queue empties.
	t.Setenv("WAYGLASS_AGENT", "claude-reviewer")
	e.do(a, ExitOK, "approve", idC, "--reason", "Diff matches the intent")
	if items := get(e.do(a, ExitOK, "review", "--for", "agent"), "queue").([]any); len(items) != 0 {
		t.Fatalf("agent queue after approval = %v", items)
	}
}

func TestToTUIDecision(t *testing.T) {
	rule := "rules[2]"
	tags, _ := json.Marshal(map[string]any{"areas": []string{"auth"}, "risk": "low"})
	d := api.Decision{
		ID: "d1", IntentID: "i1", Tags: tags, MatchedRule: &rule, Action: "human_approve", Status: "pending_human",
		ApprovalsNeeded: 1, Paths: []string{"web/login.ts"}, CreatedAt: "2026-10-12T10:00:00Z",
		Approvals: []api.Approval{{By: "john", Verdict: "approve", Reason: "ok", At: "2026-10-12T10:05:00Z"}},
	}
	got, err := toTUIDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Tags.HasArea("auth") || got.MatchedRule != rule || !got.AwaitsHuman() || got.Approved() != 1 {
		t.Fatalf("decision = %+v", got)
	}
	// Older decisions carry paths only; newer ones line counts.
	if len(got.Files) != 1 || got.Files[0] != (tui.FileStat{Path: "web/login.ts"}) || !got.LinesUnknown {
		t.Fatalf("files from paths = %+v, lines unknown %v", got.Files, got.LinesUnknown)
	}
	d.Detail.Files = []api.FileStat{{Path: "web/login.ts", Added: 2, Deleted: 1}}
	if got, _ := toTUIDecision(d); got.Files[0] != (tui.FileStat{Path: "web/login.ts", Added: 2, Deleted: 1}) || got.LinesUnknown {
		t.Fatalf("files from detail = %+v, lines unknown %v", got.Files, got.LinesUnknown)
	}
	d.Tags = json.RawMessage(`"nope"`)
	if _, err := toTUIDecision(d); err == nil {
		t.Fatal("bad tags must be an error, not empty tags")
	}
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		kind, data, want string
	}{
		{"intent.declared", `{"goal":"Write the guide"}`, "declared: Write the guide"},
		{"intent.pushed", `{"paths":["a","b"]}`, "pushed 2 file(s)"},
		{"decision.proposed", `{"action":"block","matched_rule":"rules[0]","reason":"Secrets never land."}`, "block by rules[0]: Secrets never land."},
		{"decision.approve", `{"reason":"Checked it"}`, "approved: Checked it"},
		{"intent.landed", `{"commit":"0123456789abcdef"}`, "landed 0123456789ab on trunk"},
		{"token.refreshed", `{}`, "token refreshed"},
		{"intent.pushed", `not json`, "pushed 0 file(s)"},
	}
	for _, tt := range tests {
		if got := summarize(api.AuditEvent{Kind: tt.kind, Data: json.RawMessage(tt.data)}); got != tt.want {
			t.Errorf("summarize(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}
