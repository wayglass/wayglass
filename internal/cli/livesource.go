package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/policy"
	"github.com/wayglass/wayglass/internal/tui"
)

// liveSource is the TUI's view of a project on the Worker.
type liveSource struct {
	c            *api.Client
	project      string
	jurisdiction string
	warnings     []string
}

// Warnings implements tui's warner: what this Worker cannot show.
func (s *liveSource) Warnings() []string { return s.warnings }

var _ tui.DataSource = (*liveSource)(nil)

func (s *liveSource) Project() string { return s.project }

func (s *liveSource) Intents(ctx context.Context) ([]tui.Intent, error) {
	all, err := s.c.ListIntents(ctx, s.project, "")
	if err != nil {
		return nil, err
	}
	out := make([]tui.Intent, 0, len(all))
	for _, in := range all {
		out = append(out, tui.Intent{
			ID: in.ID, Agent: in.Agent, Goal: in.Goal, Reason: in.Reason,
			DeclaredPaths: nonNilStrings(in.DeclaredPaths), ActualPaths: nonNilStrings(in.ActualPaths),
			Fork: in.ForkRepo, Status: in.Status, Jurisdiction: s.jurisdiction,
			CreatedAt: parseWorkerTime(in.CreatedAt), Parent: deref(in.ParentID), Family: deref(in.AgentFamily),
			Stale: in.Stale, AbandonedReason: deref(in.AbandonedReason),
		})
	}
	return out, nil
}

// Overlaps pairs open intents whose declared or actual paths intersect, the
// same check the Worker runs on declare and push, each with a suggested
// re-plan (intent.SuggestReplan). The Worker does not re-plan by itself;
// the suggestion is labelled as one.
func (s *liveSource) Overlaps(ctx context.Context) ([]tui.Overlap, error) {
	in, err := s.Intents(ctx)
	if err != nil {
		return nil, err
	}
	return tui.FindOverlaps(in), nil
}

func (s *liveSource) Decisions(ctx context.Context) ([]tui.Decision, error) {
	all, err := s.c.ListDecisions(ctx, s.project, "")
	if api.IsCode(err, api.CodeNotFound) && strings.Contains(err.Error(), "no route") {
		// An older Worker: show intents, overlaps and activity, and say
		// plainly that decisions are missing rather than failing.
		s.warnings = []string{"this Worker has no GET /v1/decisions yet; decisions are not shown until worker/ is deployed"}
		return []tui.Decision{}, nil
	}
	s.warnings = nil
	if err != nil {
		return nil, err
	}
	out := make([]tui.Decision, 0, len(all))
	for _, d := range all {
		td, err := toTUIDecision(d)
		if err != nil {
			return nil, err
		}
		out = append(out, td)
	}
	return out, nil
}

// Cost reads the Worker's Artifacts operation counts. An older Worker
// without GET /v1/cost shows no cost at all, never a made-up one.
func (s *liveSource) Cost(ctx context.Context) (*tui.Cost, error) {
	c, err := s.c.Cost(ctx, s.project)
	switch {
	case isMissingRoute(err):
		return nil, nil
	case err != nil:
		// Cost is a side figure; losing it must not take the swarm view down.
		s.warnings = append(s.warnings, "cost unavailable: "+err.Error())
		return nil, nil
	}
	return toTUICost(c), nil
}

func toTUICost(c *api.CostSummary) *tui.Cost {
	out := &tui.Cost{Ops: c.Total.Ops, TokensMinted: c.Total.TokensMinted, EstUSD: c.Total.EstUSD, Basis: costBasis(c)}
	if p := c.PerLandedChange; p != nil {
		out.Landed, out.PerLandedOps, out.PerLandedUSD = p.Landed, p.Ops, p.EstUSD
	}
	if ai := c.AI; ai != nil {
		// Only what the Worker counted; a Worker without AI stats shows none.
		out.AI = &tui.AICost{Calls: ai.Calls, EstUSD: ai.EstUSD}
		out.AI.Tokens = ai.Tokens
		if t := ai.Today; t != nil {
			out.AI.Today, out.AI.TodayCalls, out.AI.TodayUSD = true, t.Calls, t.EstUSD
		}
	}
	return out
}

