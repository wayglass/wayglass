package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/policy"
)

// The AI reviewer in the sample data. Like the rest of DemoSource it is
// SAMPLE DATA: no model runs; the verdicts below are scripted so the screens
// show what an AI approval, an escalation and advice look like.
const (
	demoAIModel = "@cf/meta/llama-4-scout-17b-16e-instruct"
	demoAI      = aiPrefix + demoAIModel
	demoI18n    = "i2b3c4d5e6f"
	demoMetrics = "i4c5d6e7f8a"
)

// DemoAISource is DemoSource with the AI reviewer in the script: two more
// agent_review changes, one it approves and one it escalates to a person,
// and advice on the auth change. It also answers AskAI, so `i` in
// `wayglass review --demo-data` shows the flow without a Worker.
type DemoAISource struct{ *DemoSource }

// WithAIReviewer adds the AI reviewer's steps to the script. Call it before
// the source is read.
func (s *DemoSource) WithAIReviewer() *DemoAISource {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, demoAIScript()...)
	return &DemoAISource{s}
}

func demoAIScript() []demoStep {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	return []demoStep{
		{sec(12.6), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoI18n, Agent: "codex-3", Goal: "Translate the footer into German",
				Reason: "The DE launch page still shows English footer links.", DeclaredPaths: []string{"web/footer.ts"}},
				classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
					{Path: "web/footer.ts", Added: lines(8, "de string"), Deleted: 2}}})
		}},
		{sec(13.2), func(s *DemoSource, at time.Time) { s.push(at, demoI18n) }},
		{sec(13.6), func(s *DemoSource, at time.Time) { s.propose(at, demoI18n, "d2b3c4d5e6f") }},
		{sec(14.4), func(s *DemoSource, at time.Time) {
			s.aiApprove(at, "d2b3c4d5e6f", "The diff adds the eight footer strings the goal names and changes nothing else; it stays inside web/footer.ts.")
		}},
		{sec(15.0), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoMetrics, Agent: "claude-4", Goal: "Retry failed metrics uploads",
				Reason: "Dashboards show gaps when the collector restarts.", DeclaredPaths: []string{"web/metrics.ts"}},
				classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
					{Path: "web/metrics.ts", Added: append(lines(10, "retry"), "fetch('https://metrics.example.net/v1/ingest')", "while (true) { await send() }"), Deleted: 1}}})
		}},
		{sec(15.6), func(s *DemoSource, at time.Time) { s.push(at, demoMetrics) }},
		{sec(16.0), func(s *DemoSource, at time.Time) { s.propose(at, demoMetrics, "d4c5d6e7f8a") }},
		{sec(17.0), func(s *DemoSource, at time.Time) {
			s.aiEscalate(at, "d4c5d6e7f8a", "The change adds a call to a host the goal does not mention (metrics.example.net) and retries without an upper bound.",
				[]string{"new outbound host metrics.example.net", "retry loop has no limit"})
		}},
		{sec(18.0), func(s *DemoSource, at time.Time) {
			s.aiAdvise(at, "d3f2a91c0d1", AIAdvice{
				Summary:   "Limits login attempts per client address in a 60-second window; the lockout is per address, not per account.",
				Risks:     []string{"Shared office or carrier NAT addresses reach the limit together"},
				Concerns:  []string{"No test covers the window reset"},
				Questions: []string{"Should failed attempts also count per account?"},
			})
		}},
	}
}

// aiApprove records the AI reviewer's approval of an agent_review change.
func (s *DemoSource) aiApprove(at time.Time, decisionID, reason string) {
	i := slices.IndexFunc(s.decs, func(d Decision) bool { return d.ID == decisionID })
	if i < 0 || s.decs[i].Status != StatusPendingAgent {
		return
	}
	d := &s.decs[i]
	d.Approvals = append(d.Approvals, Approval{By: demoAI, Verdict: Approve, Reason: reason, At: at, Source: SourceAI})
	d.Status, d.DecidedBy = StatusApproved, demoAI
	s.intent(d.IntentID).Status = IntentLanded
	s.emit(at, "decision.approve", demoAI, d.IntentID, d.ID, "approved by AI reviewer: "+reason)
	s.emit(at, "change.landed", demoAI, d.IntentID, d.ID, "landed on trunk")
}

