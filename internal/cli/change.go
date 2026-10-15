package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/intent"
	"github.com/wayglass/wayglass/internal/notes"
	"github.com/wayglass/wayglass/internal/policy"
)

// maxReportedPaths matches the Worker's limit on one push report.
const maxReportedPaths = 200

// session is one command's view of an intent: the checkout, the Worker and
// the intent record.
type session struct {
	w      *workspace
	c      *api.Client
	intent *api.Intent
}

func (a *app) openSession(ctx context.Context, args []string) (*session, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	w, err := openWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	id, err := w.resolveIntent(args)
	if err != nil {
		return nil, err
	}
	it, err := c.GetIntent(ctx, w.cfg.Project, id)
	if err != nil {
		return nil, err
	}
	return &session{w: w, c: c, intent: it}, nil
}

// forkToken gets a write token for the intent's fork, held in memory only:
// a fresh one from the Worker if it can mint one, else the token the agent
// kept from `intent declare` in WAYGLASS_FORK_TOKEN.
func (s *session) forkToken(ctx context.Context) (string, error) {
	t, err := s.c.ForkToken(ctx, s.w.cfg.Project, s.intent.ID)
	if err == nil && t.Value != "" {
		return t.Value, nil
	}
	if err != nil && !api.IsCode(err, api.CodeNotFound) && !api.IsCode(err, api.CodeMethodNotAllowed) {
		return "", err
	}
	if v := os.Getenv(EnvForkToken); v != "" {
		return v, nil
	}
	return "", errorf(CodeNoToken, ExitError,
		"the Worker did not mint a fork token; set %s to the token.value from 'wayglass intent declare --json'", EnvForkToken)
}

// base returns the fork base for this intent, or "" if none yet. The
// Worker's base_commit wins when this checkout has it: it moves when the
// intent is re-stacked because its parent was abandoned, and the local ref
// follows so push reports the whole change the next decision covers.
func (s *session) base(ctx context.Context) (string, bool, error) {
	ref := baseRef(s.intent.ID)
	if b := deref(s.intent.BaseCommit); b != "" {
		if sha, ok, err := s.w.git.Resolve(ctx, b+"^{commit}"); err == nil && ok {
			if cur, has, _ := s.w.git.Resolve(ctx, ref); !has || cur != sha {
				if _, err := s.w.git.Run(ctx, "update-ref", ref, sha); err != nil {
					return "", false, err
				}
			}
			return sha, true, nil
		}
	}
	sha, ok, err := s.w.git.Resolve(ctx, ref)
	return sha, ok, err
}

// ownBase is where the intent's own change starts for push reports and the
// propose preview: the merge base with the trunk this checkout last fetched
// (refs/wayglass/trunk, from wayglass sync or land) when that trunk is at or
// past the fork base, so trunk files a sync merged in are not counted as the
// intent's. The returned base is used as a three-dot range, which git
// resolves to that merge base. The Worker makes the same cut on its own
// (worker/src/ownchange.ts); its decision is the one that counts.
func ownBase(ctx context.Context, g gitx.Git, base string) string {
	trunk, ok, err := g.Resolve(ctx, trunkRef)
	if err != nil || !ok || trunk == base {
		return base
	}
	if base != "" && !isAncestor(ctx, g, base, trunk) {
		return base
	}
	return trunk
}

