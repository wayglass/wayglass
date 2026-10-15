package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/policy"
)

// DemoSource is a scripted, in-memory DataSource for tests and the hidden
// --demo-data flag. It is SAMPLE DATA: nothing here talks to the Worker or
// Artifacts, and the TUI labels it as such on screen and in JSON. Its
// decisions are not hard-coded, though: each sample change runs through the
// real classifier and policy evaluator against demoPolicy.
//
// The script plays out against a clock, so `wayglass watch --demo-data`
// shows intents arriving, tags being classified and decisions landing.
type DemoSource struct {
	mu      sync.Mutex
	start   time.Time
	now     func() time.Time
	pol     *policy.Policy
	script  []demoStep
	next    int // index of the next script step to apply
	intents []Intent
	changes map[string]classify.Change // by intent id
	decs    []Decision
	events  []Event
}

type demoStep struct {
	at    time.Duration
	apply func(s *DemoSource, at time.Time)
}

// demoPolicy mirrors examples/policy.yaml, with rule names so the matched
// rule reads well on screen.
const demoPolicy = `
version: 1
classify:
  area:
    docs: ["docs/**", "**/*.md"]
    web: ["web/**"]
    auth: ["**/auth/**", "**/login*"]
    infra: ["worker/wrangler.jsonc", ".github/**"]
  data_class:
    secrets: { content: ["(?i)api[_-]?key", "art_v[0-9]+_([a-z]+_)?[0-9a-f]{40}"] }
    pii: { content: ["(?i)\\bssn\\b", "(?i)date_of_birth"] }
  risk:
    high: { lines_changed_over: 400 }
rules:
  - { name: secrets-never-land, when: { data_class: secrets }, action: block, reason: "Secrets never land." }
  - { name: infra-two-people, when: { area: infra }, action: multi_approve, approvers: 2, reason: "Infra changes affect everyone." }
  - { name: auth-needs-human, when: { area: auth }, action: human_approve, reason: "Auth is a security boundary." }
  - { name: eu-pii-quarantine, when: { jurisdiction: eu, data_class: pii }, action: quarantine, reason: "EU personal data needs DPO review." }
  - { name: docs-auto, when: { area: docs, risk: low }, action: auto_merge, reason: "Routine docs change." }
  - { name: low-risk-agent-review, when: { risk: low }, action: agent_review, reason: "Low risk; a second agent checks intent vs diff." }
default: { action: human_approve, reason: "No rule matched; a person decides." }
`

// DemoProject is the project name the sample data uses.
const DemoProject = "acme-web"

// NewDemoSource starts the script at start and reads time from now. Pass a
// fixed clock in tests; pass time.Now for a live demo.
func NewDemoSource(start time.Time, now func() time.Time) *DemoSource {
	pol, err := policy.Parse([]byte(demoPolicy))
	if err != nil {
		panic("tui: demo policy is invalid: " + err.Error()) // a programming error, caught by tests
	}
	s := &DemoSource{start: start, now: now, pol: pol, changes: map[string]classify.Change{}}
	s.script = demoScript()
	return s
}

// Sample marks this source as sample data.
func (s *DemoSource) Sample() bool { return true }

// Project implements DataSource.
func (s *DemoSource) Project() string { return DemoProject }

// Intents implements DataSource.
func (s *DemoSource) Intents(context.Context) ([]Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	return cloneIntents(s.intents), nil
}

// Overlaps implements DataSource. Like the live source, it computes the
// overlaps and their suggested re-plans from the intents.
func (s *DemoSource) Overlaps(context.Context) ([]Overlap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	return FindOverlaps(s.intents), nil
}

// Decisions implements DataSource.
func (s *DemoSource) Decisions(context.Context) ([]Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	out := make([]Decision, len(s.decs))
	for i, d := range s.decs {
		out[i] = cloneDecision(d)
	}
	return out, nil
}

