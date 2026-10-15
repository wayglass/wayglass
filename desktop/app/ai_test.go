package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// The window names the AI reviewer where it decided, carries an
// escalation's reason and concerns, and advice, into the Needs-you queue.
func TestDecisionViewCarriesAIRecord(t *testing.T) {
	by := fakeAI
	gw := "wayglass"
	v, err := decisionView(api.Decision{ID: "d1", IntentID: "i1", Action: "human_approve", Status: "pending_human", Tags: json.RawMessage(`{}`),
		Detail: api.DecisionDetail{
			Escalation: &api.Escalation{By: by, Reason: "adds a new host", Concerns: []string{"new outbound host"}, FromAction: "agent_review", AI: &api.AIMeta{Model: fakeAIModel, Gateway: &gw}},
			AIAdvice:   &api.AIAdvice{Summary: "fine within its paths", Questions: []string{"rollback?"}, By: by},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if v.AIModel != fakeAIModel || !v.AwaitsHuman || v.Escalation == nil || v.Escalation.Model != fakeAIModel || v.Escalation.Reason != "adds a new host" || v.Escalation.FromAction != "agent_review" || len(v.Escalation.Concerns) != 1 {
		t.Fatalf("escalation view = %+v (model %q)", v.Escalation, v.AIModel)
	}
	if v.AIAdvice == nil || v.AIAdvice.Model != fakeAIModel || v.AIAdvice.Risks == nil || v.AIAdvice.Concerns == nil || len(v.AIAdvice.Questions) != 1 {
		t.Fatalf("advice view = %+v", v.AIAdvice)
	}
	for _, src := range []string{"ai", ""} {
		v, err := decisionView(api.Decision{ID: "d2", IntentID: "i2", Action: "agent_review", Status: "approved", DecidedBy: &by,
			Approvals: []api.Approval{{By: by, Verdict: "approve", Reason: "matches the goal", IdentitySource: src}}})
		if err != nil {
			t.Fatal(err)
		}
		if v.AIModel != fakeAIModel || !v.Approvals[0].AI || v.Approvals[0].Who != "AI reviewer ("+fakeAIModel+")" || v.Approvals[0].Source != src {
			t.Fatalf("source %q: %+v", src, v)
		}
	}
	john := "john"
	v, _ = decisionView(api.Decision{ID: "d3", DecidedBy: &john, Approvals: []api.Approval{{By: "john", Verdict: "approve", IdentitySource: "access"}}})
	if v.AIModel != "" || v.Approvals[0].AI || v.Approvals[0].Who != "john" || v.Escalation != nil || v.AIAdvice != nil {
		t.Fatalf("a person's decision marked as the AI's: %+v", v)
	}
	// The event feed names the AI on its entries.
	for kind, want := range map[string]string{
		"decision.approve":   "approved by AI reviewer: fine",
		"decision.escalated": "escalated to a person by AI reviewer: fine (concerns: a; b)",
		"decision.ai_advice": "AI advice: s",
	} {
		got := summarize(api.AuditEvent{Kind: kind, Actor: by, Data: json.RawMessage(`{"reason":"fine","identity_source":"ai","concerns":["a","b"],"summary":"s"}`)})
		if got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
	if got := summarize(api.AuditEvent{Kind: "decision.approve", Actor: "john", Data: json.RawMessage(`{"reason":"ok"}`)}); got != "approved: ok" {
		t.Errorf("a person's approval: %q", got)
	}
}

// AskAI: a verdict on the agent_review change (approve, or escalate and the
// change joins the Needs-you queue with the AI's reason), advice on a change
// a person decides (recorded, decides nothing, cleaned), and the Worker's
// refusals in its own words.
func TestAskAI(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		f, srv := newFake(t)
		a, _ := newApp(t, srv, "john", &fakeNative{})
		out := a.AskAI("i0000000004")
		if out.OK || out.Code != "ai_unavailable" || !strings.Contains(out.Message, "AI_REVIEW=off") || f.asks != 0 {
			t.Fatalf("off: %+v (asks %d)", out, f.asks)
		}
		out = a.AskAI("i0000000002")
		if out.OK || out.Code != "ai_unavailable" || f.asks != 0 {
			t.Fatalf("advice while off: %+v", out)
		}
	})
	t.Run("approve", func(t *testing.T) {
		f, srv := newFake(t)
		f.aiVerdict = "approve"
		a, n := newApp(t, srv, "john", &fakeNative{})
		_ = n
		out := a.AskAI("i0000000004")
		if !out.OK || out.Kind != "review" || out.Verdict != "approve" || out.Model != fakeAIModel || out.Reason == "" || out.Decision == nil {
			t.Fatalf("approve: %+v", out)
		}
		if out.Decision.Status != "approved" || out.Decision.AIModel != fakeAIModel || len(out.Decision.Approvals) != 1 || !out.Decision.Approvals[0].AI {
			t.Fatalf("decision: %+v", out.Decision)
		}
		s, err := a.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.NeedsYou) != 2 {
			t.Fatalf("an approval adds nothing to the queue: %+v", s.NeedsYou)
		}
	})
	t.Run("escalate then advise", func(t *testing.T) {
		f, srv := newFake(t)
		f.aiVerdict = "escalate"
		a, _ := newApp(t, srv, "john", &fakeNative{})
		out := a.AskAI("i0000000004")
		if !out.OK || out.Verdict != "escalate" || len(out.Concerns) != 1 || out.Decision == nil || out.Decision.Escalation == nil || !out.Decision.AwaitsHuman {
			t.Fatalf("escalate: %+v", out)
		}
		s, err := a.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		var esc *NeedsYou
		for i := range s.NeedsYou {
			if s.NeedsYou[i].Intent.ID == "i0000000004" {
				esc = &s.NeedsYou[i]
			}
		}
		if esc == nil || esc.Own || esc.Decision.Escalation == nil || esc.Decision.Escalation.Reason != "The change touches more than the goal says." || esc.Decision.Escalation.Model != fakeAIModel {
			t.Fatalf("the escalated change should wait for john with the AI's reason: %+v", s.NeedsYou)
		}
		if s.Counters.NeedsYou != 2 {
			t.Fatalf("needs you = %d, want 2", s.Counters.NeedsYou)
		}
		if !strings.Contains(s.Events[0].Summary, "escalated to a person by AI reviewer") {
			t.Fatalf("events = %+v", s.Events[0])
		}
		// Asking again, now that a person must decide, gives advice.
		out = a.AskAI("i0000000004")
		if !out.OK || out.Kind != "advice" || out.Advice == nil || out.Advice.Model != fakeAIModel || len(out.Advice.Risks) != 1 || out.Decision == nil || out.Decision.AIAdvice == nil {
			t.Fatalf("advice: %+v", out)
		}
		if strings.ContainsAny(out.Advice.Summary, "\x1b") || !strings.Contains(out.Advice.Summary, "Within its declared paths") {
			t.Fatalf("model text must be cleaned: %q", out.Advice.Summary)
		}
		if out.Decision.DecidedBy != "" || !out.Decision.AwaitsHuman {
			t.Fatalf("advice decided something: %+v", out.Decision)
		}
		s, _ = a.Snapshot()
		for _, x := range s.NeedsYou {
			if x.Intent.ID == "i0000000004" && (x.Decision.AIAdvice == nil || x.Decision.AIAdvice.Summary == "") {
				t.Fatalf("advice should be on the queue card: %+v", x.Decision)
			}
		}
		if f.asks != 2 {
			t.Fatalf("asks = %d, want 2", f.asks)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		f, srv := newFake(t)
		f.aiVerdict = "approve"
		a, _ := newApp(t, srv, "john", &fakeNative{})
		for _, tc := range []struct{ intent, code, msg string }{
			{"i0000000001", "not_waiting", "approved"},
			{"i00000000ff", "not_waiting", "no proposed change"},
			{"not-an-id", "bad_request", "not an intent id"},
		} {
			out := a.AskAI(tc.intent)
			if out.OK || out.Code != tc.code || !strings.Contains(out.Message, tc.msg) {
				t.Errorf("%s: %+v", tc.intent, out)
			}
		}
		if f.asks != 0 {
			t.Fatalf("asks = %d, want 0", f.asks)
		}
		f.noAIAdvice = true
		out := a.AskAI("i0000000002")
		if out.OK || out.Code != "ai_unavailable" || !strings.Contains(out.Message, "ai-advice") {
			t.Fatalf("no advice route: %+v", out)
		}
	})
}
