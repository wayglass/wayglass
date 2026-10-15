package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

// CodeRestackConflict means the intent's commits do not merge with the new
// parent's head. Before the Worker was asked, nothing changed; after (the
// parent moved in between), the message says how to finish by hand.
const CodeRestackConflict = "restack_conflict"

// Local refs restack uses: the intent's own commits when this checkout did
// not have them, and the new parent's head it merges onto.
func restackHeadRef(intentID string) string { return "refs/wayglass/restack/" + intentID + "/head" }
func restackOntoRef(intentID string) string { return "refs/wayglass/restack/" + intentID + "/onto" }

type restackResult struct {
	IntentID string     `json:"intent_id"`
	On       string     `json:"on"`
	Intent   api.Intent `json:"intent"`
	// Previous is where the intent was; its old fork is kept.
	Previous api.RestackPrevious `json:"previous"`
	// Base is the new parent's head: the new diff base.
	Base string `json:"base"`
	// Carried is the intent's own head that was merged onto Base, "" when it
	// had nothing of its own yet.
	Carried string `json:"carried"`
	// Commit is the new fork's head: a merge of Carried and Base, or Base.
	Commit string   `json:"commit"`
	Merged bool     `json:"merged"`
	Pushed bool     `json:"pushed"`
	Files  []string `json:"files"`
	// SupersededDecision no longer covers the change; land refuses it.
	SupersededDecision *string `json:"superseded_decision"`
	MustPropose        bool    `json:"must_propose"`
	// FetchedHead is true when this checkout lacked the intent's commits and
	// fetched them from its old fork with a read token.
	FetchedHead   bool         `json:"fetched_head"`
	LocalMoved    bool         `json:"local_fast_forwarded"`
	MergeUnsigned bool         `json:"merge_unsigned,omitempty"`
	Overlaps      []overlapOut `json:"overlaps"`
	Warnings      []string     `json:"warnings"`
}

func newIntentRestackCmd(a *app) *cobra.Command {
	var on, reason string
	cmd := &cobra.Command{
		Use:   "restack <intent-id> --on <other-intent-id>",
		Short: "Stack an intent on another unlanded intent (the overlap re-plan 'stack')",
		Long: "Moves an open or proposed intent onto another unlanded intent in one step,\n" +
			"instead of declaring again with --on and abandoning the old intent. Only\n" +
			"its author (the agent a bound key names, or the name it declared with) or\n" +
			"the admin may. The reason goes to the audit log.\n\n" +
			"The Worker forks the other intent's fork, as declare --on does, and the\n" +
			"intent diffs from that intent's head from then on: it is classified on its\n" +
			"own change only and lands after the other one. This command merges the\n" +
			"intent's pushed commits (in its own checkout: HEAD) onto that head in a\n" +
			"temporary worktree, never rebasing, and pushes the merge to the new fork.\n" +
			"It checks the merge before asking the Worker, so a conflict changes\n" +
			"nothing. The old fork is kept; its tokens are revoked.\n\n" +
			"A decision made before the restack covered another base: land refuses it\n" +
			"until you run 'wayglass propose' again. Refuses cycles, landed or abandoned\n" +
			"parents, and rubber-stamp reasons.",
		Example: `  wayglass intent restack i9876543210 --on i0123456789 \
    --reason "Both edit web/login.ts; build on the rate limit instead of racing it" --json`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason = strings.TrimSpace(reason)
			on = strings.TrimSpace(on)
			switch {
			case on == "":
				return UsageError("--on is required: the intent to stack on")
			case on == args[0]:
				return UsageError("an intent cannot be stacked on itself")
			case reason == "":
				return UsageError("--reason is required: the audit log keeps why the plan changed")
			case placeholderReason.MatchString(reason):
				return UsageError("--reason %q looks like a placeholder; say why the intent is re-stacked", reason)
			case fillerReason(reason):
				return UsageError("--reason %q is a rubber stamp, not a why; say why the intent is re-stacked", reason)
			}
			ctx := cmd.Context()
			o := a.output(cmd)
			s, err := a.openSession(ctx, args)
			if err != nil {
				return err
			}
			res, err := s.restack(ctx, on, reason)
			if res != nil {
				for _, m := range res.Warnings {
					o.Warn("%s", m)
				}
			}
			if err != nil {
				return err
			}
			for _, ov := range res.Overlaps {
				warnOverlaps(o, res.IntentID, []api.Overlap{ov.Overlap})
				if ov.Replan != nil {
					o.Warn("suggested re-plan: %s", ov.Replan.Summary)
				}
			}
			return o.Emit(res, func(out io.Writer) error { return renderRestack(out, *res, reason) })
		},
	}
	cmd.Flags().StringVar(&on, "on", "", "the unlanded intent to stack on (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why the intent is re-stacked (required)")
	return cmd
}

