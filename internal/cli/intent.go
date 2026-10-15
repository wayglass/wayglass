package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
)

func newIntentCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intent",
		Short: "Declare and inspect intents (the why before the code)",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  groupRunE(a),
	}
	cmd.AddCommand(newIntentDeclareCmd(a), newIntentListCmd(a), newIntentShowCmd(a), newIntentAbandonCmd(a), newIntentRestackCmd(a))
	return cmd
}

func newIntentDeclareCmd(a *app) *cobra.Command {
	var goal, reason, on, family string
	var paths []string
	var showToken bool
	cmd := &cobra.Command{
		Use:   "declare",
		Short: "Declare an intent and get a fork with a short-lived write token",
		Long: "Registers the intent with the Worker, which forks the trunk for it and\n" +
			"mints a write token scoped to that fork only. The token is printed only in\n" +
			"--json output (or with --show-token) and is never written to disk.\n" +
			"The intent becomes this checkout's current intent for push and propose.\n\n" +
			"With --on <intent-id> the intent is stacked on another unlanded intent:\n" +
			"its fork starts from that intent's pushed head, so it is classified on its\n" +
			"own change only, and it can land only after the parent has landed.",
		Example: `  wayglass intent declare --goal "Add rate limiting to login" \
    --path "web/login.ts" --path "worker/src/limits/**" \
    --reason "Brute-force attempts in the audit log" --json`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case strings.TrimSpace(goal) == "":
				return UsageError("--goal is required")
			case strings.TrimSpace(reason) == "":
				return UsageError("--reason is required: every change carries its why")
			case len(paths) == 0:
				return UsageError("at least one --path is required")
			}
			fam, err := familyFlag(family)
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
			in := api.DeclareIntentInput{
				Project: w.cfg.Project, Goal: goal, Reason: reason, Paths: paths, Agent: c.Agent, On: on, Family: fam,
			}
			warnings := []string{}
			res, err := c.DeclareIntent(ctx, in)
			// The Worker forks before it records the intent, so an upstream
			// failure created nothing and a retry cannot duplicate it.
			for i := 0; err != nil && isUpstream(err) && i < len(declareBackoff); i++ {
				warnings = append(warnings, fmt.Sprintf("Artifacts fork failed (%v); retried", err))
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(declareBackoff[i]):
				}
				res, err = c.DeclareIntent(ctx, in)
			}
			if err != nil {
				return err
			}
			if on != "" && deref(res.Intent.ParentID) != on {
				return errorf(CodeStackUnsupported, ExitError,
					"the Worker ignored --on: intent %s was forked from the trunk, so its diff would include %s's change. Land %s first, or use a Worker with stacked intents",
					res.Intent.ID, on, on)
			}
			if err := w.setCurrentIntent(res.Intent.ID); err != nil {
				return err
			}
			synced, warn := syncToFork(ctx, w, res)
			if warn != "" {
				warnings = append(warnings, warn)
			}
			o := a.output(cmd)
			for _, m := range warnings {
				o.Warn("%s", m)
			}
			warnOverlaps(o, res.Intent.ID, res.Overlaps)
			if res.Overlaps == nil {
				res.Overlaps = []api.Overlap{}
			}
			out := declareResult{DeclareResult: *res, FastForwarded: synced, Warnings: warnings}
			return o.Emit(out, func(out io.Writer) error {
				st := cliStyles(o)
				_, _ = lipgloss.Fprintf(out, "%s %s  %s\n", st.title.Render("Declared"), st.id(res.Intent.ID), res.Intent.Goal)
				if on != "" {
					_, _ = lipgloss.Fprintf(out, "Stacked on %s at %s; it lands after %s does.\n", st.id(on), st.id(short(deref(res.Intent.BaseCommit))), on)
				}
				if synced != "" {
					_, _ = lipgloss.Fprintf(out, "Checkout fast-forwarded to the fork start %s.\n", st.id(short(synced)))
				}
				token := "write, expires " + localTime(res.Token.ExpiresAt)
				if showToken {
					token += "\n" + res.Token.Value
				}
				fields(out, st,
					field{"Fork", res.Fork.Remote},
					field{"Token", token},
					field{"Next", "commit your change, then 'wayglass push'."},
				)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&goal, "goal", "", "what this change will do")
	cmd.Flags().StringVar(&reason, "reason", "", "why it is needed")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "expected path or glob (repeatable)")
	cmd.Flags().BoolVar(&showToken, "show-token", false, "print the fork token in human output too")
	cmd.Flags().StringVar(&on, "on", "", "stack on this unlanded intent (fork from its head, land after it)")
	cmd.Flags().StringVar(&family, "family", "", "your model family: claude, openai, gemini, meta, other or human (default $"+api.EnvFamily+"); policy can require a reviewer from another")
	return cmd
}

// declareBackoff is how long declare waits before each retry of a
// transient Artifacts fork failure. Several agents declaring at once
// occasionally get INTERNAL_ERROR from the fork; a second try succeeds.
var declareBackoff = []time.Duration{time.Second, 3 * time.Second}

func isUpstream(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == api.CodeUpstream
}

