package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/notes"
	"github.com/wayglass/wayglass/internal/policy"
)

const (
	defaultReplayLimit = 20
	maxReplayLimit     = 500
)

func newPolicyCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Work with .wayglass/policy.yaml",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  groupRunE(a),
	}
	cmd.AddCommand(newPolicyTestCmd(a))
	return cmd
}

// replayRecorded is the decision a commit's why note recorded when it landed.
type replayRecorded struct {
	Rule      string `json:"rule"`
	Action    string `json:"action"`
	DecidedBy string `json:"decided_by"`
}

type replayCommit struct {
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	// Decision is what the local policy file decides for this commit today,
	// with its shadow_matches and would_differ.
	Decision policy.Decision `json:"decision"`
	// Hits is every rule that matched, enforced or shadow, in file order.
	Hits []policy.RuleHit `json:"hits"`
	// Recorded is the decision in the commit's why note; nil without one.
	Recorded *replayRecorded `json:"recorded"`
	// Changed is true when the replay decides a different action than the
	// recorded one, meaning the policy file changed since.
	Changed bool `json:"changed"`
}

type replaySummary struct {
	Commits       int `json:"commits"`
	ShadowMatched int `json:"shadow_matched"`
	WouldDiffer   int `json:"would_differ"`
	Recorded      int `json:"recorded"`
	Changed       int `json:"changed"`
}

type replayResult struct {
	PolicyFile   string             `json:"policy_file"`
	Range        string             `json:"range"`
	Jurisdiction string             `json:"jurisdiction"`
	Limit        int                `json:"limit"`
	Truncated    bool               `json:"truncated"`
	Commits      []replayCommit     `json:"commits"`
	Rules        []policy.RuleStats `json:"rules"`
	Summary      replaySummary      `json:"summary"`
}

func newPolicyTestCmd(a *app) *cobra.Command {
	var (
		since, rev, file, jurisdiction string
		limit                          int
	)
	cmd := &cobra.Command{
		Use:     "test",
		Aliases: []string{"replay"},
		Short:   "Replay recent commits through the local policy, shadow rules included",
		Long: "Classifies each recent commit on the current branch (first parent only)\n" +
			"and decides it with the local policy file, so a rule can be tried before\n" +
			"it is enforced: add it with `mode: shadow`, run this, and read what it\n" +
			"would have decided. Nothing is recorded and the Worker is not contacted.\n" +
			"Commits with a why note also show the decision recorded when they landed.",
		Example: "  wayglass policy test\n" +
			"  wayglass policy test --since v0.1.0\n" +
			"  wayglass policy test --file ../drafts/policy.yaml --json",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			if limit < 1 || limit > maxReplayLimit {
				return UsageError("--limit must be between 1 and %d", maxReplayLimit)
			}
			w, err := openWorkspace(ctx)
			if err != nil {
				var e *Error
				if !errors.As(err, &e) || e.Code != CodeNotInitialized {
					return err
				}
				// No project yet: a policy file can still be tried.
				if w, err = openRepo(ctx); err != nil {
					return err
				}
			}
			path := cmp.Or(file, w.path(policyFile))
			pol, err := policy.LoadFile(path)
			if err != nil {
				return errorf(CodePolicy, ExitError, "%v", err)
			}
			if !cmd.Flags().Changed("jurisdiction") {
				jurisdiction = w.cfg.Jurisdiction
			}
			rng := rev
			if since != "" {
				rng = since + ".." + rev
			}
			// One extra commit tells us whether the range was cut short.
			log, err := w.git.Commits(ctx, rng, limit+1)
			if err != nil {
				return errorf(CodeNotRepo, ExitError, "cannot list %s: %v", rng, err)
			}
			res := replayResult{
				PolicyFile: relPath(w.root, path), Range: rng, Jurisdiction: jurisdiction, Limit: limit,
				Commits: []replayCommit{}, Rules: []policy.RuleStats{},
			}
			if len(log) > limit {
				log, res.Truncated = log[:limit], true
			}
			ds := make([]policy.Decision, 0, len(log))
			for _, c := range log {
				diff, err := w.git.Diff(ctx, c.Parent, c.SHA)
				if err != nil {
					return err
				}
				files, err := classify.ParseUnifiedDiff(strings.NewReader(diff))
				if err != nil {
					return fmt.Errorf("commit %s: %w", short(c.SHA), err)
				}
				if err := sizeBinaries(ctx, w.git, c.SHA, files); err != nil {
					return fmt.Errorf("commit %s: %w", short(c.SHA), err)
				}
				d := pol.Evaluate(classify.Change{Files: files, Jurisdiction: jurisdiction})
				rc := replayCommit{Commit: c.SHA, Subject: c.Subject, Author: c.Author, Decision: d, Hits: pol.Hits(d.Tags)}
				n, err := notes.Read(ctx, w.git, c.SHA)
				switch {
				case err == nil:
					rc.Recorded = &replayRecorded{Rule: n.Rule, Action: n.Action, DecidedBy: n.DecidedBy}
					rc.Changed = n.Action != string(d.Action)
				case !errors.Is(err, notes.ErrNoNote):
					o.Warn("reading the why note on %s: %v", short(c.SHA), err)
				}
				res.Commits = append(res.Commits, rc)
				ds = append(ds, d)
				res.Summary.Commits++
				if len(d.ShadowMatches) > 0 {
					res.Summary.ShadowMatched++
				}
				if d.WouldDiffer {
					res.Summary.WouldDiffer++
				}
				if rc.Recorded != nil {
					res.Summary.Recorded++
				}
				if rc.Changed {
					res.Summary.Changed++
				}
			}
			res.Rules = pol.Summarize(ds)
			if res.Truncated && since != "" {
				o.Warn("showing the newest %d commits of %s; raise --limit to see more", limit, rng)
			}
			return o.Emit(res, func(out io.Writer) error { return renderReplay(out, res) })
		},
	}
	f := cmd.Flags()
	f.StringVar(&since, "since", "", "replay commits after this revision (since..rev)")
	f.StringVar(&rev, "rev", "HEAD", "replay commits reachable from this revision")
	f.IntVarP(&limit, "limit", "n", defaultReplayLimit, fmt.Sprintf("replay at most this many commits (max %d)", maxReplayLimit))
	f.StringVar(&file, "file", "", "policy file to try (default .wayglass/policy.yaml)")
	f.StringVar(&jurisdiction, "jurisdiction", "", "jurisdiction to tag changes with (default from .wayglass/config.yaml)")
	return cmd
}