// Audit implements DataSource.
func (s *DemoSource) Audit(_ context.Context, since int64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	out := []Event{}
	for _, e := range s.events {
		if e.Seq > since && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

// Decide implements DataSource.
func (s *DemoSource) Decide(_ context.Context, id string, v Verdict, by, reason string) (Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	if strings.TrimSpace(reason) == "" {
		return Decision{}, fmt.Errorf("reason is required")
	}
	if v != Approve && v != Reject {
		return Decision{}, fmt.Errorf("unknown verdict %q", v)
	}
	i := slices.IndexFunc(s.decs, func(d Decision) bool { return d.ID == id })
	if i < 0 {
		return Decision{}, fmt.Errorf("decision %s: %w", id, ErrNotFound)
	}
	if !s.decs[i].AwaitsHuman() {
		return Decision{}, fmt.Errorf("decision %s: %w", id, ErrNotPending)
	}
	s.verdict(i, by, v, reason, s.now())
	return cloneDecision(s.decs[i]), nil
}

// advance applies every script step whose time has come. Steps apply in
// script order, so audit sequence numbers never change once handed out.
func (s *DemoSource) advance() {
	elapsed := s.now().Sub(s.start)
	for s.next < len(s.script) && s.script[s.next].at <= elapsed {
		st := s.script[s.next]
		st.apply(s, s.start.Add(st.at))
		s.next++
	}
}

func (s *DemoSource) intent(id string) *Intent {
	for i := range s.intents {
		if s.intents[i].ID == id {
			return &s.intents[i]
		}
	}
	return nil
}

func (s *DemoSource) emit(at time.Time, kind, actor, intentID, decisionID, summary string) {
	s.events = append(s.events, Event{
		Seq: int64(len(s.events) + 1), At: at, Kind: kind, Actor: actor,
		IntentID: intentID, DecisionID: decisionID, Summary: summary,
	})
}

func (s *DemoSource) declare(at time.Time, in Intent, ch classify.Change) {
	in.Fork = DemoProject + "--" + in.ID
	in.Status = IntentOpen
	in.Jurisdiction = ch.Jurisdiction
	in.CreatedAt = at
	in.ActualPaths = []string{}
	s.intents = append(s.intents, in)
	s.changes[in.ID] = ch
	s.emit(at, "intent.declared", in.Agent, in.ID, "", "declared: "+in.Goal)
}

func (s *DemoSource) push(at time.Time, id string) {
	in := s.intent(id)
	ch := s.changes[id]
	for _, f := range ch.Files {
		in.ActualPaths = append(in.ActualPaths, f.Path)
	}
	s.emit(at, "intent.pushed", in.Agent, id, "", fmt.Sprintf("pushed %d file(s) to %s", len(ch.Files), in.Fork))
}

func (s *DemoSource) propose(at time.Time, intentID, decisionID string) {
	in := s.intent(intentID)
	ch := s.changes[intentID]
	pd := s.pol.Evaluate(ch)
	d := Decision{
		ID: decisionID, IntentID: intentID, Commit: decisionID[1:8],
		Tags: pd.Tags, MatchedRule: pd.Rule.ID(), Action: pd.Action, Reason: pd.Reason,
		ApprovalsNeeded: max(pd.Approvers, 1), Approvals: []Approval{}, CreatedAt: at,
	}
	for _, f := range ch.Files {
		d.Files = append(d.Files, FileStat{Path: f.Path, Added: len(f.Added), Deleted: f.Deleted})
	}
	switch pd.Action {
	case policy.AutoMerge:
		d.Status, d.DecidedBy = StatusApproved, "policy"
	case policy.AgentReview:
		d.Status = StatusPendingAgent
	case policy.Quarantine:
		d.Status = StatusQuarantined
	case policy.Block:
		d.Status, d.DecidedBy = StatusBlocked, "policy"
	default:
		d.Status = StatusPendingHuman
	}
	in.Status = IntentProposed
	s.decs = append(s.decs, d)
	s.emit(at, "change.proposed", in.Agent, intentID, decisionID, "proposed "+d.Commit)
	s.emit(at, "decision.made", "policy", intentID, decisionID,
		fmt.Sprintf("%s by rule %s: %s", d.Action, d.MatchedRule, d.Reason))
	if d.Status == StatusApproved {
		in.Status = IntentLanded
		s.emit(at, "change.landed", "policy", intentID, decisionID, "landed on trunk")
	}
}

func (s *DemoSource) verdict(i int, by string, v Verdict, reason string, at time.Time) {
	d := &s.decs[i]
	d.Approvals = append(d.Approvals, Approval{By: by, Verdict: v, Reason: reason, At: at})
	in := s.intent(d.IntentID)
	switch {
	case v == Reject:
		d.Status, d.DecidedBy = StatusRejected, by
		in.Status = IntentAbandoned
		s.emit(at, "decision.rejected", by, d.IntentID, d.ID, "rejected: "+reason)
	case d.Approved() >= d.ApprovalsNeeded:
		d.Status, d.DecidedBy = StatusApproved, by
		in.Status = IntentLanded
		s.emit(at, "decision.approved", by, d.IntentID, d.ID, "approved: "+reason)
		s.emit(at, "change.landed", by, d.IntentID, d.ID, "landed on trunk")
	default:
		s.emit(at, "decision.approval", by, d.IntentID, d.ID,
			fmt.Sprintf("approval %d of %d: %s", d.Approved(), d.ApprovalsNeeded, reason))
	}
}

func (s *DemoSource) scriptedVerdict(at time.Time, decisionID, by, reason string) {
	i := slices.IndexFunc(s.decs, func(d Decision) bool { return d.ID == decisionID })
	if i < 0 || (!s.decs[i].AwaitsHuman() && s.decs[i].Status != StatusPendingAgent) {
		return // a person already decided it during the demo
	}
	if s.decs[i].Status == StatusPendingAgent {
		d := &s.decs[i]
		d.Status, d.DecidedBy = StatusApproved, by
		s.intent(d.IntentID).Status = IntentLanded
		s.emit(at, "decision.approved", by, d.IntentID, d.ID, "agent review passed: "+reason)
		s.emit(at, "change.landed", by, d.IntentID, d.ID, "landed on trunk")
		return
	}
	s.verdict(i, by, Approve, reason, at)
}

func added(lines ...string) []string { return lines }

func lines(n int, prefix string) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("%s %d", prefix, i+1)
	}
	return out
}

