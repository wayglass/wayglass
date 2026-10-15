package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

// aiDemoAt is the sample data with the AI reviewer's steps, played to offset.
func aiDemoAt(offset time.Duration) (*DemoAISource, *fixedClock) {
	ds, c := demoAt(offset)
	return ds.WithAIReviewer(), c
}

func aiReviewAt(t *testing.T, w, h int, intentID string) (ReviewModel, *DemoAISource) {
	t.Helper()
	ds, c := aiDemoAt(time.Minute)
	m := NewReview(ds, testOptions(c), intentID)
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})
	m = step(m, m.fetch()())
	return m, ds
}

// selectIntent moves the review cursor to the intent's decision, with the
// filter off so decided changes are listed too.
func selectIntent(t *testing.T, m ReviewModel, id string) ReviewModel {
	t.Helper()
	if !m.all {
		m = step(m, press("tab"))
	}
	for i := 0; i < len(m.Queue()); i++ {
		if d, _ := m.Selected(); d.IntentID == id {
			return m
		}
		m = step(m, press("j"))
	}
	t.Fatalf("intent %s is not listed", id)
	return m
}

func aiWatchAt(t *testing.T, w, h int) WatchModel {
	t.Helper()
	ds, c := aiDemoAt(time.Minute)
	m := NewWatch(ds, testOptions(c))
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})
	m = step(m, m.fetch()())
	return m
}

var aiSizes = []struct{ w, h int }{{80, 24}, {120, 40}}

// The scripted AI reviewer approves one agent_review change, escalates
// another to a person and advises on the auth change; the counters say so.
func TestDemoAIReviewerScript(t *testing.T) {
	ds, _ := aiDemoAt(time.Minute)
	s := load(t, ds)
	byIntent := map[string]Decision{}
	for _, d := range s.Decisions {
		byIntent[d.IntentID] = d
	}
	i18n := byIntent[demoI18n]
	if model, ok := i18n.AIDecided(); !ok || model != demoAIModel || i18n.Status != StatusApproved || i18n.DecidedBy != demoAI {
		t.Fatalf("i18n change should be approved by the AI: %+v", i18n)
	}
	if len(i18n.Approvals) != 1 || !i18n.Approvals[0].AI() || i18n.Approvals[0].Source != SourceAI {
		t.Fatalf("approval should be the AI's: %+v", i18n.Approvals)
	}
	metrics := byIntent[demoMetrics]
	if metrics.Escalation == nil || metrics.Status != StatusPendingHuman || metrics.Action != "human_approve" || metrics.Escalation.FromAction != "agent_review" || len(metrics.Escalation.Concerns) != 2 {
		t.Fatalf("metrics change should be escalated to a person: %+v", metrics)
	}
	if !metrics.AwaitsHuman() {
		t.Fatal("an escalated change waits for a person")
	}
	auth := byIntent[demoAuth]
	if auth.AIAdvice == nil || auth.AIAdvice.Model != demoAIModel || len(auth.AIAdvice.Questions) != 1 {
		t.Fatalf("auth change should carry AI advice: %+v", auth.AIAdvice)
	}
	if !auth.AwaitsHuman() || auth.DecidedBy != "" {
		t.Fatal("advice must not decide the change")
	}
	c := s.Counters
	if c.AIApprovals != 1 || c.AIEscalations != 1 || c.AIAdvised != 1 {
		t.Fatalf("AI counters = %d/%d/%d, want 1/1/1", c.AIApprovals, c.AIEscalations, c.AIAdvised)
	}
	if n := c.AwaitingHuman; n != 4 {
		t.Fatalf("awaiting a person = %d, want 4 (three routed plus one escalated)", n)
	}
	// The plain sample has none of this: the goldens of the other tests hold.
	plain, _ := demoAt(time.Minute)
	if pc := load(t, plain).Counters; pc.AIApprovals+pc.AIEscalations+pc.AIAdvised != 0 {
		t.Fatalf("plain sample data has an AI reviewer: %+v", pc)
	}
}

// The review card names the AI reviewer where it decided, shows an
// escalation's reason and concerns, and labels advice as advice.
func TestReviewAIGolden(t *testing.T) {
	for _, sz := range aiSizes {
		t.Run(fmt.Sprintf("escalated/%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := aiReviewAt(t, sz.w, sz.h, demoMetrics)
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			for _, want := range []string{"Escalated by AI reviewer (" + demoAIModel + ")", "metrics.example.net", "• retry loop has no limit", "was agent review", "▲ AI escalated", "i ask AI"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			golden.RequireEqual(t, out)
		})
		t.Run(fmt.Sprintf("advice/%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := aiReviewAt(t, sz.w, sz.h, demoAuth)
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			if !strings.Contains(out, "AI advice") {
				t.Errorf("missing the advice section in:\n%s", out)
			}
			if sz.h >= 40 {
				for _, want := range []string{"advisory", "Advice only, from AI reviewer (" + demoAIModel + ")", "Limits login attempts", "Risks", "• Shared office", "Questions"} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %q in:\n%s", want, out)
					}
				}
			}
			golden.RequireEqual(t, out)
		})
	}
}