// costBasis is the label every cost figure carries.
func costBasis(c *api.CostSummary) string {
	return fmt.Sprintf("estimate at $%.2f per 1,000 Artifacts operations (%s, as of %s), before the %d included per month. %s",
		c.Pricing.USDPer1000Ops, c.Pricing.Source, c.Pricing.AsOf, c.Pricing.IncludedOpsPerMonth, c.Scope)
}

// isMissingRoute reports a Worker that predates a route.
func isMissingRoute(err error) bool {
	return api.IsCode(err, api.CodeNotFound) && strings.Contains(err.Error(), "no route")
}

func toTUIDecision(d api.Decision) (tui.Decision, error) {
	var tags classify.Tags
	if len(d.Tags) > 0 {
		if err := json.Unmarshal(d.Tags, &tags); err != nil {
			return tui.Decision{}, fmt.Errorf("decision %s: reading tags: %w", d.ID, err)
		}
	}
	files := make([]tui.FileStat, 0, len(d.Detail.Files))
	for _, f := range d.Detail.Files {
		files = append(files, tui.FileStat{Path: f.Path, Added: f.Added, Deleted: f.Deleted})
	}
	linesUnknown := len(files) == 0 && len(d.Paths) > 0 // decided before the Worker recorded line counts
	if linesUnknown {
		for _, p := range d.Paths {
			files = append(files, tui.FileStat{Path: p})
		}
	}
	approvals := make([]tui.Approval, 0, len(d.Approvals))
	for _, a := range d.Approvals {
		// The AI reviewer keeps its record name ("workers-ai:<model>"): the
		// screens recognise and name it themselves.
		by := a.By
		if !a.IsAI() {
			by = approverName(a.By, a.ClaimedBy, "")
		}
		approvals = append(approvals, tui.Approval{By: by, Verdict: tui.Verdict(a.Verdict), Reason: a.Reason, At: parseWorkerTime(a.At), Source: a.IdentitySource})
	}
	out := tui.Decision{
		ID: d.ID, IntentID: d.IntentID, Commit: deref(d.CommitSHA), Tags: tags,
		MatchedRule: cmp.Or(deref(d.MatchedRule), "default"), Action: policy.Action(d.Action), Reason: d.Reason,
		Status: d.Status, ApprovalsNeeded: d.ApprovalsNeeded, Approvals: approvals,
		DecidedBy: deref(d.DecidedBy), Files: files, LinesUnknown: linesUnknown, CreatedAt: parseWorkerTime(d.CreatedAt),
		ReviewerDifferentFamily: d.Detail.Reviewer != nil && d.Detail.Reviewer.DifferentFamily,
		IntentStatus:            d.IntentStatus,
	}
	if e := d.Detail.Escalation; e != nil {
		out.Escalation = &tui.Escalation{By: e.By, Model: e.ModelOf(), Reason: e.Reason, Concerns: nonNilStrings(e.Concerns),
			FromAction: e.FromAction, RuleReason: e.RuleReason, At: parseWorkerTime(e.At)}
	}
	if a := d.Detail.AIAdvice; a != nil {
		out.AIAdvice = toTUIAdvice(*a)
	}
	return out, nil
}

func toTUIAdvice(a api.AIAdvice) *tui.AIAdvice {
	return &tui.AIAdvice{Model: a.ModelOf(), Summary: a.Summary, Risks: nonNilStrings(a.Risks), Concerns: nonNilStrings(a.Concerns),
		Questions: nonNilStrings(a.Questions), At: parseWorkerTime(a.At)}
}