func (s *session) restack(ctx context.Context, on, reason string) (*restackResult, error) {
	g := s.w.git
	it := s.intent
	if !slices.Contains(activeStatuses, it.Status) {
		return nil, errorf(api.CodeConflict, ExitError, "intent %s is %s; only an open or proposed intent can be re-stacked", it.ID, it.Status)
	}
	res := &restackResult{IntentID: it.ID, On: on, Files: []string{}, Overlaps: []overlapOut{}, Warnings: []string{}}

	carried, err := s.carriedHead(ctx, res)
	if err != nil {
		return res, err
	}
	res.Carried = carried

	// Check the merge against the parent's head before anything changes.
	rt, err := s.c.ReadToken(ctx, s.w.cfg.Project, on)
	if err != nil {
		return res, err
	}
	onto, err := fetchHead(ctx, s.w, rt.Fork.Remote, rt.Token.Value, restackOntoRef(it.ID))
	if err != nil {
		return res, fmt.Errorf("fetching %s's fork: %w", on, err)
	}
	message := fmt.Sprintf("Restack %s onto %s: %s\n\nWhy: %s\n\nWayglass-Intent: %s", it.ID, on, it.Goal, reason, it.ID)
	var preMerged string
	if onto != "" && needsMerge(ctx, s, carried, onto) {
		sha, unsigned, conflicts, err := mergeCommit(ctx, g, carried, onto, message)
		if errors.Is(err, errMergeConflict) {
			return res, errorf(CodeRestackConflict, ExitError,
				"%s's commits conflict with %s's head %s in %s; nothing changed. To stack anyway: git merge %s, resolve, wayglass push, then restack again",
				it.ID, on, short(onto), cmp.Or(strings.Join(conflicts, ", "), "the merge"), restackOntoRef(it.ID))
		}
		if err != nil {
			return res, err
		}
		preMerged, res.MergeUnsigned = sha, unsigned
	}

	rr, err := s.c.RestackIntent(ctx, s.w.cfg.Project, it.ID, on, reason)
	if api.IsCode(err, api.CodeMethodNotAllowed) || isMissingRoute(err) {
		return res, errorf(CodeStackUnsupported, ExitError, "this Worker has no restack route; declare a new intent with --on %s and abandon %s instead", on, it.ID)
	}
	if err != nil {
		return res, err
	}
	res.Intent, res.Previous, res.SupersededDecision = rr.Intent, rr.Previous, rr.SupersededDecision
	res.MustPropose = it.Status == "proposed" || rr.SupersededDecision != nil
	res.Base = deref(rr.Intent.BaseCommit)
	branch := cmp.Or(rr.Fork.DefaultBranch, "main")
	tg := g
	tg.Token = rr.Token.Value // in memory only

	// The Worker read the parent's head itself; it moved only if someone
	// pushed in between. Fetch the new fork when this checkout lacks it.
	if _, ok, _ := g.Resolve(ctx, res.Base+"^{commit}"); res.Base != "" && !ok {
		if _, err := fetchHead(ctx, s.w, rr.Fork.Remote, rr.Token.Value, baseRef(it.ID)); err != nil {
			return res, s.restackUnfinished(rr, fmt.Errorf("fetching the new fork: %w", err))
		}
	}
	if res.Base != "" {
		if _, err := g.Run(ctx, "update-ref", baseRef(it.ID), res.Base); err != nil {
			return res, err
		}
	}

	res.Commit = res.Base
	switch {
	case !needsMerge(ctx, s, carried, res.Base):
		// Nothing of its own beyond the parent's head.
	case res.Base == onto && preMerged != "":
		res.Commit, res.Merged = preMerged, true
	default:
		sha, unsigned, conflicts, err := mergeCommit(ctx, g, carried, res.Base, message)
		if errors.Is(err, errMergeConflict) {
			return res, errorf(CodeRestackConflict, ExitError,
				"%s moved during the restack and %s's commits conflict with its head %s in %s. %s is now stacked on %s in %s; finish by hand: git merge %s, resolve, wayglass push %s",
				on, it.ID, short(res.Base), cmp.Or(strings.Join(conflicts, ", "), "the merge"), it.ID, on, rr.Fork.Name, baseRef(it.ID), it.ID)
		}
		if err != nil {
			return res, s.restackUnfinished(rr, err)
		}
		res.Commit, res.Merged, res.MergeUnsigned = sha, true, unsigned
	}

	if res.Commit != "" && res.Commit != res.Base {
		if err := tg.Push(ctx, rr.Fork.Remote, res.Commit+":refs/heads/"+branch); err != nil {
			return res, s.restackUnfinished(rr, err)
		}
		res.Pushed = true
		files, err := g.ChangedFiles(ctx, res.Base, res.Commit)
		if err != nil {
			return res, err
		}
		if len(files) > maxReportedPaths {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%d files changed; the Worker accepts at most %d per report, so overlaps were not re-checked", len(files), maxReportedPaths))
		} else {
			res.Files = append(res.Files, files...)
			rep, err := s.c.ReportPush(ctx, it.ID, api.PushInput{Project: s.w.cfg.Project, Paths: files, Commit: res.Commit})
			if err != nil {
				res.Warnings = append(res.Warnings, "pushed, but reporting the push to the Worker failed: "+err.Error())
			} else if rep.Intent.ID != "" {
				res.Intent = rep.Intent
				rr.Overlaps = rep.Overlaps
			}
		}
	}
	res.Overlaps, err = withReplans(ctx, s.c, s.w.cfg.Project, res.Intent, rr.Overlaps)
	if err != nil {
		res.Warnings = append(res.Warnings, "could not read intents to suggest re-plans: "+err.Error())
	}
	if res.Overlaps == nil {
		res.Overlaps = []overlapOut{}
	}

	// In the intent's own checkout, move the branch to the new head when
	// that loses nothing.
	if s.w.currentIntent() == it.ID && res.Commit != "" {
		cur, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
		switch {
		case err != nil:
			res.Warnings = append(res.Warnings, fmt.Sprintf("HEAD is detached; run: git merge --ff-only %s", short(res.Commit)))
		default:
			switch fastForwardLocal(ctx, g, strings.TrimSpace(cur), res.Commit) {
			case localMoved:
				res.LocalMoved = true
			case localCurrent:
			default:
				res.Warnings = append(res.Warnings, fmt.Sprintf("this checkout was not fast-forwarded to %s (uncommitted changes?); run: git merge --ff-only %s", short(res.Commit), short(res.Commit)))
			}
		}
	}
	return res, nil
}

