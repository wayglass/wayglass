package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/intent"
	"github.com/wayglass/wayglass/internal/policy"
)

// The types in this file are the frontend's whole data model. Every field
// has a json tag and every slice is non-nil, so JavaScript never sees null
// where it expects an array.

// Session is what the window needs before it can show a project.
type Session struct {
	LoggedIn bool   `json:"logged_in"`
	Problem  string `json:"problem,omitempty"` // why not logged in, in words
	Worker   string `json:"worker"`
	// Identity is who approvals are recorded as (X-Wayglass-Agent): the
	// display name if set, else DefaultIdentity.
	Identity string `json:"identity"`
	// DefaultIdentity is the CLI's identity: login or $WAYGLASS_AGENT, else
	// user@host.
	DefaultIdentity string `json:"default_identity"`
	// DisplayName is the name set in the window; "" means none.
	DisplayName   string   `json:"display_name"`
	Projects      []string `json:"projects"`
	Current       string   `json:"current"`
	Notifications bool     `json:"notifications"`
	PollMillis    int64    `json:"poll_ms"`
	// Version is the build ("v0.2.0", or "dev" for an untagged build).
	Version string `json:"version"`
}

// Snapshot is one consistent view of a project.
type Snapshot struct {
	Project   string         `json:"project"`
	FetchedAt string         `json:"fetched_at"`
	Counters  Counters       `json:"counters"`
	NeedsYou  []NeedsYou     `json:"needs_you"`
	Intents   []IntentView   `json:"intents"`
	Overlaps  []OverlapView  `json:"overlaps"`
	Decisions []DecisionView `json:"decisions"`
	Events    []EventView    `json:"events"` // newest first
	// Warnings are calm notes about what could not be read, such as a
	// Worker that predates GET /v1/decisions; the rest still renders.
	Warnings []string `json:"warnings"`
}

// Counters is the strip across the top of the window.
type Counters struct {
	Agents       int            `json:"agents"`        // distinct agents that ever declared
	ActiveAgents int            `json:"active_agents"` // agents with an open or proposed intent
	Intents      int            `json:"intents"`
	OpenIntents  int            `json:"open_intents"`
	Landed       int            `json:"landed"`
	Decisions    int            `json:"decisions"`
	ByAction     map[string]int `json:"by_action"` // all six actions, zero included
	NeedsYou     int            `json:"needs_you"`
	Overlaps     int            `json:"overlaps"`
}

