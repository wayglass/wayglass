package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/intent"
	"github.com/wayglass/wayglass/internal/policy"
	"github.com/wayglass/wayglass/internal/textsafe"
)

// DataSource is everything the TUI reads from and writes to the Worker. It is
// deliberately narrow so the real API client can satisfy it with thin
// adapters, and so tests and --demo-data can use DemoSource instead.
type DataSource interface {
	// Project names the project being watched.
	Project() string
	// Intents lists the project's intents, any status.
	Intents(ctx context.Context) ([]Intent, error)
	// Overlaps lists live path overlaps between open intents.
	Overlaps(ctx context.Context) ([]Overlap, error)
	// Decisions lists policy decisions, newest last.
	Decisions(ctx context.Context) ([]Decision, error)
	// Audit returns audit events with Seq > since, oldest first, at most limit.
	Audit(ctx context.Context, since int64, limit int) ([]Event, error)
	// Decide records a person's verdict on a decision routed to them. The
	// reason is required: every decision carries its why.
	Decide(ctx context.Context, decisionID string, v Verdict, by, reason string) (Decision, error)
}

// Verdict is a person's answer to a change policy routed to them.
type Verdict string

// Verdicts.
const (
	Approve Verdict = "approve"
	Reject  Verdict = "reject"
)

// Intent statuses, as the Worker reports them.
const (
	IntentOpen      = "open"
	IntentProposed  = "proposed"
	IntentLanded    = "landed"
	IntentAbandoned = "abandoned"
)

// Decision statuses, as the Worker reports them.
const (
	StatusApproved     = "approved"
	StatusRejected     = "rejected"
	StatusPendingAgent = "pending_agent"
	StatusPendingHuman = "pending_human"
	StatusQuarantined  = "quarantined"
	StatusBlocked      = "blocked"
	StatusSuperseded   = "superseded"
)

// Intent is one declared unit of agent work.
type Intent struct {
	ID            string    `json:"id"`
	Agent         string    `json:"agent"`
	Goal          string    `json:"goal"`
	Reason        string    `json:"reason"`
	DeclaredPaths []string  `json:"declared_paths"`
	ActualPaths   []string  `json:"actual_paths"`
	Fork          string    `json:"fork"`
	Status        string    `json:"status"`
	Jurisdiction  string    `json:"jurisdiction,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// Parent is the intent this one is stacked on ("" when forked from
	// trunk). A stacked pair is not an overlap: the child builds on the
	// parent's head on purpose, and the Worker skips that pair too.
	Parent string `json:"parent,omitempty"`
	// Family is the author's declared model family ("" when none).
	Family string `json:"family,omitempty"`
	// Stale: open or proposed with no push for the Worker's
	// STALE_INTENT_HOURS. Shown, never acted on.
	Stale bool `json:"stale,omitempty"`
	// AbandonedReason is why its author or the admin dropped it.
	AbandonedReason string `json:"abandoned_reason,omitempty"`
}

// StatusText is the status as watch shows it: "stale" stands in for an
// active intent's status once it has gone quiet.
func (i Intent) StatusText() string {
	if i.Stale && i.Active() {
		return "stale"
	}
	return i.Status
}

// Overlap is two open intents that declared or touched the same paths, with
// a suggested re-plan so they do not fail at merge time. The re-plan is
// computed by intent.SuggestReplan from paths and statuses; it is a
// suggestion (Replan.Suggested), never a claim that something was changed.
type Overlap struct {
	A      string        `json:"a"` // intent id
	B      string        `json:"b"` // intent id
	Paths  []string      `json:"paths"`
	Replan intent.Replan `json:"replan"`
	At     time.Time     `json:"at"` // when the newer of the two was declared
}

// FindOverlaps pairs the open and proposed intents whose declared or actual
// paths intersect, the same check the Worker runs on declare and push, and
// suggests a re-plan for each pair.
func FindOverlaps(intents []Intent) []Overlap {
	active := slices.DeleteFunc(slices.Clone(intents), func(i Intent) bool { return !i.Active() })
	parent := map[string]string{}
	for _, i := range active {
		parent[i.ID] = i.Parent
	}
	// A stack shares its ancestors' code on purpose, at any depth.
	inStack := func(child, ancestor string) bool {
		for p, n := parent[child], 0; p != "" && n < 64; p, n = parent[p], n+1 {
			if p == ancestor {
				return true
			}
		}
		return false
	}
	out := []Overlap{}
	for i, x := range active {
		for _, y := range active[i+1:] {
			if inStack(x.ID, y.ID) || inStack(y.ID, x.ID) {
				continue
			}
			r, ok := intent.SuggestReplan(x.side(), y.side())
			if !ok {
				continue
			}
			a, b := x, y
			if r.First == y.ID {
				a, b = y, x
			}
			out = append(out, Overlap{A: a.ID, B: b.ID, Paths: r.Paths, Replan: r, At: later(x.CreatedAt, y.CreatedAt)})
		}
	}
	return out
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Active reports whether the intent is still open or proposed.
func (i Intent) Active() bool { return i.Status == IntentOpen || i.Status == IntentProposed }

func (i Intent) side() intent.Side {
	return intent.Side{ID: i.ID, Status: i.Status, CreatedAt: i.CreatedAt, Declared: i.DeclaredPaths, Actual: i.ActualPaths}
}

// Undeclared lists actual paths no declared pattern covers.
func (i Intent) Undeclared() []string {
	out := []string{}
	for _, a := range i.ActualPaths {
		if !slices.ContainsFunc(i.DeclaredPaths, func(d string) bool { return intent.PatternsIntersect(d, a) }) {
			out = append(out, a)
		}
	}
	return out
}

// FileStat is one file in a change's diff summary.
type FileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// Approval is one person's, agent's or AI reviewer's verdict on a decision.
type Approval struct {
	By      string    `json:"by"`
	Verdict Verdict   `json:"verdict"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
	// Source is how the decider signed in: "admin", "key", "access" or
	// "ai" (the Workers AI reviewer). Empty when the Worker did not say.
	Source string `json:"source,omitempty"`
}

