package cli

import (
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

// aiAdviceResult is the output of ai-advice.
type aiAdviceResult struct {
	IntentID string       `json:"intent_id"`
	Decision api.Decision `json:"decision"`
	Advice   api.AIAdvice `json:"advice"`
	Next     string       `json:"next"`
	// Untrusted lists the fields a model wrote from the change's own text.
	// A person reads them as a second opinion to check, never as a verdict.
	Untrusted []string `json:"untrusted"`
}

var adviceUntrusted = []string{"advice.summary", "advice.risks", "advice.questions"}

func newAIAdviceCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ai-advice <intent-id>",
		Short: "Ask the Worker's AI for advice on a change waiting for a person",
		Long: "Asks the Worker to have its AI read the intent's change and write advice for\n" +
			"the person deciding it: what the change does against its goal, the risks it\n" +
			"sees and the questions to ask. The advice is stored on the decision and in\n" +
			"the audit log. It never decides: the change stays in your queue exactly as\n" +
			"it was, and only you approve or reject it.\n\n" +
			"Works on changes policy routed to a person (human_approve, multi_approve,\n" +
			"quarantine). An agent_review change is reviewed instead: 'wayglass ai-review'.\n" +
			"The Worker must have the reviewer on (AI_REVIEW=manual or auto).",
		Example: "  wayglass ai-advice i269d1c348e\n  wayglass ai-advice i269d1c348e --json",
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
			out, err := c.AIAdviseIntent(ctx, w.cfg.Project, args[0])
			if api.IsCode(err, api.CodeAIUnavailable) {
				return errorf(api.CodeAIUnavailable, ExitError, "the Worker's AI is not available (%s); read the change with 'wayglass diff %s'", err.Error(), args[0])
			}
			if err != nil {
				return err
			}
			res := aiAdviceResult{IntentID: args[0], Decision: out.Decision, Advice: out.Advice, Untrusted: adviceUntrusted}
			res.Next = "Advice recorded; the decision is unchanged. Decide with 'wayglass review' or 'wayglass approve|reject " + args[0] + " --reason ...'."
			return a.output(cmd).Emit(res, func(o io.Writer) error {
				st := cliStyles(a.output(cmd))
				ad := res.Advice
				_, _ = lipgloss.Fprintf(o, "AI advice on %s (decision %s, still %s)\n", st.id(res.IntentID), st.id(res.Decision.ID), st.status(res.Decision.Status))
				rows := []field{{"Adviser", ad.Reviewer + aiVia(api.AIVerdict{Gateway: ad.Gateway, LatencyMS: ad.LatencyMS, Tokens: ad.Tokens})}, {"Summary", ad.Summary}, {"Matches goal", ad.MatchesGoal}}
				if len(ad.Risks) > 0 {
					rows = append(rows, field{"Risks", strings.Join(ad.Risks, "; ")})
				}
				if len(ad.Questions) > 0 {
					rows = append(rows, field{"Questions", strings.Join(ad.Questions, "; ")})
				}
				for _, wn := range ad.Warnings {
					rows = append(rows, field{"Warning", st.stopped.Render(wn)})
				}
				if ad.Source == "guard" {
					rows = append(rows, field{"Written by", "a Wayglass guard, not the model"})
				}
				fields(o, st, rows...)
				_, err := lipgloss.Fprintln(o, res.Next)
				return err
			})
		},
	}
	return cmd
}
