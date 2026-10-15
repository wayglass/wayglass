package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/notes"
	"github.com/wayglass/wayglass/internal/tui"
)

const fakeAIModel = "@cf/meta/llama-4-scout-17b-16e-instruct"

// The live source carries the AI reviewer's record into the TUI: who
// decided, an escalation's reason and concerns, advice, and the AI's cost.
func TestLiveSourceCarriesAIRecord(t *testing.T) {
	by := fakeAIReviewer
	gw := "wayglass"
	d := api.Decision{ID: "d1", IntentID: "i1", Action: "human_approve", Status: api.StatusPendingHuman, Tags: json.RawMessage(`{}`),
		Detail: api.DecisionDetail{
			Escalation: &api.Escalation{By: by, Reason: "adds a new host", Concerns: []string{"new outbound host"}, FromAction: "agent_review", AI: &api.AIMeta{Model: fakeAIModel, Gateway: &gw}},
			AIAdvice:   &api.AIAdvice{Summary: "fine within its paths", Questions: []string{"rollback?"}, By: by},
		}}
	td, err := toTUIDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	if td.Escalation == nil || td.Escalation.Model != fakeAIModel || td.Escalation.Reason != "adds a new host" || td.Escalation.FromAction != "agent_review" || len(td.Escalation.Concerns) != 1 {
		t.Fatalf("escalation = %+v", td.Escalation)
	}
	if model, ok := td.AIDecided(); !ok || model != fakeAIModel || td.Status != tui.StatusPendingHuman || !td.AwaitsHuman() {
		t.Fatalf("an escalated change is AI-decided and waits for a person: %+v", td)
	}
	if td.AIAdvice == nil || td.AIAdvice.Model != fakeAIModel || td.AIAdvice.Risks == nil || len(td.AIAdvice.Questions) != 1 {
		t.Fatalf("advice = %+v", td.AIAdvice)
	}
	// An approval by the AI, with and without the identity source.
	for _, src := range []string{"ai", ""} {
		d := api.Decision{ID: "d2", IntentID: "i2", Action: "agent_review", Status: api.StatusApproved, DecidedBy: &by,
			Approvals: []api.Approval{{By: by, Verdict: "approve", Reason: "matches the goal", IdentitySource: src}}}
		td, err := toTUIDecision(d)
		if err != nil {
			t.Fatal(err)
		}
		if model, ok := td.AIDecided(); !ok || model != fakeAIModel || !td.Approvals[0].AI() || td.Approvals[0].Source != src {
			t.Fatalf("source %q: %+v", src, td)
		}
	}
	// A person's decision is not marked.
	john := "john"
	td, _ = toTUIDecision(api.Decision{ID: "d3", DecidedBy: &john, Approvals: []api.Approval{{By: "john", Verdict: "approve", IdentitySource: "access"}}})
	if _, ok := td.AIDecided(); ok || td.AIInvolved() {
		t.Fatalf("a person's approval marked as AI: %+v", td)
	}

	cost := toTUICost(&api.CostSummary{Total: api.CostLine{Ops: 3}, AI: &api.AICost{Calls: 4, EstUSD: 0.01, Tokens: 350, Today: &api.AICost{Calls: 1, EstUSD: 0.002}}})
	if cost.AI == nil || cost.AI.Calls != 4 || cost.AI.Tokens != 350 || !cost.AI.Today || cost.AI.TodayCalls != 1 || cost.AI.TodayUSD != 0.002 {
		t.Fatalf("ai cost = %+v", cost.AI)
	}
	if c := toTUICost(&api.CostSummary{}); c.AI != nil {
		t.Fatal("a Worker without AI stats must show none")
	}
}