// AskAI asks the Worker's AI reviewer about a decision: a verdict when the
// change waits for a reviewer agent (POST /v1/intents/:id/ai-review), advice
// when policy routed it to a person (POST /v1/intents/:id/ai-advice). The
// Worker's refusals (the reviewer off, a change only a person may decide, a
// Worker without the advice route) come back as plain messages.
func (s *liveSource) AskAI(ctx context.Context, d tui.Decision) (tui.AIAnswer, error) {
	if d.Status == tui.StatusPendingAgent {
		out, err := s.c.AIReviewIntent(ctx, s.project, d.IntentID)
		if err != nil {
			return tui.AIAnswer{}, aiAskError(err)
		}
		dd := out.Decision
		dd.Approvals = out.Approvals
		td, err := toTUIDecision(dd)
		if err != nil {
			return tui.AIAnswer{}, err
		}
		return tui.AIAnswer{Kind: "review", Model: out.Review.Model, Verdict: out.Review.Verdict, Reason: out.Review.Reason,
			Concerns: nonNilStrings(out.Review.Concerns), Decision: td}, nil
	}
	out, err := s.c.AIAdviseIntent(ctx, s.project, d.IntentID)
	if err != nil {
		if isMissingRoute(err) {
			return tui.AIAnswer{}, fmt.Errorf("%w: this Worker has no AI advice route yet (POST /v1/intents/:id/ai-advice); deploy the current worker/", tui.ErrAIUnavailable)
		}
		return tui.AIAnswer{}, aiAskError(err)
	}
	td, err := toTUIDecision(out.Decision)
	if err != nil {
		return tui.AIAnswer{}, err
	}
	adv := toTUIAdvice(out.Advice)
	if td.AIAdvice == nil {
		td.AIAdvice = adv
	}
	return tui.AIAnswer{Kind: "advice", Model: adv.Model, Advice: adv, Decision: td}, nil
}

// aiAskError keeps the Worker's own words and marks the two refusals a
// review screen explains: the reviewer being off, and a change only a
// person may decide.
func aiAskError(err error) error {
	switch {
	case api.IsCode(err, api.CodeAIUnavailable):
		return fmt.Errorf("%w: %s", tui.ErrAIUnavailable, apiMessage(err))
	case api.IsCode(err, api.CodeNeedsHuman):
		return fmt.Errorf("needs a person: %s", apiMessage(err))
	}
	return err
}

// apiMessage is the Worker's message without the code prefix.
func apiMessage(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func (s *liveSource) Audit(ctx context.Context, since int64, limit int) ([]tui.Event, error) {
	page, err := s.c.Audit(ctx, s.project, int(since), min(limit, 500))
	if err != nil {
		return nil, err
	}
	out := make([]tui.Event, 0, len(page.Events))
	for _, e := range page.Events {
		out = append(out, tui.Event{
			Seq: int64(e.Seq), At: parseWorkerTime(e.At), Kind: e.Kind, Actor: e.Actor,
			IntentID: deref(e.IntentID), DecisionID: deref(e.DecisionID), Summary: summarize(e),
		})
	}
	return out, nil
}

// Decide records the verdict as the caller's agent identity (the
// X-Wayglass-Agent header), never a name the TUI passes in, so the Worker's
// self-approval check sees who is really deciding.
func (s *liveSource) Decide(ctx context.Context, decisionID string, v tui.Verdict, _, reason string) (tui.Decision, error) {
	res, err := s.c.Decide(ctx, decisionID, api.DecideInput{Project: s.project, Verdict: string(v), Reason: reason})
	switch {
	case api.IsCode(err, api.CodeNotFound):
		return tui.Decision{}, fmt.Errorf("decision %s: %w", decisionID, tui.ErrNotFound)
	case api.IsCode(err, api.CodeConflict):
		return tui.Decision{}, fmt.Errorf("decision %s: %w: %w", decisionID, tui.ErrNotPending, err)
	case err != nil:
		return tui.Decision{}, err
	}
	d := res.Decision
	d.Approvals = res.Approvals
	return toTUIDecision(d)
}

// summarize turns an audit record into one line for the event feed.
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
		// The AI reviewer's entries: decision.approve carries its identity
		// source; decision.escalated its concerns; decision.ai_advice a summary.
		IdentitySource string   `json:"identity_source"`
		Concerns       []string `json:"concerns"`
		Summary        string   `json:"summary"`
	}
	_ = json.Unmarshal(e.Data, &d) // a summary is best effort; the kind still shows
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
		return "landed " + short(d.Commit) + " on trunk"
	case "land.mismatch":
		return "trunk push did not match the approved change"
	case "land.cancelled":
		return "land cancelled: " + d.Reason
	}
	return strings.ReplaceAll(e.Kind, ".", " ")
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// parseWorkerTime reads the Worker's RFC 3339 timestamps; the zero time when absent.
func parseWorkerTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