// CodeStackUnsupported means the Worker created the intent but did not stack it.
const CodeStackUnsupported = "stack_unsupported"

// declareResult is the declare reply plus client-side warnings.
type declareResult struct {
	api.DeclareResult
	// FastForwarded is the fork start this checkout was moved to, when its
	// HEAD was behind it (the trunk moved, or --on a parent's head).
	FastForwarded string   `json:"fast_forwarded,omitempty"`
	Warnings      []string `json:"warnings"`
}

// syncToFork records where the new fork starts (its base ref, which push
// and propose diff against) and fast-forwards a clean checkout that is
// behind it. Without this, an agent whose clone predates a land, or that
// stacks with --on, would push a HEAD the fork cannot fast-forward to, or
// carry someone else's change in its diff. It never rewrites local
// commits: a diverged or dirty checkout only gets a warning.
func syncToFork(ctx context.Context, w *workspace, res *api.DeclareResult) (string, string) {
	branch := cmp.Or(res.Fork.DefaultBranch, "main")
	tg := w.git
	tg.Token = res.Token.Value // in memory only
	if res.Token.Value == "" || res.Fork.Remote == "" {
		return "", ""
	}
	ref := baseRef(res.Intent.ID)
	if err := tg.Fetch(ctx, res.Fork.Remote, "+refs/heads/"+branch+":"+ref); err != nil {
		var ge *gitx.Error
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref") {
			return "", "" // empty fork; push records the base
		}
		return "", "could not fetch the fork start (push will record it): " + err.Error()
	}
	base, ok, err := w.git.Resolve(ctx, ref)
	if err != nil || !ok {
		return "", ""
	}
	head, err := w.git.Head(ctx)
	if err != nil || head == base || isAncestor(ctx, w.git, base, head) {
		return "", ""
	}
	advice := fmt.Sprintf("this checkout's HEAD %s does not contain the fork start %s; run 'git merge %s' before you commit, or the push will be rejected or carry unrelated changes", short(head), short(base), short(base))
	if !isAncestor(ctx, w.git, head, base) {
		return "", advice
	}
	if dirty, err := w.git.Dirty(ctx); err != nil || dirty {
		return "", advice
	}
	if _, err := w.git.Run(ctx, "merge", "--ff-only", "--quiet", base); err != nil {
		return "", advice
	}
	return base, ""
}

// activeStatuses are intents that still compete for paths.
var activeStatuses = []string{"open", "proposed"}

func newIntentListCmd(a *app) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List open intents",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := a.client()
			if err != nil {
				return err
			}
			w, err := openWorkspace(ctx)
			if err != nil {
				return err
			}
			intents, err := c.ListIntents(ctx, w.cfg.Project, "")
			if err != nil {
				return err
			}
			if !all {
				intents = slices.DeleteFunc(intents, func(i api.Intent) bool { return !slices.Contains(activeStatuses, i.Status) })
			}
			if intents == nil {
				intents = []api.Intent{}
			}
			current := w.currentIntent()
			return a.output(cmd).Emit(map[string]any{"project": w.cfg.Project, "intents": intents}, func(out io.Writer) error {
				if len(intents) == 0 {
					_, err := fmt.Fprintln(out, "No open intents.")
					return err
				}
				st := cliStyles(a.output(cmd))
				statusW, agentW := 0, 0
				for _, i := range intents {
					statusW = max(statusW, len(intentStatus(i)))
					agentW = max(agentW, len(i.Agent))
				}
				for _, i := range intents {
					mark := " "
					if i.ID == current {
						mark = st.title.Render("*")
					}
					status := fmt.Sprintf("%-*s", statusW, intentStatus(i))
					if i.Stale {
						status = st.attention.Render(status)
					}
					_, _ = lipgloss.Fprintf(out, "%s %s  %s  %-*s  %s\n", mark, st.id(i.ID), status, agentW, i.Agent, i.Goal)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include landed and abandoned intents")
	return cmd
}

func newIntentShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <intent-id>",
		Short: "Show one intent with its paths, pushes and decisions",
		Args:  usageArgs(cobra.ExactArgs(1)),
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
			it, err := c.GetIntent(ctx, w.cfg.Project, args[0])
			if err != nil {
				return err
			}
			data := map[string]any{"intent": it}
			cost, err := intentCost(ctx, c, w.cfg.Project, it.ID)
			if err != nil {
				return err
			}
			if cost != nil {
				data["cost"] = cost
			}
			return a.output(cmd).Emit(data, func(out io.Writer) error {
				by := it.Agent
				if f := deref(it.AgentFamily); f != "" {
					by += ", " + f
				}
				st := cliStyles(a.output(cmd))
				_, _ = lipgloss.Fprintf(out, "%s  %s  %s\n", st.title.Render(it.ID), it.Goal, st.muted.Render("("+it.Status+", by "+by+")"))
				rows := []field{{"Why", it.Reason}}
				if r := deref(it.AbandonedReason); it.Status == "abandoned" && r != "" {
					rows = append(rows, field{"Dropped", "by " + deref(it.AbandonedBy) + ": " + r})
				}
				if it.Stale {
					since := localTime(it.CreatedAt) + " (declared, never pushed)"
					if p := deref(it.LastPushAt); p != "" {
						since = localTime(p)
					}
					rows = append(rows, field{"Stale", st.attention.Render("no push since " + since)})
				}
				rows = append(rows, field{"Declared", strings.Join(it.DeclaredPaths, ", ")})
				if len(it.ActualPaths) > 0 {
					rows = append(rows, field{"Changed", strings.Join(it.ActualPaths, ", ")})
				}
				if p := deref(it.ParentID); p != "" {
					rows = append(rows, field{"Stacked", "on " + st.id(p) + " at " + st.id(short(deref(it.BaseCommit))) + "; lands after it"})
				}
				if h := it.Head(); h != "" {
					rows = append(rows, field{"Head", st.id(short(h))})
				}
				rows = append(rows, field{"Fork", it.ForkRemote})
				if cost != nil {
					rows = append(rows, field{"Cost", fmt.Sprintf("%d Artifacts ops (%s), %d token(s) minted, ≈%s est.\n%s",
						cost.Ops, byOpText(cost.ByOp), cost.TokensMinted, usdText(cost.EstUSD), st.muted.Render(cost.Basis))})
				}
				fields(out, st, rows...)
				return nil
			})
		},
	}
}