// Asking from a review screen: the AI decides an agent_review change and
// advises on a change routed to a person; the Worker's refusals keep their
// words, and a Worker without the advice route says so.
func TestLiveSourceAskAI(t *testing.T) {
	e := newFlow(t)
	id := agentReviewIntent(t, e)
	src := &liveSource{c: api.New(os.Getenv("WAYGLASS_URL"), e.worker.key), project: "demo"}
	src.c.Agent = "person"
	ctx := t.Context()
	pending := func() tui.Decision {
		ds, err := src.Decisions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if d.IntentID == id {
				return d
			}
		}
		t.Fatalf("no decision for %s", id)
		return tui.Decision{}
	}

	// Off: the refusal is marked unavailable and nothing is recorded.
	_, err := src.AskAI(ctx, pending())
	if !errors.Is(err, tui.ErrAIUnavailable) || !strings.Contains(err.Error(), "AI_REVIEW=off") {
		t.Fatalf("off: %v", err)
	}
	if e.worker.aiReviews != 0 {
		t.Fatal("nothing may be recorded while the reviewer is off")
	}

	// A verdict on the agent_review change: escalate, then it waits for a person.
	e.worker.aiVerdict = "escalate"
	ans, err := src.AskAI(ctx, pending())
	if err != nil {
		t.Fatal(err)
	}
	if ans.Kind != "review" || ans.Verdict != "escalate" || ans.Model != fakeAIModel || len(ans.Concerns) != 1 || ans.Decision.Escalation == nil || !ans.Decision.AwaitsHuman() {
		t.Fatalf("escalate = %+v", ans)
	}
	if d := pending(); d.Escalation == nil || d.Escalation.Model != fakeAIModel || d.Escalation.FromAction != "agent_review" {
		t.Fatalf("the escalation should be on the record: %+v", d.Escalation)
	}

	// Advice on the change now waiting for a person: recorded, decides nothing.
	ans, err = src.AskAI(ctx, pending())
	if err != nil {
		t.Fatal(err)
	}
	if ans.Kind != "advice" || ans.Advice == nil || ans.Advice.Model != fakeAIModel || ans.Advice.Summary == "" || ans.Decision.AIAdvice == nil || ans.Decision.DecidedBy != "" {
		t.Fatalf("advice = %+v", ans)
	}
	if d := pending(); d.AIAdvice == nil || !d.AwaitsHuman() {
		t.Fatalf("advice must be recorded and decide nothing: %+v", d)
	}

	// The event feed names the AI on every entry it wrote.
	events, err := src.Audit(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var feed []string
	for _, ev := range events {
		feed = append(feed, ev.Kind+": "+ev.Summary)
	}
	all := strings.Join(feed, "\n")
	for _, want := range []string{"decision.escalated: escalated to a person by AI reviewer: The change touches more than the goal says. (concerns: scope wider than the goal)", "decision.ai_advice: AI advice: Makes login() return true"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in feed:\n%s", want, all)
		}
	}

	// A Worker without the advice route: unavailable, with the route named.
	old := os.Getenv("WAYGLASS_URL")
	noRoute := &liveSource{c: api.New(old, e.worker.key), project: "demo"}
	noRoute.c.Agent = "person"
	e.worker.noAIAdvice = true
	_, err = noRoute.AskAI(ctx, pending())
	if !errors.Is(err, tui.ErrAIUnavailable) || !strings.Contains(err.Error(), "ai-advice") {
		t.Fatalf("no route: %v", err)
	}
}