// AI reports whether the AI reviewer gave this verdict.
func (a Approval) AI() bool { return a.Source == SourceAI || IsAIReviewer(a.By) }

// SourceAI is the identity source of the Workers AI reviewer.
const SourceAI = "ai"

// aiPrefix starts the AI reviewer's name on the record: "workers-ai:<model>".
const aiPrefix = "workers-ai:"

// IsAIReviewer reports whether a decider name is the AI reviewer's.
func IsAIReviewer(by string) bool { return AIModel(by) != "" }

// AIModel returns the model behind an AI reviewer's name, or "" for anyone else.
func AIModel(by string) string {
	for _, prefix := range []string{"workers-ai:", "anthropic:", "openai:"} {
		if model, ok := strings.CutPrefix(by, prefix); ok {
			return model
		}
	}
	return ""
}

// Escalation is the AI reviewer handing an agent_review change to a person:
// its reason and the concerns it listed. The decision is then human_approve.
type Escalation struct {
	By         string    `json:"by"`
	Model      string    `json:"model,omitempty"`
	Reason     string    `json:"reason"`
	Concerns   []string  `json:"concerns"`
	FromAction string    `json:"from_action,omitempty"`
	RuleReason string    `json:"rule_reason,omitempty"`
	At         time.Time `json:"at,omitzero"`
}

// AIAdvice is the AI reviewer's advisory read of a change a person must
// decide. It decides nothing: every surface labels it as advice.
type AIAdvice struct {
	Model     string    `json:"model,omitempty"`
	Summary   string    `json:"summary"`
	Risks     []string  `json:"risks"`
	Concerns  []string  `json:"concerns"`
	Questions []string  `json:"questions"`
	At        time.Time `json:"at,omitzero"`
}

