package api

import (
	"encoding/json"

	"github.com/wayglass/wayglass/internal/policy"
)

// Wire types mirror worker/src/project.ts and worker/src/index.ts.

// Project is a Wayglass project: one trunk repo plus its intents.
type Project struct {
	Name        string `json:"name"`
	TrunkRepo   string `json:"trunk_repo"`
	TrunkRemote string `json:"trunk_remote"`
	// Jurisdiction of the Artifacts namespace holding the project's repos;
	// "" from projects made before per-project jurisdiction (the Worker's main one).
	Jurisdiction string `json:"jurisdiction,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// Repo is an Artifacts repo as the Worker reports it.
type Repo struct {
	Name          string `json:"name"`
	Remote        string `json:"remote"`
	DefaultBranch string `json:"defaultBranch"`
}

// Token is a short-lived Artifacts repo token. Value is a secret: hold it in
// memory, hand it to git through gitx, never write it to disk or logs.
type Token struct {
	Value     string `json:"value"`
	Scope     string `json:"scope"`
	ExpiresAt string `json:"expires_at"`
}

// CreateProjectResult is the reply to POST /v1/projects.
type CreateProjectResult struct {
	Project Project `json:"project"`
	Trunk   Repo    `json:"trunk"`
	Token   Token   `json:"token"`
	// Public is true when the project was opted in to the public read view
	// at creation; false from Workers that predate it.
	Public bool `json:"public"`
	// Events is nil from Workers that predate per-trunk subscriptions.
	Events *TrunkEvents `json:"events,omitempty"`
}

// TrunkEvents says whether the trunk's pushes reach the Worker's queue
// consumer, which is how pushes that skip `wayglass land` get flagged.
type TrunkEvents struct {
	Subscribed     bool   `json:"subscribed"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// Intent is a declared unit of agent work.
type Intent struct {
	ID            string   `json:"id"`
	Goal          string   `json:"goal"`
	Reason        string   `json:"reason"`
	Agent         string   `json:"agent"`
	DeclaredPaths []string `json:"declared_paths"`
	ActualPaths   []string `json:"actual_paths"`
	ForkRepo      string   `json:"fork_repo"`
	ForkRemote    string   `json:"fork_remote"`
	HeadCommit    *string  `json:"head_commit"`
	Status        string   `json:"status"`
	// BaseCommit is where the fork started: a trunk commit, or the parent's
	// fork head for a stacked intent.
	BaseCommit *string `json:"base_commit,omitempty"`
	// ParentID is the intent this one is stacked on; nil when forked from trunk
	// (and from Workers that predate stacking).
	ParentID *string `json:"parent_id,omitempty"`
	// ParentIntentID and SessionID are set on intents made by
	// `wayglass session fork`.
	ParentIntentID *string `json:"parent_intent_id,omitempty"`
	SessionID      *string `json:"session_id,omitempty"`
	// AgentFamily is the author's declared model family, if any.
	AgentFamily *string `json:"agent_family,omitempty"`
	// LastPushAt is when the agent last reported a push; nil if never.
	LastPushAt *string `json:"last_push_at,omitempty"`
	// Stale is set by the Worker on open or proposed intents with no push
	// for its STALE_INTENT_HOURS. A report only; nothing acts on it.
	Stale bool `json:"stale,omitempty"`
	// AbandonedBy and AbandonedReason are set once the intent is abandoned.
	AbandonedBy     *string `json:"abandoned_by,omitempty"`
	AbandonedReason *string `json:"abandoned_reason,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// Head returns the last reported commit, or "".
func (i Intent) Head() string {
	if i.HeadCommit == nil {
		return ""
	}
	return *i.HeadCommit
}

// PathOverlap is one intersecting pair of patterns.
type PathOverlap struct {
	Mine   string `json:"mine"`
	Theirs string `json:"theirs"`
}

// Overlap is another active intent whose paths intersect this one.
type Overlap struct {
	IntentID string        `json:"intent_id"`
	Goal     string        `json:"goal"`
	Agent    string        `json:"agent"`
	Status   string        `json:"status"`
	Paths    []PathOverlap `json:"paths"`
	// Relation is "parent" or "child" when the other intent is this one's
	// session source or a session fork of it: expected, but both can land.
	Relation string `json:"relation,omitempty"`
	// Stale says the other intent has not pushed for the Worker's
	// STALE_INTENT_HOURS: possibly dead weight, not live competition.
	Stale bool `json:"stale,omitempty"`
}

// DeclareResult is the reply to POST /v1/intents.
type DeclareResult struct {
	Intent   Intent    `json:"intent"`
	Fork     Repo      `json:"fork"`
	Token    Token     `json:"token"`
	Overlaps []Overlap `json:"overlaps"`
}

// PushResult is the reply to POST /v1/intents/:id/push.
type PushResult struct {
	Intent   Intent    `json:"intent"`
	Overlaps []Overlap `json:"overlaps"`
}

// Decision statuses (worker/src/project.ts).
const (
	StatusApproved     = "approved"
	StatusRejected     = "rejected"
	StatusPendingAgent = "pending_agent"
	StatusPendingHuman = "pending_human"
	StatusQuarantined  = "quarantined"
	StatusBlocked      = "blocked"
	// StatusSuperseded: the intent was re-stacked while this decision was
	// pending or approved (not yet landed), so it no longer covers the change.
	// Nobody can decide or land it; the author proposes again. A superseded
	// approval keeps decided_by and its approvals: who approved stays on record.
	StatusSuperseded = "superseded"
)

// Decision is the policy outcome recorded for a proposal.
type Decision struct {
	ID              string          `json:"id"`
	IntentID        string          `json:"intent_id"`
	CommitSHA       *string         `json:"commit_sha"`
	Tags            json.RawMessage `json:"tags"`
	MatchedRule     *string         `json:"matched_rule"`
	Action          string          `json:"action"`
	Reason          string          `json:"reason"`
	Status          string          `json:"status"`
	ApprovalsNeeded int             `json:"approvals_needed"`
	DecidedBy       *string         `json:"decided_by"`
	DecidedAt       *string         `json:"decided_at"`
	LandedCommit    *string         `json:"landed_commit,omitempty"`
	// PolicyCommit and PolicySHA256 name the trunk policy the decision was
	// made under ("sha256:<hex>" or "absent"); empty before Oct 14.
	PolicyCommit *string `json:"policy_commit,omitempty"`
	PolicySHA256 *string `json:"policy_sha256,omitempty"`
	// Paths the Worker saw change between the base and CommitSHA: exactly
	// what the decision covers. Empty from Workers that predate it.
	Paths     []string       `json:"paths,omitempty"`
	Detail    DecisionDetail `json:"detail,omitzero"`
	CreatedAt string         `json:"created_at"`
	// Approvals is filled by ListDecisions only.
	Approvals []Approval `json:"approvals,omitempty"`
	// IntentStatus is the owning intent's status, from ListDecisions; a
	// decision of an abandoned intent is waiting for no one.
	IntentStatus string `json:"intent_status,omitempty"`
}

// DecisionDetail is the part of a decision's detail the CLI reads: per-file
// line counts, shadow rules that matched (logged, never enforced) and
// whether the first of them would have changed the action. Older decisions
// have none of them.
type DecisionDetail struct {
	// Files has lines added and removed per file; empty for decisions made
	// before the Worker recorded them.
	Files         []FileStat           `json:"files,omitempty"`
	ShadowMatches []policy.ShadowMatch `json:"shadow_matches,omitempty"`
	WouldDiffer   bool                 `json:"would_differ,omitzero"`
	// Reviewer constrains who may approve (policy rule `reviewer`).
	Reviewer *policy.Reviewer `json:"reviewer,omitempty"`
	// AI is the rule's AI settings (policy rule `ai`), if any.
	AI *policy.AI `json:"ai,omitempty"`
	// Truncated lists why the Worker's diff was incomplete; the decision's
	// paths are then partial.
	Truncated []string `json:"truncated,omitempty"`
	// Escalation is set once the AI reviewer handed an agent_review change
	// to a person (worker/src/aireview.ts); the action is then human_approve.
	Escalation *Escalation `json:"escalation,omitempty"`
	// AIAdvice is the AI reviewer's advisory read of a change routed to a
	// person: it informs the person's verdict and decides nothing. Absent
	// from Workers that predate it.
	AIAdvice *AIAdvice `json:"ai_advice,omitempty"`
}

// Escalation records the AI reviewer sending a change to a person: who,
// why, and what it saw. Every field but By and Reason may be absent.
type Escalation struct {
	By             string   `json:"by"`
	Reason         string   `json:"reason"`
	Concerns       []string `json:"concerns,omitempty"`
	FromAction     string   `json:"from_action,omitempty"`
	RuleReason     string   `json:"rule_reason,omitempty"`
	Family         string   `json:"family,omitempty"`
	IdentitySource string   `json:"identity_source,omitempty"`
	At             string   `json:"at,omitempty"`
	AI             *AIMeta  `json:"ai,omitempty"`
}

// AIMeta is how one model call ran: audited with the verdict or advice.
type AIMeta struct {
	Model      string   `json:"model,omitempty"`
	Gateway    *string  `json:"gateway,omitempty"`
	LatencyMS  int      `json:"latency_ms,omitempty"`
	Tokens     *Tokens  `json:"tokens,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	Source     string   `json:"source,omitempty"`
}

// ModelOf returns the model the escalation names, from its AI record or
// its decider ("workers-ai:<model>").
func (e Escalation) ModelOf() string {
	if e.AI != nil && e.AI.Model != "" {
		return e.AI.Model
	}
	return AIModel(e.By)
}

// ModelOf returns the model that gave the advice.
func (a AIAdvice) ModelOf() string {
	if a.Model != "" {
		return a.Model
	}
	if a.AI != nil && a.AI.Model != "" {
		return a.AI.Model
	}
	return AIModel(a.By)
}

// FileStat is one file in a decision's diff summary.
type FileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// Approval is one person's or agent's verdict on a decision.
type Approval struct {
	DecisionID string `json:"decision_id"`
	// By is the identity the verdict counts as: a key's bound name, or
	// "admin" for the admin key, whatever name it reported.
	By string `json:"by"`
	// ClaimedBy is the name the admin key reported with its verdict. It is
	// kept for the record and never counted as a distinct approver.
	ClaimedBy string `json:"claimed_by,omitempty"`
	// Family is the decider's declared model family, if any.
	Family  string `json:"family,omitempty"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
	// IdentitySource is how the decider signed in: "admin", "key", "access"
	// (Cloudflare Access) or "ai" (the Workers AI reviewer). Empty for
	// approvals before Oct 14.
	IdentitySource string `json:"identity_source,omitempty"`
}

// IsAI reports whether the AI reviewer gave this verdict.
func (a Approval) IsAI() bool { return a.IdentitySource == SourceAI || IsAIReviewer(a.By) }

// ProposeResult is the reply to POST /v1/intents/:id/propose.
type ProposeResult struct {
	Intent   Intent   `json:"intent"`
	Decision Decision `json:"decision"`
}

// Land is one attempt to land an approved change on the trunk.
type Land struct {
	ID           string  `json:"id"`
	IntentID     string  `json:"intent_id"`
	DecisionID   string  `json:"decision_id"`
	Status       string  `json:"status"`
	ExpiresAt    string  `json:"expires_at"`
	LandedCommit *string `json:"landed_commit"`
}

// LandResult is the reply to POST /v1/intents/:id/land: a reservation and a
// short-lived trunk write token. Token.Value is a secret.
type LandResult struct {
	Land     Land     `json:"land"`
	Decision Decision `json:"decision"`
	Trunk    struct {
		Repo   string `json:"repo"`
		Remote string `json:"remote"`
	} `json:"trunk"`
	Fork struct {
		Repo   string `json:"repo"`
		Remote string `json:"remote"`
		Commit string `json:"commit"`
	} `json:"fork"`
	Token Token `json:"token"`
	// TrunkBefore is the trunk head when the token was issued; the Worker
	// accepts only the approved change on top of exactly this. Nil from
	// Workers that predate it.
	TrunkBefore *string `json:"trunk_before,omitempty"`
}

// LandedResult is the reply to POST /v1/intents/:id/landed.
type LandedResult struct {
	Intent       Intent   `json:"intent"`
	Decision     Decision `json:"decision"`
	Land         Land     `json:"land"`
	TokenRevoked bool     `json:"token_revoked"`
}

// DecideResult is the reply to POST /v1/decisions/:id.
type DecideResult struct {
	Decision  Decision   `json:"decision"`
	Approvals []Approval `json:"approvals"`
	// WaitingFor says why an approval left the decision pending (a
	// multi_approve change short of distinct approvers); empty otherwise.
	WaitingFor string `json:"waiting_for,omitempty"`
}

// AuditEvent is one append-only audit record.
type AuditEvent struct {
	Seq        int             `json:"seq"`
	At         string          `json:"at"`
	Kind       string          `json:"kind"`
	Actor      string          `json:"actor"`
	IntentID   *string         `json:"intent_id"`
	DecisionID *string         `json:"decision_id"`
	Data       json.RawMessage `json:"data"`
}

// AuditPage is the reply to GET /v1/audit.
type AuditPage struct {
	Events []AuditEvent `json:"events"`
	Next   int          `json:"next"`
}

// WhyResult is the reply to GET /v1/why.
type WhyResult struct {
	Commit    string     `json:"commit"`
	Intent    Intent     `json:"intent"`
	Decision  Decision   `json:"decision"`
	Approvals []Approval `json:"approvals"`
}

// DecisionWithApprovals is a decision as frozen in a session snapshot.
type DecisionWithApprovals struct {
	Decision
	Approvals []Approval `json:"approvals"`
}

// SessionSnapshot is what an agent handed on: the intent and everything
// decided about it up to the share.
type SessionSnapshot struct {
	Intent    Intent                  `json:"intent"`
	Decisions []DecisionWithApprovals `json:"decisions"`
	Overlaps  []Overlap               `json:"overlaps"`
	History   []AuditEvent            `json:"history"`
}

// Session is a shared, append-only session snapshot.
type Session struct {
	ID       string `json:"id"`
	IntentID string `json:"intent_id"`
	SharedBy string `json:"shared_by"`
	// HeadCommit is the fork head the Worker read from Artifacts at the share.
	HeadCommit *string         `json:"head_commit"`
	Notes      string          `json:"notes"`
	Snapshot   SessionSnapshot `json:"snapshot"`
	CreatedAt  string          `json:"created_at"`
}

// ShareResult is the reply to POST /v1/intents/:id/sessions.
type ShareResult struct {
	Session Session `json:"session"`
	URL     string  `json:"url"`
}

// SessionView is the reply to GET /v1/projects/:project/sessions/:id.
type SessionView struct {
	Project string   `json:"project"`
	Session Session  `json:"session"`
	Forks   []Intent `json:"forks"`
	URL     string   `json:"url"`
}

// SessionParent is the intent a session fork continues.
type SessionParent struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	ForkRepo string `json:"fork_repo"`
}

// ForkSessionResult is the reply to POST /v1/projects/:project/sessions/:id/fork:
// a new intent and a write token for its own new fork. Token.Value is a secret.
type ForkSessionResult struct {
	Session  Session       `json:"session"`
	URL      string        `json:"url"`
	Intent   Intent        `json:"intent"`
	Parent   SessionParent `json:"parent"`
	Fork     Repo          `json:"fork"`
	Token    Token         `json:"token"`
	Overlaps []Overlap     `json:"overlaps"`
}

// CostLine is a count of Artifacts binding calls with an estimate.
type CostLine struct {
	Ops          int            `json:"ops"`
	ByOp         map[string]int `json:"by_op"`
	TokensMinted int            `json:"tokens_minted"`
	EstUSD       float64        `json:"est_usd"`
}

// Pricing is the published Artifacts price the estimate uses.
type Pricing struct {
	USDPer1000Ops       float64 `json:"usd_per_1000_ops"`
	IncludedOpsPerMonth int     `json:"included_ops_per_month"`
	Source              string  `json:"source"`
	AsOf                string  `json:"as_of"`
}

// AICost is what the AI reviewer has cost, as GET /v1/cost reports it when
// the Worker counts model calls. Every field is optional on the wire; a
// Worker without it sends none, and nothing is estimated in its place.
type AICost struct {
	Calls       int     `json:"calls,omitempty"`
	Approvals   int     `json:"approvals,omitempty"`
	Escalations int     `json:"escalations,omitempty"`
	Advice      int     `json:"advice,omitempty"`
	Tokens      int     `json:"tokens,omitempty"`
	EstUSD      float64 `json:"est_usd,omitempty"`
	// Today is the same for the current UTC day, when the Worker keeps it.
	Today *AICost `json:"today,omitempty"`
	Basis string  `json:"basis,omitempty"`
}

// CostSummary is GET /v1/cost.
type CostSummary struct {
	// Scope says what is and is not counted; show it with the numbers.
	Scope           string              `json:"scope"`
	Pricing         Pricing             `json:"pricing"`
	Total           CostLine            `json:"total"`
	Project         CostLine            `json:"project"`
	Intents         map[string]CostLine `json:"intents"`
	Events          map[string]int      `json:"events"`
	PerLandedChange *struct {
		Landed int     `json:"landed"`
		Ops    float64 `json:"ops"`
		EstUSD float64 `json:"est_usd"`
	} `json:"per_landed_change"`
	// AI is the AI reviewer's usage; nil from Workers that do not count it.
	AI *AICost `json:"ai,omitempty"`
}