func newIntentAbandonCmd(a *app) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "abandon <intent-id>",
		Short: "Give up an open or proposed intent, with a reason",
		Long: "Marks the intent abandoned. Only its author (the agent a bound key names,\n" +
			"or the name it declared with) or the admin may. The reason goes to the\n" +
			"audit log. The intent drops out of overlap checks, watch and the review\n" +
			"queues; its fork tokens are revoked and no new ones are minted. The fork\n" +
			"repo is kept, nothing is deleted.\n\n" +
			"Intents stacked on it (declare --on, session fork) take over its commits:\n" +
			"they are re-stacked onto its base and must propose again before landing.",
		Example: `  wayglass intent abandon i0123456789 --reason "Superseded by i9876543210, which also covers lockout" --json`,
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason = strings.TrimSpace(reason)
			switch {
			case reason == "":
				return UsageError("--reason is required: the audit log keeps why work was dropped")
			case placeholderReason.MatchString(reason):
				return UsageError("--reason %q looks like a placeholder; say why the work is dropped", reason)
			case fillerReason(reason):
				return UsageError("--reason %q is a rubber stamp, not a why; say why the work is dropped", reason)
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
			res, err := c.AbandonIntent(ctx, w.cfg.Project, args[0], reason)
			if err != nil {
				return err
			}
			if res.Restacked == nil {
				res.Restacked = []string{}
			}
			cleared := false
			if w.currentIntent() == res.Intent.ID {
				if err := w.setCurrentIntent(""); err != nil {
					return err
				}
				cleared = true
			}
			out := struct {
				*api.AbandonResult
				ClearedCurrent bool `json:"cleared_current"`
			}{res, cleared}
			return a.output(cmd).Emit(out, func(o io.Writer) error {
				fmt.Fprintf(o, "Abandoned %s: %s\n", res.Intent.ID, res.Intent.Goal)
				fmt.Fprintf(o, "Why:       %s\n", reason)
				fmt.Fprintf(o, "Fork:      kept (%s); %d live token(s) revoked\n", res.Intent.ForkRepo, res.TokensRevoked)
				for _, id := range res.Restacked {
					fmt.Fprintf(o, "Restacked: %s now carries its commits; it must propose again before it lands\n", id)
				}
				if cleared {
					_, err := fmt.Fprintln(o, "This checkout has no current intent now; declare a new one to keep working.")
					return err
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the work is dropped (required)")
	return cmd
}

// warnOverlaps tells people about overlaps on stderr; agents read them from
// the overlaps field of the JSON data.
func warnOverlaps(o Output, id string, overlaps []api.Overlap) {
	for _, ov := range overlaps {
		pairs := make([]string, 0, len(ov.Paths))
		for _, p := range ov.Paths {
			pairs = append(pairs, p.Mine+" ~ "+p.Theirs)
		}
		o.Warn("%s overlaps %s (%s, %s): %s", id, ov.IntentID, ov.Agent, ov.Goal, strings.Join(pairs, ", "))
	}
}

// intentCostOut is one intent's Worker-side Artifacts cost, with its basis.
type intentCostOut struct {
	api.CostLine
	Basis string `json:"basis"`
}

// intentCost is nil when the Worker predates GET /v1/cost or has no calls
// recorded for the intent (declared before metering).
func intentCost(ctx context.Context, c *api.Client, project, id string) (*intentCostOut, error) {
	sum, err := c.Cost(ctx, project)
	if isMissingRoute(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	line, ok := sum.Intents[id]
	if !ok {
		return nil, nil
	}
	return &intentCostOut{CostLine: line, Basis: costBasis(sum)}, nil
}

func byOpText(m map[string]int) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func usdText(v float64) string {
	if v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// intentStatus is an intent's status as 'intent list' shows it.
func intentStatus(i api.Intent) string {
	if i.Stale {
		return i.Status + " (stale)"
	}
	return i.Status
}