func relPath(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func renderReplay(out io.Writer, r replayResult) error {
	if len(r.Commits) == 0 {
		_, err := fmt.Fprintf(out, "No commits in %s.\n", r.Range)
		return err
	}
	what := fmt.Sprintf("%d %s in %s", len(r.Commits), plural(len(r.Commits), "commit", "commits"), r.Range)
	if r.Truncated {
		what = fmt.Sprintf("the newest %d commits of %s", len(r.Commits), r.Range)
	}
	fmt.Fprintf(out, "Replaying %s through %s\n\n", what, r.PolicyFile)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, c := range r.Commits {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", short(c.Commit), c.Decision.Action, c.Decision.Rule.ID(), clipText(c.Subject, 60))
		if line := shadowLine(c.Decision.ShadowMatches, c.Decision.WouldDiffer); line != "" {
			fmt.Fprintf(tw, "  \t\t\t%s\n", line)
		}
		if c.Changed {
			fmt.Fprintf(tw, "  \t\t\trecorded when it landed: %s by %s\n", c.Recorded.Action, c.Recorded.Rule)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nRules")
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  RULE\tMODE\tACTION\tMATCHED\tDECIDED\tWOULD DIFFER")
	for _, s := range r.Rules {
		mode, differ := cmp.Or(string(s.Mode), "-"), "-"
		if s.Mode == policy.Shadow {
			differ = fmt.Sprint(s.Differed)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%s\n", s.Rule, mode, s.Action, s.Matched, s.Decided, differ)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	sum := r.Summary
	fmt.Fprintln(out)
	if sum.ShadowMatched == 0 {
		fmt.Fprintln(out, "No shadow rule matched these commits.")
	} else {
		fmt.Fprintf(out, "Enforcing the shadow rules would change %d of %d %s.\n", sum.WouldDiffer, sum.Commits, plural(sum.Commits, "commit", "commits"))
	}
	if sum.Changed > 0 {
		fmt.Fprintf(out, "%d of %d recorded %s would be decided differently by this file today.\n", sum.Changed, sum.Recorded, plural(sum.Recorded, "decision", "decisions"))
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// clipText shortens s to n runes with an ellipsis.
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
