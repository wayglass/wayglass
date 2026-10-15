package cli

import (
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

// aiReviewResult is the output of ai-review.
type aiReviewResult struct {
	IntentID  string         `json:"intent_id"`
	Decision  api.Decision   `json:"decision"`
	Approvals []api.Approval `json:"approvals"`
	Review    api.AIVerdict  `json:"review"`
	Next      string         `json:"next"`
}

func newAIReviewCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ai-review <intent-id>",
		Short: "Have the Worker's AI reviewer decide an agent_review change",
		Long: "Asks the Worker to review the intent's latest agent_review decision with\n" +
			"Workers AI. The model reads the intent and the change's diff and either\n" +
			"approves (recorded as workers-ai:<model>, like any reviewer's approval) or\n" +
			"escalates the change to a person. It never rejects, and it never decides a\n" +
			"change policy routed to a person (needs_human, exit 4). Anything it cannot\n" +
			"read or answer with confidence goes to a person.\n\n" +
			"The Worker must have the reviewer on (AI_REVIEW=manual or auto); otherwise\n" +
			"ai_unavailable (exit 1) and the change keeps waiting for a reviewer agent\n" +
			"('wayglass review --for agent').",
		Example: "  wayglass ai-review i269d1c348e --json",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := a.client()
			if err != nil {
				return err
			}
			w, err := openWorkspace(ctx)
			if err != nil {
				return err
			}
			out, err := c.AIReviewIntent(ctx, w.cfg.Project, args[0])
			if api.IsCode(err, api.CodeAIUnavailable) {
				return errorf(api.CodeAIUnavailable, ExitError,
					"the Worker's AI reviewer is not available (%s); nothing was recorded. A reviewer agent decides it: 'wayglass review --for agent'", err.Error())
			}
			if api.IsCode(err, api.CodeNeedsHuman) {
				return errorf(api.CodeNeedsHuman, ExitNeedsHuman,
					"policy routed intent %s to a person, so the AI reviewer cannot decide it; nothing was recorded. "+
						"A person decides with 'wayglass review' or 'wayglass approve|reject %s --reason ...'", args[0], args[0])
			}
			if api.IsCode(err, api.CodeNeedsAgent) {
				return errorf(api.CodeNeedsAgent, ExitError,
					"policy wants a reviewer agent for intent %s (reviewer: agent), so the AI reviewer cannot decide it; nothing was recorded. "+
						"A reviewer agent finds it with 'wayglass review --for agent'", args[0])
			}
			if api.IsCode(err, api.CodeSuperseded) {
				return errorf(api.CodeSuperseded, ExitError,
					"intent %s was re-stacked after it was proposed, so its decision no longer covers the change; nothing was recorded. "+
						"Its author runs 'wayglass propose' again first", args[0])
			}
			if err != nil {
				return err
			}
			res := aiReviewResult{IntentID: args[0], Decision: out.Decision, Approvals: out.Approvals, Review: out.Review}
			res.Next = aiReviewNext(out)
			return a.output(cmd).Emit(res, func(o io.Writer) error {
				st := cliStyles(a.output(cmd))
				r := res.Review
				word := st.good.Render("Approved")
				if r.Verdict != "approve" {
					word = st.stopped.Render("Escalated to a person")
				}
				_, _ = lipgloss.Fprintf(o, "%s %s: decision %s is now %s\n", word, st.id(res.IntentID), st.id(res.Decision.ID), st.status(res.Decision.Status))
				rows := []field{{"Reviewer", r.ReviewerName() + aiVia(r)}, {"Reason", r.Reason}}
				if len(r.Concerns) > 0 {
					rows = append(rows, field{"Concerns", strings.Join(r.Concerns, "; ")})
				}
				if r.Source == "guard" {
					rows = append(rows, field{"Decided by", "a Wayglass guard, not the model"})
				} else if r.Confidence != nil {
					rows = append(rows, field{"Confidence", fmt.Sprintf("%.2f", *r.Confidence)})
				}
				fields(o, st, rows...)
				_, err := lipgloss.Fprintln(o, res.Next)
				return err
			})
		},
	}
	return cmd
}

// aiVia says what the call cost and went through: "(via gateway wayglass, 1.2 s, 321+44 tokens)".
func aiVia(r api.AIVerdict) string {
	var parts []string
	if r.Gateway != nil && *r.Gateway != "" {
		parts = append(parts, "via gateway "+*r.Gateway)
	}
	if r.LatencyMS > 0 {
		parts = append(parts, fmt.Sprintf("%.1f s", float64(r.LatencyMS)/1000))
	}
	if r.Tokens != nil {
		parts = append(parts, fmt.Sprintf("%d+%d tokens", r.Tokens.Prompt, r.Tokens.Completion))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func aiReviewNext(out *api.AIReviewResult) string {
	if out.Review.Verdict == "approve" {
		return decisionNext(out.Decision, "", len(out.Approvals))
	}
	return "Escalated: a person decides it now with 'wayglass review'; the AI's reason and concerns are in the audit log."
}