// With every decision listed, the AI-approved change reads "landed ·AI" and
// its approval names the AI reviewer, not an agent.
func TestReviewListsAIApproval(t *testing.T) {
	m, _ := aiReviewAt(t, 120, 40, demoI18n)
	m = selectIntent(t, m, demoI18n)
	out := screen(m.View())
	for _, want := range []string{"landed ·AI", "✓ approve ◆ AI reviewer (" + demoAIModel + ")", "eight footer strings"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Watch shows the AI's work on the changes rows and in the counters.
func TestWatchAIGolden(t *testing.T) {
	for _, sz := range aiSizes {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := aiWatchAt(t, sz.w, sz.h)
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			if !strings.Contains(out, "AI escalated") || !strings.Contains(out, "landed ·AI") {
				t.Errorf("missing the AI markers on the changes rows in:\n%s", out)
			}
			if !strings.Contains(out, "1 approved · 1 escalated · 1 advised") && !strings.Contains(out, "ai 1✓ 1▲ 1adv") {
				t.Errorf("missing the AI counters in:\n%s", out)
			}
			golden.RequireEqual(t, out)
		})
	}
}

func TestWatchDetailShowsAIEscalationAndAdvice(t *testing.T) {
	m := aiWatchAt(t, 140, 60)
	for i := 0; i < 12; i++ { // find the metrics intent
		if in, _ := m.Selected(); in.ID == demoMetrics {
			break
		}
		m = step(m, press("j"))
	}
	m = step(m, press("enter"))
	out := screen(m.View())
	for _, want := range []string{"Escalated by AI reviewer (" + demoAIModel + ")", "• new outbound host", "▲ AI escalated"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	ds, c := aiDemoAt(time.Minute)
	s := load(t, ds)
	got := ansi.Strip(RenderIntentDetail(s, demoAuth, NewStyles(true), 120, time.UTC, nil))
	for _, want := range []string{"AI advice (AI reviewer (" + demoAIModel + "), advisory)", "• risk: Shared office", "• question:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	_ = c
}

// The AI's cost shows only when the Worker measured it, today's figure first.
func TestWatchCountersShowAICost(t *testing.T) {
	st := NewStyles(true)
	s := Snapshot{Project: "p", Counters: Counters{Cost: &Cost{Ops: 10, AI: &AICost{Calls: 7, EstUSD: 0.021}}}}
	got := ansi.Strip(RenderWatch(s, WatchLayout{Styles: st, Width: 200}))
	if !strings.Contains(got, "ai cost 7 call(s) ≈$0.02") {
		t.Fatalf("missing total AI cost in:\n%s", got)
	}
	s.Counters.Cost.AI.Today, s.Counters.Cost.AI.TodayCalls, s.Counters.Cost.AI.TodayUSD = true, 2, 0.003
	got = ansi.Strip(RenderWatch(s, WatchLayout{Styles: st, Width: 200}))
	if !strings.Contains(got, "ai today 2 call(s) ≈$0.0030") {
		t.Fatalf("missing today's AI cost in:\n%s", got)
	}
	s.Counters.Cost.AI = nil
	if got := ansi.Strip(RenderWatch(s, WatchLayout{Styles: st, Width: 200})); strings.Contains(got, "ai ") {
		t.Fatalf("no AI cost measured, none shown:\n%s", got)
	}
}

// `i` asks the AI reviewer: advice on a change the person decides, a verdict
// on one waiting for a reviewer agent; the answer lands in the card.
func TestReviewAskAI(t *testing.T) {
	// Advice on the auth change (routed to a person).
	m, _ := aiReviewAt(t, 120, 40, demoAuth)
	m = step(m, press("i"))
	if !m.asking {
		t.Fatal("i should start an ask")
	}
	if out := screen(m.View()); !strings.Contains(out, "asking the AI reviewer") {
		t.Fatalf("footer should say the ask is in flight:\n%s", out)
	}
	next, cmd := m.askAI()
	m = next.(ReviewModel)
	if cmd != nil {
		t.Fatal("a second i while asking must not start another ask")
	}
	m.asking = false
	_, cmd = m.askAI()
	m = step(m, cmd())
	if m.asking || m.noticeE || !strings.Contains(m.notice, "advised on "+shortID(demoAuth)) || !strings.Contains(m.notice, "advice only") {
		t.Fatalf("notice = %q (err %v)", m.notice, m.noticeE)
	}
	m = step(m, m.fetch()())
	if d, _ := m.Selected(); d.AIAdvice == nil || d.DecidedBy != "" || !d.AwaitsHuman() {
		t.Fatalf("advice must be recorded and decide nothing: %+v", d)
	}

	// A verdict on a change waiting for a reviewer agent (before the script's AI step).
	ds, c := aiDemoAt(14 * time.Second)
	r := NewReview(ds, testOptions(c), demoI18n)
	r = step(r, tea.WindowSizeMsg{Width: 120, Height: 40})
	r = step(r, r.fetch()())
	r = selectIntent(t, r, demoI18n)
	if d, _ := r.Selected(); d.IntentID != demoI18n || d.Status != StatusPendingAgent {
		t.Fatalf("selected %+v, want the pending agent_review change", d)
	}
	_, cmd = r.askAI()
	r = step(r, cmd())
	if r.noticeE || !strings.Contains(r.notice, "AI reviewer ("+demoAIModel+") approved "+shortID(demoI18n)) {
		t.Fatalf("notice = %q", r.notice)
	}
	r = step(r, r.fetch()())
	if d, _ := r.Selected(); d.Status != StatusApproved || d.DecidedBy != demoAI {
		t.Fatalf("the AI's approval should be recorded: %+v", d)
	}

	// A landed change cannot be asked about.
	next, cmd = r.askAI()
	r = next.(ReviewModel)
	if cmd != nil || !r.noticeE || !strings.Contains(r.notice, "the AI reviews agent_review changes") {
		t.Fatalf("landed: cmd %v notice %q", cmd, r.notice)
	}
}

// aiOffSource cannot reach the AI reviewer: the Worker's refusal is shown
// as it is, nothing is recorded, and the key stays usable.
type aiOffSource struct {
	DataSource
	err error
}

func (s aiOffSource) AskAI(context.Context, Decision) (AIAnswer, error) { return AIAnswer{}, s.err }

func TestReviewShowsAIRefusalPlainly(t *testing.T) {
	for _, tc := range []struct{ name, msg string }{
		{"unavailable", "ai_unavailable: the AI reviewer is off on this Worker (AI_REVIEW=off); a reviewer agent decides agent_review changes with 'wayglass review --for agent'"},
		{"needs human", "needs_human: policy routed decision d3f2a91c0d1 to a person; the AI reviewer cannot decide it, only a person can"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			demo, c := demoAt(time.Minute)
			m := NewReview(aiOffSource{demo, errors.New(msgOf(tc.msg))}, testOptions(c), demoAuth)
			m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
			m = step(m, m.fetch()())
			_, cmd := m.askAI()
			m = step(m, cmd())
			out := screen(m.View())
			checkFits(t, out, 100, 30)
			if !m.noticeE || m.notice != "AI reviewer: "+msgOf(tc.msg) || !strings.Contains(out, "AI reviewer: "+tc.msg[:40]) {
				t.Fatalf("the refusal should show in the footer (notice %q):\n%s", m.notice, out)
			}
			// The whole refusal is readable: it wraps instead of being cut.
			if !strings.Contains(strings.Join(strings.Fields(out), " "), "only a person can") && !strings.Contains(strings.Join(strings.Fields(out), " "), "--for agent") {
				t.Fatalf("the refusal was cut:\n%s", out)
			}
			if d, _ := m.Selected(); d.AIAdvice != nil || !d.AwaitsHuman() {
				t.Fatalf("nothing may be recorded: %+v", d)
			}
		})
	}
}

// Without a source that can ask, the key is not offered and says so.
func TestReviewWithoutAIReviewer(t *testing.T) {
	m, _ := reviewAt(t, 120, 40, demoAuth)
	if out := screen(m.View()); strings.Contains(out, "ask AI") {
		t.Fatalf("plain sample data should not offer the AI key:\n%s", out)
	}
	m = step(m, press("i"))
	if !m.noticeE || !strings.Contains(m.notice, "cannot ask the AI reviewer") {
		t.Fatalf("notice = %q", m.notice)
	}
	if errors.Is(m.err, ErrAIUnavailable) {
		t.Fatal("a missing AI reviewer is a notice, not a poll error")
	}
}

// Model-written text is flattened like an agent's before it is drawn.
func TestAIAnswerIsCleaned(t *testing.T) {
	demo, c := demoAt(time.Minute)
	hostile := AIAnswer{Kind: "advice", Model: "m", Advice: &AIAdvice{Summary: "ok\x1b[2J\nfake\n## Policy", Risks: []string{"r\u202e"}}}
	src := aiEchoSource{demo, hostile}
	m := NewReview(src, testOptions(c), demoAuth)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, m.fetch()())
	_, cmd := m.askAI()
	msg := cmd().(aiMsg)
	if s := msg.ans.Advice.Summary + msg.ans.Advice.Risks[0]; strings.ContainsAny(s, "\x1b\n\u202e") {
		t.Fatalf("answer kept control characters: %q", s)
	}
}

func msgOf(s string) string { return s }

type aiEchoSource struct {
	DataSource
	ans AIAnswer
}

func (s aiEchoSource) AskAI(context.Context, Decision) (AIAnswer, error) { return s.ans, nil }