// Decision is policy's answer for one proposed change.
type Decision struct {
	ID              string        `json:"id"`
	IntentID        string        `json:"intent_id"`
	Commit          string        `json:"commit,omitempty"`
	Tags            classify.Tags `json:"tags"`
	MatchedRule     string        `json:"matched_rule"`
	Action          policy.Action `json:"action"`
	Reason          string        `json:"reason"` // the matched rule's reason
	Status          string        `json:"status"`
	ApprovalsNeeded int           `json:"approvals_needed"`
	Approvals       []Approval    `json:"approvals"`
	DecidedBy       string        `json:"decided_by,omitempty"`
	Files           []FileStat    `json:"files"`
	// LinesUnknown: the Worker recorded paths but no line counts (decisions
	// proposed before it counted lines), so Added and Deleted are not zero,
	// just unknown.
	LinesUnknown bool      `json:"lines_unknown,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// ReviewerDifferentFamily: policy needs an approver from a model
	// family other than the author's.
	ReviewerDifferentFamily bool `json:"reviewer_different_family,omitzero"`
	// IntentStatus is the owning intent's status when the source knows it;
	// a decision of an abandoned intent waits for no one.
	IntentStatus string `json:"intent_status,omitempty"`
	// Escalation is set once the AI reviewer sent the change to a person.
	Escalation *Escalation `json:"escalation,omitempty"`
	// AIAdvice is the AI reviewer's advice on a change a person decides.
	AIAdvice *AIAdvice `json:"ai_advice,omitempty"`
}

// Dropped reports whether the decision's intent was abandoned.
func (d Decision) Dropped() bool { return d.IntentStatus == IntentAbandoned }

// AIDecided returns the AI reviewer's model when it decided this change
// (approved it, or escalated it to a person), and whether it did.
func (d Decision) AIDecided() (model string, ok bool) {
	if d.Escalation != nil {
		return cmpOr(d.Escalation.Model, AIModel(d.Escalation.By)), true
	}
	if IsAIReviewer(d.DecidedBy) {
		return AIModel(d.DecidedBy), true
	}
	for _, a := range d.Approvals {
		if a.AI() {
			return AIModel(a.By), true
		}
	}
	return "", false
}

// AIInvolved reports whether the AI reviewer decided, escalated or advised
// on this change: the rows that carry the AI marker.
func (d Decision) AIInvolved() bool {
	_, ok := d.AIDecided()
	return ok || d.AIAdvice != nil
}

// Approved counts approve verdicts so far.
func (d Decision) Approved() int {
	n := 0
	for _, a := range d.Approvals {
		if a.Verdict == Approve {
			n++
		}
	}
	return n
}

// AwaitsHuman reports whether the decision sits in a person's queue.
func (d Decision) AwaitsHuman() bool {
	return !d.Dropped() && d.Action.NeedsHuman() && (d.Status == StatusPendingHuman || d.Status == StatusQuarantined)
}

// Event is one audit log record.
type Event struct {
	Seq        int64     `json:"seq"`
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	Actor      string    `json:"actor"`
	IntentID   string    `json:"intent_id,omitempty"`
	DecisionID string    `json:"decision_id,omitempty"`
	Summary    string    `json:"summary"`
}

// MinutesSavedPerChange is the assumed human review time saved by each
// change that lands without a person. It is an estimate and is always
// labelled as one.
const MinutesSavedPerChange = 15

// Counters are the numbers on screen.
type Counters struct {
	Agents      int `json:"agents"`
	Intents     int `json:"intents"`
	OpenIntents int `json:"open_intents"` // open or proposed
	Decisions   int `json:"decisions"`
	// ByAction counts decisions per policy action; every action is present.
	ByAction         map[policy.Action]int `json:"by_action"`
	AutoLanded       int                   `json:"auto_landed"`    // auto_merge or agent_review
	HumanRouted      int                   `json:"human_routed"`   // human_approve, multi_approve, quarantine
	AwaitingHuman    int                   `json:"awaiting_human"` // still in a person's queue
	Blocked          int                   `json:"blocked"`
	Overlaps         int                   `json:"overlaps"`
	TimeSavedMinutes int                   `json:"time_saved_minutes"`
	TimeSavedBasis   string                `json:"time_saved_basis"`
	// LastEventAt is the newest audit event's time, absent before any.
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	// AIApprovals, AIEscalations and AIAdvised count what the AI reviewer
	// did: changes it approved, changes it sent to a person, and changes
	// routed to a person it advised on.
	AIApprovals   int `json:"ai_approvals"`
	AIEscalations int `json:"ai_escalations"`
	AIAdvised     int `json:"ai_advised"`
	// Cost is absent when the source cannot measure it (sample data, or a
	// Worker without GET /v1/cost). Never estimated from anything else.
	Cost *Cost `json:"cost,omitempty"`
}

// Cost is what the project has cost in Artifacts operations, as far as the
// Worker can see: its own binding calls (client git traffic to forks is not
// counted). The dollar figures are estimates from published pricing.
type Cost struct {
	Ops          int     `json:"ops"`
	TokensMinted int     `json:"tokens_minted"`
	EstUSD       float64 `json:"est_usd"`
	// Landed is how many metered intents landed; the per-change figures
	// average them. Zero when none has.
	Landed       int     `json:"landed"`
	PerLandedOps float64 `json:"per_landed_ops"`
	PerLandedUSD float64 `json:"per_landed_est_usd"`
	Basis        string  `json:"basis"`
	// AI is the AI reviewer's measured usage; nil when the Worker does not
	// count it.
	AI *AICost `json:"ai,omitempty"`
}

// AICost is what the AI reviewer cost, as far as the Worker counted it.
type AICost struct {
	Calls  int     `json:"calls"`
	Tokens int     `json:"tokens"` // prompt plus completion
	EstUSD float64 `json:"est_usd"`
	// TodayCalls and TodayUSD are the same for the current UTC day; Today
	// says whether the Worker reported them.
	Today      bool    `json:"today"`
	TodayCalls int     `json:"today_calls"`
	TodayUSD   float64 `json:"today_est_usd"`
}

// AIAnswer is what the AI reviewer said when a person asked for it from a
// review screen: a verdict on an agent_review change (Kind "review"), or
// advice on a change the person decides (Kind "advice").
type AIAnswer struct {
	Kind     string    `json:"kind"`
	Model    string    `json:"model"`
	Verdict  string    `json:"verdict,omitempty"` // review: approve or escalate
	Reason   string    `json:"reason,omitempty"`
	Concerns []string  `json:"concerns,omitempty"`
	Advice   *AIAdvice `json:"advice,omitempty"`
	Decision Decision  `json:"decision"`
}

// aiAsker is implemented by sources that can ask the Worker's AI reviewer.
// For a decision waiting for a reviewer agent the AI decides (approve or
// escalate); for one routed to a person it only advises.
type aiAsker interface {
	AskAI(ctx context.Context, d Decision) (AIAnswer, error)
}

// ErrAIUnavailable: the Worker's AI reviewer is off, or this Worker has no
// AI advice route.
var ErrAIUnavailable = errors.New("the AI reviewer is not available on this Worker")

// coster is implemented by sources that can measure cost.
type coster interface {
	Cost(ctx context.Context) (*Cost, error)
}

// Snapshot is one poll's worth of state.
type Snapshot struct {
	Project   string     `json:"project"`
	Sample    bool       `json:"sample"` // true when the data is --demo-data, never real
	FetchedAt time.Time  `json:"fetched_at"`
	Counters  Counters   `json:"counters"`
	Intents   []Intent   `json:"intents"`
	Overlaps  []Overlap  `json:"overlaps"`
	Decisions []Decision `json:"decisions"`
	Events    []Event    `json:"events"`
	Cursor    int64      `json:"cursor"` // highest audit seq seen
	// Warnings say what the source could not show (for example, an older
	// Worker without a route). Absent when everything loaded.
	Warnings []string `json:"warnings,omitempty"`
}

// warner is implemented by sources that can report partial data.
type warner interface{ Warnings() []string }

// sampler is implemented by data sources that serve sample data.
type sampler interface{ Sample() bool }

// IsSample reports whether ds serves sample data that must be labelled.
func IsSample(ds DataSource) bool {
	s, ok := ds.(sampler)
	return ok && s.Sample()
}

// EventLimit caps how many audit events a snapshot carries.
const EventLimit = 200

// Load reads a full snapshot. Events are those after since, appended to
// prev (which may be nil), and trimmed to EventLimit.
func Load(ctx context.Context, ds DataSource, since int64, prev []Event, now time.Time) (Snapshot, error) {
	intents, err := ds.Intents(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	overlaps, err := ds.Overlaps(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	decisions, err := ds.Decisions(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	events, err := ds.Audit(ctx, since, EventLimit)
	if err != nil {
		return Snapshot{}, err
	}
	all := append(slices.Clone(prev), events...)
	if len(all) > EventLimit {
		all = all[len(all)-EventLimit:]
	}
	cursor := since
	for _, e := range all {
		cursor = max(cursor, e.Seq)
	}
	s := Snapshot{
		Project:   ds.Project(),
		Sample:    IsSample(ds),
		FetchedAt: now,
		Intents:   nonNil(intents),
		Overlaps:  nonNil(overlaps),
		Decisions: nonNil(decisions),
		Events:    nonNil(all),
		Cursor:    cursor,
	}
	s.Counters = Count(s.Intents, s.Overlaps, s.Decisions)
	if cs, ok := ds.(coster); ok {
		c, err := cs.Cost(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		s.Counters.Cost = c
	}
	if w, ok := ds.(warner); ok {
		s.Warnings = w.Warnings()
	}
	if n := len(s.Events); n > 0 {
		at := s.Events[n-1].At
		s.Counters.LastEventAt = &at
	}
	return s, nil
}

// Count derives the on-screen counters.
func Count(intents []Intent, overlaps []Overlap, decisions []Decision) Counters {
	agents := map[string]bool{}
	for _, in := range intents {
		agents[in.Agent] = true
	}
	c := Counters{
		Agents:         len(agents),
		Intents:        len(intents),
		ByAction:       map[policy.Action]int{},
		Decisions:      len(decisions),
		Overlaps:       len(overlaps),
		TimeSavedBasis: "estimate: 15 min of review per change that landed without a person",
	}
	for _, a := range policy.Actions() {
		c.ByAction[a] = 0
	}
	for _, in := range intents {
		if in.Active() {
			c.OpenIntents++
		}
	}
	latest := latestPerIntent(decisions)
	for _, d := range decisions {
		c.ByAction[d.Action]++
		switch {
		case d.Action.Lands():
			c.AutoLanded++
		case d.Action.NeedsHuman():
			c.HumanRouted++
		case d.Action == policy.Block:
			c.Blocked++
		}
		if d.AwaitsHuman() && isLatest(latest, d) {
			c.AwaitingHuman++
		}
		switch _, ai := d.AIDecided(); {
		case d.Escalation != nil:
			c.AIEscalations++
		case ai:
			c.AIApprovals++
		}
		if d.AIAdvice != nil {
			c.AIAdvised++
		}
	}
	c.TimeSavedMinutes = c.AutoLanded * MinutesSavedPerChange
	return c
}

// latestPerIntent maps each intent to the id of its newest decision. Only
// that decision can be decided (the Worker refuses the rest with
// decision_replaced), so queues show one entry per intent: the one a
// person can act on.
func latestPerIntent(decisions []Decision) map[string]string {
	at := map[string]time.Time{}
	latest := map[string]string{}
	for _, d := range decisions {
		if d.IntentID == "" {
			continue // nothing to group by; such a decision is its own latest
		}
		if t, ok := at[d.IntentID]; !ok || !d.CreatedAt.Before(t) {
			at[d.IntentID] = d.CreatedAt
			latest[d.IntentID] = d.ID
		}
	}
	return latest
}

// isLatest reports whether d is its intent's newest decision (or has no intent to compare with).
func isLatest(latest map[string]string, d Decision) bool {
	return d.IntentID == "" || latest[d.IntentID] == d.ID
}

// Queue returns the decisions waiting for a person, oldest first: each
// intent's latest decision only.
func Queue(decisions []Decision) []Decision {
	latest := latestPerIntent(decisions)
	q := []Decision{}
	for _, d := range decisions {
		if d.AwaitsHuman() && isLatest(latest, d) {
			q = append(q, d)
		}
	}
	slices.SortStableFunc(q, func(a, b Decision) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return q
}

// AgentQueue returns the agent_review decisions waiting for a reviewer
// agent, oldest first: each intent's latest decision only.
func AgentQueue(decisions []Decision) []Decision {
	latest := latestPerIntent(decisions)
	q := []Decision{}
	for _, d := range decisions {
		if d.Status == StatusPendingAgent && !d.Dropped() && isLatest(latest, d) {
			q = append(q, d)
		}
	}
	slices.SortStableFunc(q, func(a, b Decision) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return q
}

// ErrNotFound is returned by Decide for an unknown decision.
var ErrNotFound = errors.New("not found")

// ErrNotPending is returned by Decide when the decision is not waiting for a
// person.
var ErrNotPending = errors.New("decision is not waiting for a person")

// nonNil makes JSON print [] instead of null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// flattened returns a deep copy of the snapshot with every string on one
// line (textsafe.LineAll), for the one-shot renderers: the live models
// flatten what they load, but RenderWatch and RenderQueue take a snapshot
// straight from Load, and must not change the caller's copy.
func (s Snapshot) flattened() Snapshot {
	s.Intents = cloneIntents(s.Intents)
	decs := make([]Decision, len(s.Decisions))
	for i, d := range s.Decisions {
		decs[i] = cloneDecision(d)
	}
	s.Decisions = decs
	s.Events = slices.Clone(s.Events)
	s.Overlaps = slices.Clone(s.Overlaps)
	for i := range s.Overlaps {
		s.Overlaps[i].Paths = slices.Clone(s.Overlaps[i].Paths)
	}
	s.Warnings = slices.Clone(s.Warnings)
	if s.Counters.Cost != nil {
		c := *s.Counters.Cost
		if c.AI != nil {
			ai := *c.AI
			c.AI = &ai
		}
		s.Counters.Cost = &c
	}
	textsafe.LineAll(&s)
	return s
}