// The AI's approval and escalation read as the AI's in why and log, with
// the model named; a person's stay as they were.
func TestWhyAndLogNameTheAIReviewer(t *testing.T) {
	st := cliStyles(Output{})
	by := fakeAIReviewer
	at := "2026-10-14T10:00:00Z"
	rule := "low-risk-agent-review"
	var out bytes.Buffer
	r := whyResult{File: "web/profile.ts", Line: 1, Commit: "abc1234", Source: "worker", Verified: "worker", Worker: &api.WhyResult{
		Intent:    api.Intent{ID: "i1", Goal: "Profile page", Reason: "Users asked"},
		Decision:  api.Decision{ID: "d1", Action: "agent_review", Status: api.StatusApproved, MatchedRule: &rule, Reason: "low risk", DecidedBy: &by, DecidedAt: &at},
		Approvals: []api.Approval{{By: by, Verdict: "approve", Reason: "The diff does what the goal says and nothing else.", IdentitySource: "ai"}},
	}}
	if err := renderWhy(&out, st, r); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"decided by AI reviewer (" + fakeAIModel + ")", "Approved: by AI reviewer (" + fakeAIModel + "): The diff does what the goal says"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "workers-ai:") {
		t.Errorf("the raw decider name leaked:\n%s", got)
	}

	// From a note, the same words.
	out.Reset()
	n := notes.Note{IntentID: "i1", Goal: "Profile page", Reason: "Users asked", Action: "agent_review", Rule: rule, DecidedBy: by, IdentitySource: "ai",
		Approvals: []notes.Approval{{By: by, Verdict: "approve", Reason: "matches the goal", IdentitySource: "ai"}}}
	if err := renderWhy(&out, st, whyResult{File: "f", Line: 1, Commit: "abc1234", Source: "note", Verified: "worker", Note: &n}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "decided by AI reviewer ("+fakeAIModel+")") || !strings.Contains(got, "by AI reviewer ("+fakeAIModel+"): matches the goal") {
		t.Errorf("note rendering:\n%s", got)
	}

	// An escalation then a person's approval, and advice, on the Worker's record.
	out.Reset()
	john := "john"
	r.Worker.Decision = api.Decision{ID: "d1", Action: "human_approve", Status: api.StatusApproved, MatchedRule: &rule, Reason: "low risk", DecidedBy: &john, DecidedAt: &at,
		Detail: api.DecisionDetail{
			Escalation: &api.Escalation{By: by, Reason: "adds a new host", Concerns: []string{"new outbound host", "no retry limit"}, FromAction: "agent_review"},
			AIAdvice:   &api.AIAdvice{Summary: "the host is the metrics collector", By: by},
		}}
	r.Worker.Approvals = []api.Approval{{By: "john", Verdict: "approve", Reason: "the host is ours", IdentitySource: "access"}}
	if err := renderWhy(&out, st, r); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	for _, want := range []string{"Escalated: by AI reviewer (" + fakeAIModel + "): adds a new host", "concerns: new outbound host; no retry limit", "was agent_review; a person decided",
		"john (Access): the host is ours", "AI advice: from AI reviewer (" + fakeAIModel + "), advisory only: the host is the metrics collector"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// log rows.
	ev := func(kind, actor, data string) api.AuditEvent {
		return api.AuditEvent{Seq: 1, At: at, Kind: kind, Actor: actor, Data: json.RawMessage(data)}
	}
	cases := []struct {
		e    api.AuditEvent
		want string
		tone tui.Tone
	}{
		{ev("decision.approve", by, `{"reason":"matches","status":"approved","identity_source":"ai","ai":{"model":"m2"}}`), "approved by AI reviewer (m2): matches", tui.ToneGood},
		{ev("decision.approve", by, `{"reason":"matches","status":"approved"}`), "approved by AI reviewer (" + fakeAIModel + "): matches", tui.ToneGood},
		{ev("decision.approve", "john", `{"reason":"checked","status":"approved","identity_source":"access"}`), "approved: checked", tui.ToneGood},
		{ev("decision.escalated", by, `{"reason":"new host","concerns":["a","b"],"from_action":"agent_review"}`), "escalated to a person by AI reviewer (" + fakeAIModel + "): new host (concerns: a; b)", tui.ToneAttention},
		{ev("decision.escalated", by, `{"reason":"new host"}`), "escalated to a person by AI reviewer (" + fakeAIModel + "): new host", tui.ToneAttention},
		{ev("decision.ai_advice", by, `{"summary":"fine","risks":["r"],"questions":["q"]}`), "AI advice from AI reviewer (" + fakeAIModel + "): fine (2 point(s); advisory)", tui.ToneNone},
	}
	for _, tc := range cases {
		row := logRow(tc.e)
		if row.Detail != tc.want || row.Tone != tc.tone {
			t.Errorf("%s: detail %q tone %v, want %q %v", tc.e.Kind, row.Detail, row.Tone, tc.want, tc.tone)
		}
	}
	rendered := tui.RenderLog([]tui.LogRow{logRow(cases[3].e)}, tui.NewStyles(true), 200, time.UTC)
	if !strings.Contains(rendered, "▲ escalated to a person by AI reviewer") {
		t.Errorf("log should mark the escalation for attention:\n%s", rendered)
	}
}