// carriedHead is the intent's own head to take along: HEAD in the intent's
// own checkout (what push would push), else the head it last pushed,
// fetched from its fork when this checkout lacks it. "" when it has nothing.
func (s *session) carriedHead(ctx context.Context, res *restackResult) (string, error) {
	g := s.w.git
	it := s.intent
	pushed := it.Head()
	if s.w.currentIntent() == it.ID {
		head, err := g.Head(ctx)
		if err != nil {
			return "", err
		}
		if pushed != "" {
			if _, ok, _ := g.Resolve(ctx, pushed+"^{commit}"); !ok || !isAncestor(ctx, g, pushed, head) {
				return "", errorf(api.CodeConflict, ExitError,
					"HEAD %s does not contain %s's pushed head %s; get it into this checkout (git pull from its fork, or a fresh clone) before restacking",
					short(head), it.ID, short(pushed))
			}
		}
		if dirty, err := g.Dirty(ctx); err == nil && dirty {
			res.Warnings = append(res.Warnings, "uncommitted changes stay behind; commit them and run wayglass push after the restack")
		}
		return head, nil
	}
	if pushed == "" {
		return "", nil
	}
	if _, ok, _ := g.Resolve(ctx, pushed+"^{commit}"); ok {
		return pushed, nil
	}
	rt, err := s.c.ReadToken(ctx, s.w.cfg.Project, it.ID)
	if err != nil {
		return "", errorf(CodeMissing, ExitError, "%s's commits are not in this checkout and its fork could not be read: %v", it.ID, err)
	}
	head, err := fetchHead(ctx, s.w, cmp.Or(rt.Fork.Remote, it.ForkRemote), rt.Token.Value, restackHeadRef(it.ID))
	if err != nil {
		return "", errorf(CodeMissing, ExitError, "%s's commits are not in this checkout and fetching its fork failed: %v", it.ID, err)
	}
	res.FetchedHead = true
	return head, nil
}