// Sample intent and decision ids.
const (
	demoDocs    = "i1a2b3c4d5e"
	demoAuth    = "i3f2a91c0d1"
	demoPII     = "i0c9d8e7f61"
	demoInfra   = "i7b4e0d2a55"
	demoSession = "i5a6b7c8d90"
	demoStyle   = "i9e8d7c6b5a"
)

func demoScript() []demoStep {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	return []demoStep{
		{sec(0), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoDocs, Agent: "codex-1", Goal: "Document the session API",
				Reason: "Agents keep guessing the session endpoints.", DeclaredPaths: []string{"docs/**"}},
				classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
					{Path: "docs/session-api.md", Added: lines(42, "doc line")}}})
		}},
		{sec(0.4), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoAuth, Agent: "claude-1", Goal: "Add rate limiting to login",
				Reason:        "Credential stuffing spikes seen in last week's logs.",
				DeclaredPaths: []string{"web/login.ts", "web/middleware/**"}},
				classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
					{Path: "web/login.ts", Added: lines(18, "limit"), Deleted: 4},
					{Path: "web/middleware/ratelimit.ts", Added: lines(64, "bucket")}}})
		}},
		{sec(1.0), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoPII, Agent: "claude-2", Goal: "Store date of birth at signup",
				Reason:        "Age check required for the EU launch.",
				DeclaredPaths: []string{"web/signup.ts", "db/migrations/**"}},
				classify.Change{Jurisdiction: "eu", Files: []classify.FileChange{
					{Path: "web/signup.ts", Added: added("const date_of_birth = form.get('dob')", "await save({ date_of_birth })"), Deleted: 1},
					{Path: "db/migrations/0042_dob.sql", Added: added("ALTER TABLE users ADD COLUMN date_of_birth DATE;")}}})
		}},
		{sec(1.6), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoInfra, Agent: "codex-2", Goal: "Pin Worker compatibility date",
				Reason:        "Unpinned date changed runtime behaviour in staging.",
				DeclaredPaths: []string{"worker/wrangler.jsonc"}},
				classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
					{Path: "worker/wrangler.jsonc", Added: added(`"compatibility_date": "2026-09-30"`), Deleted: 1}}})
		}},
		{sec(2.2), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoSession, Agent: "claude-3", Goal: "Refactor session cookie handling",
				Reason:        "Cookie flags are set in three places.",
				DeclaredPaths: []string{"web/session.ts", "web/login.ts"}},
				classify.Change{Jurisdiction: "us"})
			s.emit(at, "intent.overlap", "wayglass", demoSession, "", "overlaps "+demoAuth+" on web/login.ts; suggest landing "+shortID(demoAuth)+" first")
		}},
		{sec(3.2), func(s *DemoSource, at time.Time) { s.push(at, demoDocs) }},
		{sec(3.8), func(s *DemoSource, at time.Time) { s.propose(at, demoDocs, "d1c2d3e4f5a") }},
		{sec(4.6), func(s *DemoSource, at time.Time) { s.push(at, demoAuth) }},
		{sec(5.4), func(s *DemoSource, at time.Time) { s.propose(at, demoAuth, "d3f2a91c0d1") }},
		{sec(6.0), func(s *DemoSource, at time.Time) {
			s.declare(at, Intent{ID: demoStyle, Agent: "codex-1", Goal: "Tidy header spacing",
				Reason: "Header overlaps the banner on small screens.", DeclaredPaths: []string{"web/header.css"}},
				classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
					{Path: "web/header.css", Added: lines(6, "rule"), Deleted: 5}}})
		}},
		{sec(6.4), func(s *DemoSource, at time.Time) { s.push(at, demoPII) }},
		{sec(6.8), func(s *DemoSource, at time.Time) { s.propose(at, demoPII, "d0c9d8e7f61") }},
		{sec(7.4), func(s *DemoSource, at time.Time) { s.push(at, demoInfra) }},
		{sec(8.0), func(s *DemoSource, at time.Time) { s.propose(at, demoInfra, "d7b4e0d2a55") }},
		{sec(9.5), func(s *DemoSource, at time.Time) {
			s.scriptedVerdict(at, "d7b4e0d2a55", "maya", "Date matches the staging fix.")
		}},
		{sec(10.0), func(s *DemoSource, at time.Time) { s.push(at, demoStyle) }},
		{sec(10.5), func(s *DemoSource, at time.Time) { s.propose(at, demoStyle, "d9e8d7c6b5a") }},
		{sec(12.0), func(s *DemoSource, at time.Time) {
			s.scriptedVerdict(at, "d9e8d7c6b5a", "agent:reviewer", "Diff matches the declared intent.")
		}},
	}
}