// aiEscalate records the AI reviewer sending an agent_review change to a
// person: the action becomes human_approve, as on the Worker.
func (s *DemoSource) aiEscalate(at time.Time, decisionID, reason string, concerns []string) {
	i := slices.IndexFunc(s.decs, func(d Decision) bool { return d.ID == decisionID })
	if i < 0 || s.decs[i].Status != StatusPendingAgent {
		return
	}
	d := &s.decs[i]
	d.Escalation = &Escalation{By: demoAI, Model: demoAIModel, Reason: reason, Concerns: slices.Clone(concerns), FromAction: string(d.Action), At: at}
	d.Action, d.Status = policy.HumanApprove, StatusPendingHuman
	s.emit(at, "decision.escalated", demoAI, d.IntentID, d.ID, "escalated to a person by AI reviewer: "+reason)
}

// aiAdvise records the AI reviewer's advice on a change a person decides.
func (s *DemoSource) aiAdvise(at time.Time, decisionID string, a AIAdvice) {
	i := slices.IndexFunc(s.decs, func(d Decision) bool { return d.ID == decisionID })
	if i < 0 || !s.decs[i].AwaitsHuman() {
		return
	}
	a.Model, a.At = demoAIModel, at
	d := &s.decs[i]
	d.AIAdvice = &a
	s.emit(at, "decision.ai_advice", demoAI, d.IntentID, d.ID, "AI advice: "+a.Summary)
}

// AskAI implements aiAsker on the sample data: a scripted verdict on a
// change waiting for a reviewer agent, or scripted advice on one waiting for
// a person. Nothing is sent anywhere.
func (s *DemoAISource) AskAI(_ context.Context, d Decision) (AIAnswer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	i := slices.IndexFunc(s.decs, func(x Decision) bool { return x.ID == d.ID })
	if i < 0 {
		return AIAnswer{}, fmt.Errorf("decision %s: %w", d.ID, ErrNotFound)
	}
	at := s.now()
	cur := &s.decs[i]
	switch {
	case cur.Status == StatusPendingAgent:
		reason := "The diff does what the goal says, stays inside the declared paths and adds no secrets, data handling or new hosts."
		if strings.Contains(strings.Join(cur.Tags.DataClasses, ","), "pii") {
			reason = "The change handles personal data the goal does not explain; a person should decide."
			s.aiEscalate(at, cur.ID, reason, []string{"personal data in the diff"})
			return AIAnswer{Kind: "review", Model: demoAIModel, Verdict: "escalate", Reason: reason, Concerns: []string{"personal data in the diff"}, Decision: cloneDecision(s.decs[i])}, nil
		}
		s.aiApprove(at, cur.ID, reason)
		return AIAnswer{Kind: "review", Model: demoAIModel, Verdict: "approve", Reason: reason, Decision: cloneDecision(s.decs[i])}, nil
	case cur.AwaitsHuman():
		a := AIAdvice{
			Summary:   "The change does what the goal says within its declared paths; the matched rule sends it to a person because of what it touches, not how.",
			Risks:     []string{"Behaviour at the boundary the rule protects is only covered by the agent's own description"},
			Concerns:  []string{"No test in the diff exercises the failure path"},
			Questions: []string{"Is there a rollback if the change misbehaves after it lands?"},
		}
		s.aiAdvise(at, cur.ID, a)
		adv := *s.decs[i].AIAdvice
		return AIAnswer{Kind: "advice", Model: demoAIModel, Advice: &adv, Decision: cloneDecision(s.decs[i])}, nil
	}
	return AIAnswer{}, fmt.Errorf("decision %s is %s: %w", d.ID, cur.Status, ErrNotPending)
}