// needsMerge reports whether carried has commits beyond onto.
func needsMerge(ctx context.Context, s *session, carried, onto string) bool {
	if carried == "" || carried == onto {
		return false
	}
	return onto == "" || !isAncestor(ctx, s.w.git, carried, onto)
}

// fetchHead fetches a fork's default branch into ref with a token held in
// memory only and returns its head ("" for an empty fork).
func fetchHead(ctx context.Context, w *workspace, remote, token, ref string) (string, error) {
	fg := w.git
	fg.Token = token
	branch, _ := fg.RemoteDefaultBranch(ctx, remote)
	if err := fg.Fetch(ctx, remote, "+refs/heads/"+cmp.Or(branch, "main")+":"+ref); err != nil {
		return "", err
	}
	head, _, err := w.git.Resolve(ctx, ref)
	return head, err
}

// restackUnfinished explains a failure after the Worker moved the intent:
// it now lives in the new fork, which holds only the parent's head.
func (s *session) restackUnfinished(rr *api.RestackResult, cause error) error {
	return errorf(CodeError, ExitError,
		"%s is now stacked on %s in %s, but its commits did not reach the new fork (%v); in its checkout run: git merge %s, then wayglass push %s",
		s.intent.ID, deref(rr.Intent.ParentID), rr.Fork.Name, cause, baseRef(s.intent.ID), s.intent.ID)
}

func renderRestack(out io.Writer, r restackResult, reason string) error {
	fmt.Fprintf(out, "Restacked %s on %s at %s.\n", r.IntentID, r.On, short(r.Base))
	fmt.Fprintf(out, "Why:      %s\n", reason)
	switch {
	case r.Merged:
		fmt.Fprintf(out, "Carried:  %s merged onto it as %s; %d file(s) changed\n", short(r.Carried), short(r.Commit), len(r.Files))
	case r.Carried != "":
		fmt.Fprintf(out, "Carried:  nothing beyond %s's head\n", r.On)
	default:
		fmt.Fprintln(out, "Carried:  nothing pushed yet")
	}
	fmt.Fprintf(out, "Fork:     %s (old fork %s kept; %d token(s) revoked)\n", r.Intent.ForkRepo, r.Previous.ForkRepo, r.Previous.TokensRevoked)
	if r.LocalMoved {
		fmt.Fprintf(out, "Checkout fast-forwarded to %s.\n", short(r.Commit))
	}
	next := fmt.Sprintf("it lands after %s.", r.On)
	if r.MustPropose {
		next = fmt.Sprintf("run 'wayglass propose %s' again (the earlier decision covered another base); %s", r.IntentID, next)
	} else {
		next = "commit, push and propose as usual; " + next
	}
	_, err := fmt.Fprintf(out, "Next:     %s\n", next)
	return err
}