// sizeBinaries fills in the size of each binary file at rev, so the preview
// weighs assets the way the Worker does (classify.RiskRule.BinaryBytesOver).
// A unified diff says only that a file is binary, never how large it is.
func sizeBinaries(ctx context.Context, g gitx.Git, rev string, files []classify.FileChange) error {
	var paths []string
	for _, f := range files {
		if f.Binary {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sizes, err := g.BlobSizes(ctx, rev, paths)
	if err != nil {
		return err
	}
	for i := range files {
		if files[i].Binary {
			files[i].Bytes = sizes[files[i].Path]
		}
	}
	return nil
}

type pushResult struct {
	IntentID  string       `json:"intent_id"`
	Commit    string       `json:"commit"`
	Committed bool         `json:"committed"`
	Branch    string       `json:"branch"`
	Base      string       `json:"base"`
	Files     []string     `json:"files"`
	Notes     bool         `json:"notes_pushed"`
	Overlaps  []overlapOut `json:"overlaps"`
	Warnings  []string     `json:"warnings"`
}

func newPushCmd(a *app) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   "push [intent-id]",
		Short: "Push the intent's fork and report changed files for overlap checks",
		Long: "Pushes HEAD to the intent's fork in Artifacts, then reports the files\n" +
			"changed since the fork was created so the Worker can re-check overlaps.\n" +
			"With -m, stages and commits every change first. The fork token stays in\n" +
			"memory and reaches git only through its environment.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			s, err := a.openSession(ctx, args)
			if err != nil {
				return err
			}
			if !slices.Contains(activeStatuses, s.intent.Status) {
				return errorf(api.CodeConflict, ExitError, "intent %s is %s", s.intent.ID, s.intent.Status)
			}
			g := s.w.git
			res := pushResult{IntentID: s.intent.ID, Warnings: []string{}}

			if message != "" {
				if dirty, err := g.Dirty(ctx); err != nil {
					return err
				} else if dirty {
					msg := strings.TrimSpace(message) + "\n\nWayglass-Intent: " + s.intent.ID + "\n"
					if _, err := g.CommitAll(ctx, msg); err != nil {
						return err
					}
					res.Committed = true
				}
			}
			head, err := g.Head(ctx)
			if err != nil {
				return err
			}
			res.Commit = head
			if dirty, err := g.Dirty(ctx); err == nil && dirty {
				res.Warnings = append(res.Warnings, "uncommitted changes were not pushed; use -m to commit them")
			}

			token, err := s.forkToken(ctx)
			if err != nil {
				return err
			}
			tg := g
			tg.Token = token
			remote := s.intent.ForkRemote

			branch, err := tg.RemoteDefaultBranch(ctx, remote)
			if err != nil {
				return err
			}
			res.Branch = cmp.Or(branch, "main")

			base, haveBase, err := s.base(ctx)
			if err != nil {
				return err
			}
			if !haveBase {
				// First push from this checkout: remember where the fork started.
				err := tg.Fetch(ctx, remote, "+refs/heads/"+res.Branch+":"+baseRef(s.intent.ID))
				var ge *gitx.Error
				switch {
				case err == nil:
					base, _, err = s.base(ctx)
					if err != nil {
						return err
					}
				case errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref"):
					res.Warnings = append(res.Warnings, "fork is empty; every file in HEAD counts as changed")
				default:
					return err
				}
			}
			res.Base = base

			// Measure the change before pushing: a change the Worker would
			// refuse to record must not reach the fork unreported.
			files, err := g.ChangedFiles(ctx, ownBase(ctx, g, base), head)
			if err != nil {
				return err
			}
			if len(files) > maxReportedPaths {
				return errorf(CodeError, ExitError, "%d files changed; the Worker accepts at most %d per report, so nothing was pushed. Split the intent", len(files), maxReportedPaths)
			}
			res.Files = files
			if res.Files == nil {
				res.Files = []string{}
			}

			if err := tg.Push(ctx, remote, "HEAD:refs/heads/"+res.Branch); err != nil {
				return err
			}
			if ok, err := notes.Exists(ctx, g); err == nil && ok {
				if err := notes.Push(ctx, tg, remote); err != nil {
					res.Warnings = append(res.Warnings, "pushing "+notes.Ref+" failed: "+err.Error())
				} else {
					res.Notes = true
				}
			}
			rep, err := s.c.ReportPush(ctx, s.intent.ID, api.PushInput{Project: s.w.cfg.Project, Paths: res.Files, Commit: head})
			if err != nil {
				return fmt.Errorf("pushed %s, but reporting it to the Worker failed: %w", short(head), err)
			}
			mine := rep.Intent
			if mine.ID == "" {
				mine = *s.intent
				mine.ActualPaths = res.Files
			}
			res.Overlaps, err = withReplans(ctx, s.c, s.w.cfg.Project, mine, rep.Overlaps)
			if err != nil {
				res.Warnings = append(res.Warnings, "could not read intents to suggest re-plans: "+err.Error())
			}

			for _, m := range res.Warnings {
				o.Warn("%s", m)
			}
			warnOverlaps(o, s.intent.ID, rep.Overlaps)
			for _, ov := range res.Overlaps {
				if ov.Replan != nil {
					o.Warn("suggested re-plan: %s", ov.Replan.Summary)
				}
			}
			return o.Emit(res, func(out io.Writer) error {
				st := cliStyles(o)
				_, err := lipgloss.Fprintf(out, "Pushed %s to %s (%s), %d file(s) changed.\n", st.id(short(head)), s.intent.ForkRepo, res.Branch, len(res.Files))
				return err
			})
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "stage and commit all changes with this message first")
	return cmd
}