// DemoDuration is when the last scripted step happens, the AI reviewer's
// steps included.
func DemoDuration() time.Duration {
	d := demoScript()[len(demoScript())-1].at
	for _, st := range demoAIScript() {
		d = max(d, st.at)
	}
	return d
}

func cloneIntents(in []Intent) []Intent {
	out := make([]Intent, len(in))
	for i, x := range in {
		x.DeclaredPaths = slices.Clone(x.DeclaredPaths)
		x.ActualPaths = slices.Clone(x.ActualPaths)
		out[i] = x
	}
	return out
}

func cloneDecision(d Decision) Decision {
	d.Approvals = slices.Clone(d.Approvals)
	if d.Escalation != nil {
		e := *d.Escalation
		e.Concerns = slices.Clone(e.Concerns)
		d.Escalation = &e
	}
	if d.AIAdvice != nil {
		a := *d.AIAdvice
		a.Risks, a.Concerns, a.Questions = slices.Clone(a.Risks), slices.Clone(a.Concerns), slices.Clone(a.Questions)
		d.AIAdvice = &a
	}
	d.Files = slices.Clone(d.Files)
	d.Tags.Areas = slices.Clone(d.Tags.Areas)
	d.Tags.DataClasses = slices.Clone(d.Tags.DataClasses)
	d.Tags.Evidence = slices.Clone(d.Tags.Evidence)
	return d
}
