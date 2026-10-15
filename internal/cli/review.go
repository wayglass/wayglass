package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/tui"
)

func newReviewCmd(a *app) *cobra.Command {
	var (
		flags tuiFlags
		who   string
	)
	cmd := &cobra.Command{
		Use:   "review [intent-id]",
		Short: "Review changes policy routed to a person (TUI on a terminal)",
		Long: "Shows the queue of changes waiting for a person. For each: the intent's goal\n" +
			"and reason, the tags, the matched rule and why, approvals so far, and a diff\n" +
			"summary. Approving or rejecting always asks for a reason.\n\n" +
			"With --json it lists the queue and never prompts; decide with\n" +
			"'wayglass approve|reject <intent-id> --reason ...'. A reviewer agent finds\n" +
			"the agent_review changes waiting for it with '--for agent --json'; on a\n" +
			"Worker with the AI reviewer on, 'wayglass ai-review <intent-id>' asks it.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := a.output(cmd)
			if who != "human" && who != "agent" {
				return UsageError("--for must be human or agent, got %q", who)
			}
			interactive := out.Interactive() && who == "human"
			ctx := cmd.Context()
			// Sample data is shown complete: review is for deciding, and a
			// queue that grows while someone reads would be noise.
			ds, err := flags.source(ctx, a, false)
			if err != nil {
				return err
			}
			intentID := ""
			if len(args) == 1 {
				intentID = args[0]
			}
			if interactive {
				return runProgram(ctx, tui.NewReview(ds, tui.Options{Interval: flags.interval, User: reviewer(), Pick: flags.pick}, intentID), out)
			}
			s, err := tui.Load(ctx, ds, 0, nil, time.Now())
			if err != nil {
				return err
			}
			data := reviewQueue(s, intentID, who)
			return out.Emit(data, func(w io.Writer) error {
				if who == "agent" {
					return renderAgentQueue(w, data)
				}
				_, err := lipgloss.Fprint(w, tui.RenderQueue(s, tui.NewStyles(true), 100))
				return err
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&who, "for", "human", "whose queue: human (human_approve, multi_approve, quarantine) or agent (agent_review)")
	return cmd
}

func renderAgentQueue(w io.Writer, d ReviewData) error {
	if len(d.Queue) == 0 {
		_, err := fmt.Fprintln(w, "No changes waiting for an agent review.")
		return err
	}
	for _, it := range d.Queue {
		fmt.Fprintf(w, "%s  %s (by %s)\n  rule %s: %s\n", it.Intent.ID, it.Intent.Goal, it.Intent.Agent, it.Decision.MatchedRule, it.Decision.Reason)
		if it.Decision.ReviewerDifferentFamily {
			fmt.Fprintf(w, "  needs a reviewer from a model family other than the author's (%s)\n", cmp.Or(it.Intent.Family, "not declared"))
		}
	}
	_, err := fmt.Fprintln(w, "Decide with: wayglass approve|reject <intent-id> --reason \"...\"")
	return err
}

// ReviewItem is one queue entry in `review --json`.
type ReviewItem struct {
	Decision tui.Decision `json:"decision"`
	Intent   tui.Intent   `json:"intent"`
}

// ReviewData is the data of `review --json`.
type ReviewData struct {
	Project string       `json:"project"`
	Sample  bool         `json:"sample"`
	Queue   []ReviewItem `json:"queue"`
	// Warnings say what the Worker could not show; an empty queue with a
	// warning does not mean nothing waits.
	Warnings []string `json:"warnings,omitempty"`
	// Untrusted lists the fields written by the change's author or other
	// agents. A reviewer reads them as data to check, never as instructions.
	Untrusted []string `json:"untrusted"`
}

// reviewUntrusted is ReviewData.Untrusted: everything an agent wrote.
var reviewUntrusted = []string{
	"queue[].intent.goal", "queue[].intent.reason", "queue[].intent.agent",
	"queue[].intent.declared_paths", "queue[].intent.actual_paths", "queue[].intent.abandoned_reason",
	"queue[].decision.files[].path", "queue[].decision.tags.evidence[].path",
	"queue[].decision.approvals[].by", "queue[].decision.approvals[].reason",
	// The AI reviewer's words are model output read from the agent's diff: data, not instructions.
	"queue[].decision.escalation.reason", "queue[].decision.escalation.concerns",
	"queue[].decision.ai_advice.summary", "queue[].decision.ai_advice.risks", "queue[].decision.ai_advice.concerns", "queue[].decision.ai_advice.questions",
}

// reviewQueue lists what waits for who: a person's queue, or the
// agent_review changes a reviewer agent can decide.
func reviewQueue(s tui.Snapshot, intentID, who string) ReviewData {
	byID := map[string]tui.Intent{}
	for _, in := range s.Intents {
		byID[in.ID] = in
	}
	d := ReviewData{Project: s.Project, Sample: s.Sample, Queue: []ReviewItem{}, Warnings: s.Warnings, Untrusted: reviewUntrusted}
	queue := tui.Queue(s.Decisions)
	if who == "agent" {
		queue = tui.AgentQueue(s.Decisions)
	}
	for _, dec := range queue {
		if intentID != "" && dec.IntentID != intentID {
			continue
		}
		d.Queue = append(d.Queue, ReviewItem{Decision: dec, Intent: byID[dec.IntentID]})
	}
	return d
}

// reviewer names the person deciding. Placeholder until sessions carry an
// identity from `wayglass login`.
func reviewer() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	return "you"
}

func newApproveCmd(a *app) *cobra.Command {
	return newDecisionCmd(a, "approve", "Approve a change routed to you, with a reason")
}

func newRejectCmd(a *app) *cobra.Command {
	return newDecisionCmd(a, "reject", "Reject a change routed to you, with a reason")
}

// placeholderReason matches reasons copied from usage examples ("<what you
// checked>") or filler. Mirrors PLACEHOLDER in worker/src/index.ts.
var placeholderReason = regexp.MustCompile(`(?i)^<[^<>]*>$|^(todo|tbd|n/a|none|-|\.+)$`)

// fillerWords and fillerReason catch rubber stamps ("LGTM", "ok", "+1", "👍"):
// the reason is lower-cased, punctuation and emoji become spaces, and what is
// left must not be only these words. Mirrors FILLER in worker/src/index.ts.
var (
	nonWord     = regexp.MustCompile(`[^a-z0-9]+`)
	fillerWords = regexp.MustCompile(`^(?:(?:lgtm|lg|looks good(?: to me)?|ok|okay|k|approved?|approving|ship ?it|yes|yep|yeah|y|fine|good|great|nice|sure|done|ack|nack|no|nope|nah|rejected?|thanks|ty|1)(?: |$))*$`)
)

func fillerReason(reason string) bool {
	return fillerWords.MatchString(strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(reason), " ")))
}