// overlapPair is one pair of active intents whose paths intersect.
type overlapPair struct {
	A      overlapSide       `json:"a"`
	B      overlapSide       `json:"b"`
	Paths  []intent.PathPair `json:"paths"`
	Replan intent.Replan     `json:"replan"` // suggested, computed client-side
}

type overlapSide struct {
	IntentID string `json:"intent_id"`
	Goal     string `json:"goal"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
}

func side(i api.Intent) overlapSide {
	return overlapSide{IntentID: i.ID, Goal: i.Goal, Agent: i.Agent, Status: i.Status}
}

func newOverlapsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "overlaps [intent-id]",
		Short: "Show open intents whose declared or actual paths overlap",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
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
			all, err := c.ListIntents(ctx, w.cfg.Project, "")
			if err != nil {
				return err
			}
			active := slices.DeleteFunc(all, func(i api.Intent) bool { return !slices.Contains(activeStatuses, i.Status) })
			only := ""
			if len(args) == 1 {
				only = args[0]
				if !slices.ContainsFunc(active, func(i api.Intent) bool { return i.ID == only }) {
					return errorf(api.CodeNotFound, ExitError, "no open intent %s in project %s", only, w.cfg.Project)
				}
			}
			pairs := []overlapPair{}
			for i, x := range active {
				for _, y := range active[i+1:] {
					if only != "" && x.ID != only && y.ID != only {
						continue
					}
					if y.ID == only { // keep the asked-about intent on side A
						x, y = y, x
					}
					p := intent.Intersect(intent.Paths(x.DeclaredPaths, x.ActualPaths), intent.Paths(y.DeclaredPaths, y.ActualPaths))
					if r, ok := intent.SuggestReplan(replanSide(x), replanSide(y)); ok && len(p) > 0 {
						pairs = append(pairs, overlapPair{A: side(x), B: side(y), Paths: p, Replan: r})
					}
				}
			}
			return a.output(cmd).Emit(map[string]any{"project": w.cfg.Project, "overlaps": pairs}, func(out io.Writer) error {
				if len(pairs) == 0 {
					_, err := fmt.Fprintln(out, "No overlaps between open intents.")
					return err
				}
				st := cliStyles(a.output(cmd))
				for i, p := range pairs {
					if i > 0 {
						_, _ = lipgloss.Fprintln(out)
					}
					_, _ = lipgloss.Fprintf(out, "%s (%s) and %s (%s)\n", st.id(p.A.IntentID), p.A.Goal, st.id(p.B.IntentID), p.B.Goal)
					paths := make([]string, len(p.Paths))
					for j, pp := range p.Paths {
						paths[j] = pp.Mine + " ~ " + pp.Theirs
					}
					fields(out, st, field{"Paths", strings.Join(paths, "\n")}, field{"Re-plan", p.Replan.Summary + st.muted.Render(" (suggested)")})
				}
				return nil
			})
		},
	}
}

type proposeResult struct {
	IntentID string `json:"intent_id"`
	Commit   string `json:"commit"`
	DryRun   bool   `json:"dry_run"`
	// Preview is the local classification and policy decision. The Worker's
	// decision is authoritative (ARCHITECTURE D3).
	Preview  policy.Decision `json:"preview"`
	Decision *api.Decision   `json:"decision,omitempty"`
	// Agrees is false when the Worker decided differently from the preview.
	Agrees bool `json:"agrees"`
	// Undeclared lists changed files outside the intent's declared paths.
	// Overlap checks before this push could not see them coming.
	Undeclared []string `json:"undeclared_paths"`
	Note       bool     `json:"note_written"`
	Exit       int      `json:"exit"`
	// Status is the Worker's decision status ("" on a dry run); Next is
	// written from it.
	Status string   `json:"status,omitempty"`
	Next   string   `json:"next"`
	Warns  []string `json:"warnings"`
}

func newProposeCmd(a *app) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "propose [intent-id]",
		Short: "Classify the change and let policy decide (exit 3 blocked, 4 needs a human)",
		Long: "Classifies the pushed change locally against .wayglass/policy.yaml as a\n" +
			"preview, then sends it to the Worker, whose decision is authoritative.\n" +
			"Without --dry-run, each call records a new decision, replacing any existing approval.\n" +
			"To check review status without changing it, use 'wayglass watch --json'.\n" +
			"Exit codes: 0 may land (auto_merge, agent_review), 3 blocked, 4 a person\n" +
			"must act (human_approve, multi_approve, quarantine).",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			s, err := a.openSession(ctx, args)
			if err != nil {
				return err
			}
			g := s.w.git
			head, err := g.Head(ctx)
			if err != nil {
				return err
			}
			base, haveBase, err := s.base(ctx)
			if err != nil {
				return err
			}
			if !haveBase && s.intent.Head() == "" {
				return errorf(CodeNotPushed, ExitError, "intent %s has nothing pushed yet; run 'wayglass push' first", s.intent.ID)
			}
			res := proposeResult{IntentID: s.intent.ID, Commit: head, DryRun: dryRun, Warns: []string{}}
			if !haveBase {
				res.Warns = append(res.Warns, "no fork base recorded in this checkout; classifying every file in HEAD")
			}
			if !dryRun && s.intent.Head() != head {
				return errorf(CodeNotPushed, ExitError, "HEAD %s is not what intent %s last pushed (%s); run 'wayglass push' first",
					short(head), s.intent.ID, cmp.Or(short(s.intent.Head()), "nothing"))
			}

			pol, err := policy.LoadFile(s.w.path(policyFile))
			if err != nil {
				return errorf(CodePolicy, ExitError, "%v", err)
			}
			diff, err := g.Diff(ctx, ownBase(ctx, g, base), head)
			if err != nil {
				return err
			}
			files, err := classify.ParseUnifiedDiff(strings.NewReader(diff))
			if err != nil {
				return err
			}
			if err := sizeBinaries(ctx, g, head, files); err != nil {
				return err
			}
			res.Preview = pol.Evaluate(classify.Change{Files: files, Jurisdiction: s.w.cfg.Jurisdiction})
			changed := make([]string, len(files))
			for i, f := range files {
				changed[i] = f.Path
			}
			res.Undeclared = intent.Undeclared(s.intent.DeclaredPaths, changed)
			if n := len(res.Undeclared); n > 0 {
				res.Warns = append(res.Warns, fmt.Sprintf("%d changed file(s) outside the declared paths: %s; declare them next time so overlap checks see them early",
					n, strings.Join(res.Undeclared, ", ")))
			}
			res.Exit = exitForAction(res.Preview.Action)
			res.Agrees = true

			if !dryRun {
				in := api.ProposeInput{
					Project: s.w.cfg.Project, Commit: head, Tags: res.Preview.Tags,
					MatchedRule: res.Preview.Rule.ID(), Action: string(res.Preview.Action), Reason: res.Preview.Reason,
				}
				if res.Preview.Action == policy.MultiApprove {
					in.ApprovalsNeeded = res.Preview.Approvers
				}
				out, err := s.c.Propose(ctx, s.intent.ID, in)
				if err != nil {
					return err
				}
				d := out.Decision
				res.Decision = &d
				res.Exit = exitForStatus(d.Status)
				res.Status = d.Status
				res.Agrees = d.Action == string(res.Preview.Action)
				if !res.Agrees {
					res.Warns = append(res.Warns, fmt.Sprintf("the Worker decided %s, the local preview said %s; the Worker's decision stands", d.Action, res.Preview.Action))
				}
				if d.Status == api.StatusApproved {
					n := notes.Note{
						IntentID: s.intent.ID, Goal: s.intent.Goal, Reason: s.intent.Reason,
						Tags: res.Preview.Tags, Rule: cmp.Or(deref(d.MatchedRule), res.Preview.Rule.ID()),
						Action: d.Action, DecisionID: d.ID, Status: d.Status,
						DecidedBy: cmp.Or(deref(d.DecidedBy), "policy"), DecidedAt: parseTime(deref(d.DecidedAt)),
					}
					if err := notes.Write(ctx, g, head, n); err != nil {
						res.Warns = append(res.Warns, "writing the why note failed: "+err.Error())
					} else {
						res.Note = true
					}
				}
			}
			res.Next = nextStep(res, dryRun)
			for _, m := range res.Warns {
				o.Warn("%s", m)
			}
			if err := o.Emit(res, func(out io.Writer) error { return renderPropose(out, cliStyles(o), res) }); err != nil {
				return err
			}
			if res.Exit != ExitOK {
				return WithExit(res.Exit)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "classify and decide locally without recording anything")
	return cmd
}

func renderPropose(out io.Writer, st styles, r proposeResult) error {
	t := r.Preview.Tags
	rows := []field{
		{"Intent", st.id(r.IntentID) + " at " + st.id(short(r.Commit))},
		{"Area", listOrNone(t.Areas)},
		{"Data class", listOrNone(t.DataClasses)},
		{"Risk", fmt.Sprintf("%s (%d %s, %d %s)", t.Risk, t.LinesChanged, plural(t.LinesChanged, "line", "lines"), t.FilesChanged, plural(t.FilesChanged, "file", "files"))},
		{"Jurisdiction", cmp.Or(t.Jurisdiction, "none")},
	}
	if len(t.Unscanned) > 0 {
		rows = append(rows, field{"Unscanned", fmt.Sprintf("%d binary %s, %d bytes: %s", len(t.Unscanned), plural(len(t.Unscanned), "file", "files"), t.BinaryBytes, strings.Join(t.Unscanned, ", "))})
	}
	rows = append(rows, field{"Rule", fmt.Sprintf("%s -> %s: %s", r.Preview.Rule.ID(), st.action(string(r.Preview.Action)), r.Preview.Reason)})
	if r.Decision != nil {
		rows = append(rows, field{"Decision", fmt.Sprintf("%s: %s (%s)", st.id(r.Decision.ID), st.action(r.Decision.Action), st.status(r.Decision.Status))})
	}
	// The Worker's shadow record is authoritative; older Workers send none,
	// so fall back to the local preview.
	matches, differ := r.Preview.ShadowMatches, r.Preview.WouldDiffer
	// A wire v2 Worker always sends shadow_matches (at least []), so a nil
	// slice means a Worker that predates it.
	if r.Decision != nil && r.Decision.Detail.ShadowMatches != nil {
		matches, differ = r.Decision.Detail.ShadowMatches, r.Decision.Detail.WouldDiffer
	}
	if line := shadowLine(matches, differ); line != "" {
		rows = append(rows, field{"Shadow", strings.TrimPrefix(line, "shadow: ")})
	}
	fields(out, st, rows...)
	if r.Decision == nil {
		_, _ = lipgloss.Fprintln(out, st.muted.Render("Dry run: nothing recorded."))
	}
	_, err := lipgloss.Fprintln(out, r.Next)
	return err
}

// shadowLine is the calm one-line summary of shadow rules on a decision, or
// "" when none matched.
func shadowLine(matches []policy.ShadowMatch, wouldDiffer bool) string {
	if len(matches) == 0 {
		return ""
	}
	first := matches[0]
	for _, m := range matches {
		if m.Preempts {
			first = m
			break
		}
	}
	if wouldDiffer {
		return fmt.Sprintf("shadow: %s would have %s", first.Rule, wouldHave(first.Action, first.Approvers))
	}
	if len(matches) == 1 {
		return fmt.Sprintf("shadow: %s matched; enforcing it would not change this", first.Rule)
	}
	return fmt.Sprintf("shadow: %d rules matched; enforcing them would not change this", len(matches))
}

// wouldHave phrases an action as what a rule would have done to the change.
func wouldHave(a policy.Action, approvers int) string {
	switch a {
	case policy.AutoMerge:
		return "auto-merged this"
	case policy.AgentReview:
		return "sent this to agent review"
	case policy.HumanApprove:
		return "sent this to a person"
	case policy.MultiApprove:
		return fmt.Sprintf("needed %d approvers for this", max(approvers, policy.DefaultMultiApprovers))
	case policy.Quarantine:
		return "quarantined this"
	case policy.Block:
		return "blocked this"
	default:
		return "decided " + string(a)
	}
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

func exitForAction(a policy.Action) int {
	switch {
	case a == policy.Block:
		return ExitBlocked
	case a.NeedsHuman():
		return ExitNeedsHuman
	default:
		return ExitOK
	}
}

func exitForStatus(status string) int {
	switch status {
	case api.StatusBlocked, api.StatusRejected:
		return ExitBlocked
	case api.StatusPendingHuman, api.StatusQuarantined:
		return ExitNeedsHuman
	default:
		return ExitOK
	}
}

// nextStep tells people what happens now, from the Worker's decision status
// (or, on a dry run, the local preview's action).
func nextStep(r proposeResult, dryRun bool) string {
	if dryRun {
		return fmt.Sprintf("Dry run: policy would choose %s. Run without --dry-run to record the decision.", r.Preview.Action)
	}
	switch r.Status {
	case api.StatusApproved:
		return "Policy allows this change to land: 'wayglass land'."
	case api.StatusPendingAgent:
		return "Waiting for a reviewer agent (not the author): 'wayglass review --for agent --json' shows it to them. Check approval with 'wayglass watch --json', then 'wayglass land'; do not rerun propose to poll."
	case api.StatusPendingHuman:
		if r.Decision != nil && r.Decision.ApprovalsNeeded > 1 {
			return fmt.Sprintf("Waiting for %d people to approve: 'wayglass review' shows it to them.", r.Decision.ApprovalsNeeded)
		}
		return "Waiting for a person to approve: 'wayglass review' shows it to them."
	case api.StatusQuarantined:
		return "Quarantined: it stays off the trunk until a person releases it with 'wayglass approve' or rejects it."
	case api.StatusBlocked:
		return "Blocked by policy. Change the code; the decision and its reason are in the audit log."
	case api.StatusRejected:
		return "Rejected. Change the code and propose again; the reason is in the audit log."
	}
	return "Decision recorded: " + r.Status + "."
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}