// IntentView is one row of the swarm.
type IntentView struct {
	ID            string        `json:"id"`
	Agent         string        `json:"agent"`
	Goal          string        `json:"goal"`
	Reason        string        `json:"reason"`
	Status        string        `json:"status"`
	Active        bool          `json:"active"`
	DeclaredPaths []string      `json:"declared_paths"`
	ActualPaths   []string      `json:"actual_paths"`
	Head          string        `json:"head"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	Latest        *DecisionView `json:"latest,omitempty"` // latest decision, if proposed
	OverlapsWith  []string      `json:"overlaps_with"`
	Mine          bool          `json:"mine"` // authored by the app's identity
	// Stale: active, but no push for the Worker's STALE_INTENT_HOURS.
	Stale bool `json:"stale"`
	// AbandonedReason is why it was abandoned, and by whom.
	AbandonedReason string `json:"abandoned_reason"`
	AbandonedBy     string `json:"abandoned_by"`
}

// DecisionView is a policy decision with everything a person needs to judge it.
type DecisionView struct {
	ID              string         `json:"id"`
	IntentID        string         `json:"intent_id"`
	Commit          string         `json:"commit"`
	Action          string         `json:"action"`
	Status          string         `json:"status"`
	MatchedRule     string         `json:"matched_rule"`
	Reason          string         `json:"reason"`
	Tags            TagsView       `json:"tags"`
	ApprovalsNeeded int            `json:"approvals_needed"`
	Approvals       []ApprovalView `json:"approvals"`
	Files           []FileView     `json:"files"`
	Added           int            `json:"added"`
	Deleted         int            `json:"deleted"`
	// LinesUnknown: only paths were recorded (proposed before the Worker
	// counted lines), so Added and Deleted are unknown, not zero.
	LinesUnknown bool   `json:"lines_unknown"`
	DecidedBy    string `json:"decided_by"`
	CreatedAt    string `json:"created_at"`
	AwaitsHuman  bool   `json:"awaits_human"`
	// AIModel names the AI reviewer's model when it decided this change
	// (approved it or escalated it to a person); "" otherwise.
	AIModel string `json:"ai_model"`
	// Escalation is set once the AI reviewer sent the change to a person.
	Escalation *EscalationView `json:"escalation,omitempty"`
	// AIAdvice is the AI reviewer's advice on a change a person decides.
	// Advisory: it decides nothing.
	AIAdvice *AdviceView `json:"ai_advice,omitempty"`
}

// EscalationView is the AI reviewer handing a change to a person: who, why
// and what it saw. Model-written text, shown as text like an agent's.
type EscalationView struct {
	By         string   `json:"by"`
	Model      string   `json:"model"`
	Reason     string   `json:"reason"`
	Concerns   []string `json:"concerns"`
	FromAction string   `json:"from_action"`
}

// AdviceView is the AI reviewer's advice on a change a person must decide.
type AdviceView struct {
	Model     string   `json:"model"`
	Summary   string   `json:"summary"`
	Risks     []string `json:"risks"`
	Concerns  []string `json:"concerns"`
	Questions []string `json:"questions"`
}

// TagsView is the four tag kinds plus the evidence behind them.
type TagsView struct {
	Areas        []string       `json:"areas"`
	DataClasses  []string       `json:"data_classes"`
	Risk         string         `json:"risk"`
	Jurisdiction string         `json:"jurisdiction"`
	Lines        int            `json:"lines"`
	Files        int            `json:"files"`
	Evidence     []EvidenceView `json:"evidence"`
}

// EvidenceView says why a tag was applied.
type EvidenceView struct {
	Tag    string `json:"tag"`
	Value  string `json:"value"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// ApprovalView is one verdict on a decision.
type ApprovalView struct {
	By      string `json:"by"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
	// Source is how the decider signed in ("admin", "key", "access", "ai").
	Source string `json:"source"`
	// AI is true for the Workers AI reviewer; Who is then "AI reviewer (<model>)".
	AI  bool   `json:"ai"`
	Who string `json:"who"`
}

// FileView is one line of the diff stat.
type FileView struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// NeedsYou is a decision policy routed to a person, with its intent.
type NeedsYou struct {
	Decision DecisionView `json:"decision"`
	Intent   IntentView   `json:"intent"`
	// Own is true when the app's identity wrote the change; it cannot decide it.
	Own bool `json:"own"`
}

// OverlapView is two open intents whose paths intersect.
type OverlapView struct {
	Older      string   `json:"older"`
	Newer      string   `json:"newer"`
	OlderGoal  string   `json:"older_goal"`
	NewerGoal  string   `json:"newer_goal"`
	OlderAgent string   `json:"older_agent"`
	NewerAgent string   `json:"newer_agent"`
	Paths      []string `json:"paths"`
	Suggestion string   `json:"suggestion"`
}

// EventView is one audit record as a timeline line.
type EventView struct {
	Seq        int    `json:"seq"`
	At         string `json:"at"`
	Kind       string `json:"kind"`
	Actor      string `json:"actor"`
	IntentID   string `json:"intent_id"`
	DecisionID string `json:"decision_id"`
	Summary    string `json:"summary"`
}

// DecideOutcome is the result of Approve or Reject. A refusal (by the app or
// the Worker) is an outcome, not an error, so the window can show it inline
// next to the reason the person typed.
type DecideOutcome struct {
	OK       bool          `json:"ok"`
	Code     string        `json:"code,omitempty"`
	Message  string        `json:"message,omitempty"`
	Decision *DecisionView `json:"decision,omitempty"`
}

// AIOutcome is the result of AskAI: what the AI reviewer said, or why the
// Worker refused (the reviewer off, a change only a person may decide).
type AIOutcome struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Kind is "review" (a verdict on an agent_review change) or "advice".
	Kind     string        `json:"kind,omitempty"`
	Model    string        `json:"model,omitempty"`
	Verdict  string        `json:"verdict,omitempty"` // review: approve or escalate
	Reason   string        `json:"reason,omitempty"`
	Concerns []string      `json:"concerns,omitempty"`
	Advice   *AdviceView   `json:"advice,omitempty"`
	Decision *DecisionView `json:"decision,omitempty"`
}

// activeStatuses are the intent statuses that still compete for paths
// (internal/cli/intent.go and the Worker's ACTIVE_STATUSES).
var activeStatuses = []string{"open", "proposed"}

// build turns the Worker's lists into a Snapshot. identity marks the
// intents the app's own identity wrote.
func build(project, identity string, intents []api.Intent, decisions []api.Decision, events []EventView, now time.Time) (Snapshot, error) {
	s := Snapshot{
		Project: project, FetchedAt: now.UTC().Format(time.RFC3339),
		NeedsYou: []NeedsYou{}, Intents: []IntentView{}, Overlaps: []OverlapView{}, Decisions: []DecisionView{},
		Events: nonNil(events), Warnings: []string{},
		Counters: Counters{ByAction: map[string]int{}},
	}
	for _, a := range policy.Actions() {
		s.Counters.ByAction[string(a)] = 0
	}

	latest := map[string]DecisionView{} // the Worker lists newest last
	for _, d := range decisions {
		v, err := decisionView(d)
		if err != nil {
			return Snapshot{}, err
		}
		s.Decisions = append(s.Decisions, v)
		s.Counters.ByAction[v.Action]++
		latest[v.IntentID] = v
	}
	s.Counters.Decisions = len(s.Decisions)
	slices.Reverse(s.Decisions) // newest first for the window

	agents, active := map[string]bool{}, map[string]bool{}
	byID := map[string]int{}
	for _, in := range intents {
		v := IntentView{
			ID: in.ID, Agent: in.Agent, Goal: in.Goal, Reason: in.Reason, Status: in.Status,
			Active:        slices.Contains(activeStatuses, in.Status),
			DeclaredPaths: nonNil(in.DeclaredPaths), ActualPaths: nonNil(in.ActualPaths),
			Head: in.Head(), CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
			OverlapsWith: []string{}, Mine: sameIdentity(in.Agent, identity),
			AbandonedReason: deref(in.AbandonedReason), AbandonedBy: deref(in.AbandonedBy),
		}
		v.Stale = in.Stale && v.Active
		if d, ok := latest[in.ID]; ok {
			v.Latest = &d
		}
		agents[in.Agent] = true
		if v.Active {
			active[in.Agent] = true
			s.Counters.OpenIntents++
		}
		if in.Status == "landed" {
			s.Counters.Landed++
		}
		byID[in.ID] = len(s.Intents)
		s.Intents = append(s.Intents, v)
	}
	s.Counters.Agents, s.Counters.ActiveAgents, s.Counters.Intents = len(agents), len(active), len(s.Intents)

	s.Overlaps = overlaps(intents)
	s.Counters.Overlaps = len(s.Overlaps)
	for _, o := range s.Overlaps {
		s.Intents[byID[o.Older]].OverlapsWith = append(s.Intents[byID[o.Older]].OverlapsWith, o.Newer)
		s.Intents[byID[o.Newer]].OverlapsWith = append(s.Intents[byID[o.Newer]].OverlapsWith, o.Older)
	}

	// Newest activity first; active intents above finished ones.
	slices.SortStableFunc(s.Intents, func(a, b IntentView) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return strings.Compare(cmp.Or(b.UpdatedAt, b.CreatedAt), cmp.Or(a.UpdatedAt, a.CreatedAt))
	})

	for _, in := range s.Intents {
		if in.Latest == nil || !in.Latest.AwaitsHuman || !in.Active {
			continue
		}
		s.NeedsYou = append(s.NeedsYou, NeedsYou{Decision: *in.Latest, Intent: in, Own: in.Mine})
	}
	// Oldest waiting first: it has waited longest.
	slices.SortStableFunc(s.NeedsYou, func(a, b NeedsYou) int {
		return strings.Compare(a.Decision.CreatedAt, b.Decision.CreatedAt)
	})
	for _, n := range s.NeedsYou {
		if !n.Own {
			s.Counters.NeedsYou++
		}
	}
	return s, nil
}

func decisionView(d api.Decision) (DecisionView, error) {
	var tags classify.Tags
	if len(d.Tags) > 0 && string(d.Tags) != "null" {
		if err := json.Unmarshal(d.Tags, &tags); err != nil {
			return DecisionView{}, fmt.Errorf("decision %s: reading tags: %w", d.ID, err)
		}
	}
	v := DecisionView{
		ID: d.ID, IntentID: d.IntentID, Commit: deref(d.CommitSHA), Action: d.Action, Status: d.Status,
		MatchedRule: cmp.Or(deref(d.MatchedRule), "default"), Reason: d.Reason,
		ApprovalsNeeded: d.ApprovalsNeeded, Approvals: []ApprovalView{}, Files: []FileView{},
		DecidedBy: deref(d.DecidedBy), CreatedAt: d.CreatedAt,
		Tags: TagsView{
			Areas: nonNil(tags.Areas), DataClasses: nonNil(tags.DataClasses),
			Risk: tags.Risk, Jurisdiction: tags.Jurisdiction,
			Lines: tags.LinesChanged, Files: tags.FilesChanged, Evidence: []EvidenceView{},
		},
	}
	v.AwaitsHuman = policy.Action(d.Action).NeedsHuman() &&
		(d.Status == api.StatusPendingHuman || d.Status == api.StatusQuarantined)
	for _, e := range tags.Evidence {
		v.Tags.Evidence = append(v.Tags.Evidence, EvidenceView{Tag: e.Tag, Value: e.Value, Path: e.Path, Detail: e.Detail})
	}
	for _, f := range d.Detail.Files {
		v.Files = append(v.Files, FileView(f))
		v.Added += f.Added
		v.Deleted += f.Deleted
	}
	if len(v.Files) == 0 && len(d.Paths) > 0 { // decided before the Worker recorded line counts
		v.LinesUnknown = true
		for _, p := range d.Paths {
			v.Files = append(v.Files, FileView{Path: p})
		}
	}
	for _, a := range d.Approvals {
		ap := ApprovalView{By: a.By, Verdict: a.Verdict, Reason: a.Reason, At: a.At, Source: a.IdentitySource, Who: a.By}
		if a.IsAI() {
			ap.AI, ap.Who = true, aiName(api.AIModel(a.By))
			if v.AIModel == "" {
				v.AIModel = api.AIModel(a.By)
			}
		}
		v.Approvals = append(v.Approvals, ap)
	}
	if api.IsAIReviewer(v.DecidedBy) {
		v.AIModel = api.AIModel(v.DecidedBy)
	}
	if e := d.Detail.Escalation; e != nil {
		v.Escalation = &EscalationView{By: e.By, Model: e.ModelOf(), Reason: e.Reason, Concerns: nonNil(e.Concerns), FromAction: e.FromAction}
		v.AIModel = cmp.Or(e.ModelOf(), v.AIModel)
	}
	if a := d.Detail.AIAdvice; a != nil {
		v.AIAdvice = adviceView(*a)
	}
	return v, nil
}

func adviceView(a api.AIAdvice) *AdviceView {
	return &AdviceView{Model: a.ModelOf(), Summary: a.Summary, Risks: nonNil(a.Risks), Concerns: nonNil(a.Concerns), Questions: nonNil(a.Questions)}
}

// aiName is the AI reviewer as the window names it.
func aiName(model string) string {
	if model == "" {
		return "AI reviewer"
	}
	return "AI reviewer (" + model + ")"
}

// overlaps pairs open intents whose declared or actual paths intersect: the
// same check the Worker runs on declare and push, and the same suggested
// order the TUI shows (the older intent lands first).
func overlaps(all []api.Intent) []OverlapView {
	active := slices.DeleteFunc(slices.Clone(all), func(i api.Intent) bool { return !slices.Contains(activeStatuses, i.Status) })
	parent := map[string]string{}
	for _, i := range active {
		parent[i.ID] = deref(i.ParentID)
	}
	inStack := func(child, ancestor string) bool {
		for p, n := parent[child], 0; p != "" && n < 64; p, n = parent[p], n+1 {
			if p == ancestor {
				return true
			}
		}
		return false
	}
	out := []OverlapView{}
	for i, x := range active {
		for _, y := range active[i+1:] {
			// A stack builds on its ancestors' heads on purpose (declare --on,
			// session fork); the Worker does not report it, so neither does the app.
			if inStack(x.ID, y.ID) || inStack(y.ID, x.ID) {
				continue
			}
			r, ok := intent.SuggestReplan(replanSide(x), replanSide(y))
			if !ok {
				continue
			}
			older, newer := x, y
			if y.CreatedAt < x.CreatedAt {
				older, newer = y, x
			}
			out = append(out, OverlapView{
				Older: older.ID, Newer: newer.ID, OlderGoal: older.Goal, NewerGoal: newer.Goal,
				OlderAgent: older.Agent, NewerAgent: newer.Agent, Paths: r.Paths,
				Suggestion: r.Summary, // the view labels it "Suggested:"
			})
		}
	}
	return out
}

// eventView turns an audit record into one timeline line.
func eventView(e api.AuditEvent) EventView {
	return EventView{
		Seq: e.Seq, At: e.At, Kind: e.Kind, Actor: e.Actor,
		IntentID: deref(e.IntentID), DecisionID: deref(e.DecisionID), Summary: summarize(e),
	}
}

// summarize mirrors the TUI's event feed (internal/cli/livesource.go).
func summarize(e api.AuditEvent) string {
	var d struct {
		Goal        string   `json:"goal"`
		Paths       []string `json:"paths"`
		Overlaps    []any    `json:"overlaps"`
		Action      string   `json:"action"`
		MatchedRule string   `json:"matched_rule"`
		Reason      string   `json:"reason"`
		Commit      string   `json:"commit"`
		Name        string   `json:"name"`
		// The AI reviewer's entries.
		IdentitySource string   `json:"identity_source"`
		Concerns       []string `json:"concerns"`
		Summary        string   `json:"summary"`
	}
	_ = json.Unmarshal(e.Data, &d) // best effort; the kind still shows
	switch e.Kind {
	case "project.created":
		return "project " + d.Name + " created"
	case "intent.declared":
		return "declared: " + d.Goal
	case "intent.pushed":
		return fmt.Sprintf("pushed %d file(s)", len(d.Paths))
	case "intent.overlap":
		return fmt.Sprintf("overlaps %d open intent(s)", len(d.Overlaps))
	case "decision.proposed":
		return fmt.Sprintf("%s by %s: %s", d.Action, cmp.Or(d.MatchedRule, "default"), d.Reason)
	case "decision.approve":
		if d.IdentitySource == api.SourceAI || api.IsAIReviewer(e.Actor) {
			return "approved by AI reviewer: " + d.Reason
		}
		return "approved: " + d.Reason
	case "decision.escalated":
		s := "escalated to a person by AI reviewer: " + d.Reason
		if len(d.Concerns) > 0 {
			s += " (concerns: " + strings.Join(d.Concerns, "; ") + ")"
		}
		return s
	case "decision.ai_advice":
		return "AI advice: " + cmp.Or(d.Summary, d.Reason)
	case "decision.reject":
		return "rejected: " + d.Reason
	case "intent.landed":
		return "landed " + shortSHA(d.Commit) + " on trunk"
	case "land.mismatch":
		return "trunk push did not match the approved change"
	case "land.cancelled":
		return "land cancelled: " + d.Reason
	}
	return strings.ReplaceAll(e.Kind, ".", " ")
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// sameIdentity compares agent names the way a person would: case and
// surrounding space do not make a different author.
func sameIdentity(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && strings.EqualFold(a, b)
}

// replanSide is what intent.SuggestReplan needs from a Worker intent, the
// same input the CLI's watch uses, so both suggest the same re-plan.
func replanSide(i api.Intent) intent.Side {
	t, _ := time.Parse(time.RFC3339Nano, i.CreatedAt)
	return intent.Side{ID: i.ID, Status: i.Status, CreatedAt: t, Declared: i.DeclaredPaths, Actual: i.ActualPaths}
}