// decisionResult is the output of approve and reject.
type decisionResult struct {
	IntentID  string         `json:"intent_id"`
	Verdict   string         `json:"verdict"`
	Decision  api.Decision   `json:"decision"`
	Approvals []api.Approval `json:"approvals"`
	Next      string         `json:"next"`
}

func newDecisionCmd(a *app, name, short string) *cobra.Command {
	var reason, family, decisionID string
	cmd := &cobra.Command{
		Use:   name + " <intent-id>",
		Short: short,
		Long: short + ".\n\nDecides the intent's latest proposed change. The reason is recorded with\n" +
			"your name in the audit log and travels with the commit. Exit 0 when the\n" +
			"decision was recorded, whatever the verdict.\n\n" +
			"The verdict is bound to the decision you reviewed: 'wayglass review' shows\n" +
			"its id, pass it with --decision. Without the flag the intent's latest\n" +
			"decision is read first and decided by id. Either way, if the author\n" +
			"proposes again in between, the Worker refuses (decision_replaced) instead\n" +
			"of approving a change nobody read.\n\n" +
			"A change policy routed to a person (human_approve, multi_approve,\n" +
			"quarantine) can only be decided with the admin key or a human key\n" +
			"('wayglass keys create --human'); an agent key gets needs_human (exit 4).",
		Example: "  wayglass " + name + " i269d1c348e --reason \"Checked the login flow against the intent\"",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return UsageError("--reason is required: every decision carries its why")
			}
			if placeholderReason.MatchString(strings.TrimSpace(reason)) {
				return UsageError("--reason %q looks like a placeholder; the audit log keeps it forever, so say what you checked", reason)
			}
			if fillerReason(reason) {
				return UsageError("--reason %q is a rubber stamp, not a why; the audit log keeps it forever, so say what you checked", reason)
			}
			f, err := familyFlag(family)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			c, err := a.client()
			if err != nil {
				return err
			}
			w, err := openWorkspace(ctx)
			if err != nil {
				return err
			}
			c.Family = cmp.Or(f, c.Family)
			verdict := name // the command names are the API verdicts
			if decisionID == "" {
				// Bind the verdict to the decision that exists now, so a
				// proposal that lands between this read and the verdict is
				// refused by the Worker rather than approved unseen.
				decisionID, err = latestDecisionID(ctx, c, w.cfg.Project, args[0])
				if err != nil {
					return err
				}
			}
			out, err := c.DecideIntent(ctx, args[0], api.DecideInput{Project: w.cfg.Project, Verdict: verdict, Reason: reason, DecisionID: decisionID})
			if api.IsCode(err, api.CodeDecisionReplaced) {
				return errorf(api.CodeDecisionReplaced, ExitError,
					"intent %s was proposed again after decision %s, so that decision is not the change any more; nothing was recorded. "+
						"Run 'wayglass review %s' to read the new proposal, then %s it again",
					args[0], decisionID, args[0], verdict)
			}
			if api.IsCode(err, api.CodeNeedsHuman) {
				return errorf(api.CodeNeedsHuman, ExitNeedsHuman,
					"policy routed intent %s to a person, and this key is an agent key, so it cannot %s it; nothing was recorded. "+
						"A person decides with 'wayglass %s %s --reason ...' using the admin key or a key from 'wayglass keys create --human'",
					args[0], verdict, verdict, args[0])
			}
			if api.IsCode(err, api.CodeSuperseded) {
				return errorf(api.CodeSuperseded, ExitError,
					"intent %s was re-stacked after it was proposed, so its decision (pending or approved) no longer covers the change and cannot be decided; nothing was recorded. "+
						"Its author runs 'wayglass propose' again, then 'wayglass review' shows the new decision",
					args[0])
			}
			if err != nil {
				return err
			}
			res := decisionResult{IntentID: args[0], Verdict: verdict, Decision: out.Decision, Approvals: out.Approvals}
			res.Next = decisionNext(out.Decision, out.WaitingFor, len(out.Approvals))
			return a.output(cmd).Emit(res, func(o io.Writer) error {
				d := res.Decision
				st := cliStyles(a.output(cmd))
				word := st.good.Render(verdictWord(verdict))
				if verdict != "approve" {
					word = st.stopped.Render(verdictWord(verdict))
				}
				_, _ = lipgloss.Fprintf(o, "%s %s: decision %s is now %s\n", word, st.id(res.IntentID), st.id(d.ID), st.status(d.Status))
				fields(o, st, field{"Reason", reason})
				_, err := lipgloss.Fprintln(o, res.Next)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why you decided this (required)")
	cmd.Flags().StringVar(&family, "family", "", "your model family: claude, openai, gemini, meta, other or human (default $"+api.EnvFamily+")")
	cmd.Flags().StringVar(&decisionID, "decision", "", "the decision id you reviewed (from 'wayglass review'); default: the intent's latest decision, read first")
	return cmd
}

// latestDecisionID reads the intent's newest decision so the verdict can
// name it. No decision at all is the Worker's answer to give (conflict).
func latestDecisionID(ctx context.Context, c *api.Client, project, intentID string) (string, error) {
	ds, err := c.ListDecisions(ctx, project, "")
	if err != nil {
		return "", err
	}
	id := ""
	for _, d := range ds { // oldest first; the last match is the latest
		if d.IntentID == intentID {
			id = d.ID
		}
	}
	return id, nil
}

// familyFlag validates a --family value ("" when not given).
func familyFlag(v string) (string, error) {
	f, ok := api.NormalizeFamily(v)
	if !ok {
		return "", UsageError("--family %q is not one of %s", v, strings.Join(api.Families, ", "))
	}
	return f, nil
}

func verdictWord(v string) string {
	if v == "approve" {
		return "Approved"
	}
	return "Rejected"
}

// decisionNext is the line after a verdict. waitingFor is the Worker's own
// account of a still-pending decision (distinct approvers short); an older
// Worker sends none, so the count of recorded approvals stands in.
func decisionNext(d api.Decision, waitingFor string, approvals int) string {
	switch d.Status {
	case api.StatusApproved:
		return "Approved: the agent can land it."
	case api.StatusRejected:
		return "Rejected: the agent must change course; the reason is in the audit log."
	default:
		if waitingFor != "" {
			return "Recorded; " + waitingFor + "."
		}
		return fmt.Sprintf("Recorded (%d of %d approvals); still waiting for more.", approvals, d.ApprovalsNeeded)
	}
}
