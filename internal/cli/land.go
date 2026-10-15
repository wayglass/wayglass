package cli

import (
	"cmp"
	"context"
	"encoding/json"
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
	"github.com/wayglass/wayglass/internal/notes"
)

// Error codes for land.
const (
	CodeLandConflict = "land_conflict"
	CodeMissing      = "commit_missing"
	// CodeRepropose means the change that would land is no longer the one
	// policy decided on (it now touches other paths); propose it again.
	CodeRepropose = "repropose_required"
	// CodeTrunkMoved means the trunk changed between the land token and the
	// push; nothing landed and running land again is safe.
	CodeTrunkMoved = "trunk_moved"
	// CodeForkMoved means the intent's fork no longer holds the commit the
	// decision covers, so this checkout cannot get the approved change.
	CodeForkMoved = "fork_moved"
)

// Local refs land uses to look at the trunk without touching branches.
const (
	trunkRef      = "refs/wayglass/trunk"
	trunkNotesRef = "refs/notes/wayglass-trunk"
)

// landRef holds an intent's fork head when land had to fetch it.
func landRef(intentID string) string { return "refs/wayglass/land/" + intentID }

type landResult struct {
	IntentID   string `json:"intent_id"`
	DecisionID string `json:"decision_id"`
	Commit     string `json:"commit"`   // what the trunk now points at
	Approved   string `json:"approved"` // the fork head the decision covers
	// FetchedFork is true when this checkout lacked the approved commit and
	// land fetched it from the intent's fork with a read token.
	FetchedFork bool `json:"fetched_from_fork"`
	FastForward bool `json:"fast_forward"` // true when no merge commit was needed
	// TrunkMoved is true when other changes landed after this intent forked,
	// so the approved commit was merged onto the newer trunk.
	TrunkMoved bool `json:"trunk_moved"`
	// MergedPaths are decided paths the trunk had also changed; git merged
	// both edits without conflict. The Worker accepts that (same file set).
	MergedPaths []string `json:"merged_paths"`
	Branch      string   `json:"branch"`
	Note        bool     `json:"note_written"`
	Verified    bool     `json:"verified"` // the Worker checked the trunk holds exactly the approved change
	Mirror      string   `json:"mirror,omitempty"`
	Mirrored    bool     `json:"mirrored"`
	// LocalMoved is true only when land moved the local branch; LocalCurrent
	// is true when it already contained the landed commit.
	LocalMoved   bool `json:"local_fast_forwarded"`
	LocalCurrent bool `json:"local_up_to_date"`
	Aborted      bool `json:"land_aborted,omitempty"`
	// MergeUnsigned is true when commit.gpgsign is set but signing failed
	// (no key or agent reachable), so the merge commit was made unsigned.
	MergeUnsigned bool     `json:"merge_unsigned,omitempty"`
	Warns         []string `json:"warnings"`
}

func newLandCmd(a *app) *cobra.Command {
	var noMirror bool
	cmd := &cobra.Command{
		Use:   "land [intent-id]",
		Short: "Merge an approved change into the trunk, with its why, and mirror it",
		Long: "Asks the Worker for a 300-second trunk token (only for an approved decision\n" +
			"whose fork has not moved, and for a stacked intent only once its parent\n" +
			"landed), merges the approved commit into the current trunk in a temporary\n" +
			"worktree, pushes it with the why note, and has the Worker verify the trunk\n" +
			"now holds exactly that change. Then pushes the trunk branch and notes to\n" +
			"the mirror remote in .wayglass/config.yaml (e.g. GitHub) and fast-forwards\n" +
			"the local branch when it can. The token never touches disk.\n\n" +
			"Only the intent's author (its own agent key) or the admin key may land\n" +
			"it, but from any checkout of the project, not only the agent's: when the\n" +
			"approved commit is not here, land fetches the intent's fork with a\n" +
			"short-lived read token and refuses unless its head is exactly the commit\n" +
			"the decision covers.\n\n" +
			"When the trunk moved since the intent forked, the approved commit is merged,\n" +
			"never rebased, so the commit that lands is the one policy classified. Edits\n" +
			"that git merges cleanly in the decided files land; a conflict stops with\n" +
			"land_conflict, and a merge that would touch other files stops with\n" +
			"repropose_required. Either way nothing is pushed and the trunk token is\n" +
			"given back at once.\n\n" +
			"A decision holds only under the trunk policy it was made under: when the\n" +
			"trunk's .wayglass/policy.yaml changed since (or the Worker cannot tell),\n" +
			"land stops with policy_changed before any trunk token is issued. In the\n" +
			"intent's clone run wayglass sync (merges the trunk in), then push and\n" +
			"propose again to be decided under the current policy. The same three steps\n" +
			"resolve land_conflict and repropose_required.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			s, err := a.openSession(ctx, args)
			if err != nil {
				return err
			}
			res, err := s.land(ctx, noMirror)
			if res != nil {
				for _, m := range res.Warns {
					o.Warn("%s", m)
				}
			}
			if err != nil {
				return err
			}
			return o.Emit(res, func(out io.Writer) error { return renderLand(out, cliStyles(o), *res) })
		},
	}
	cmd.Flags().BoolVar(&noMirror, "no-mirror", false, "do not push to the mirror remote")
	return cmd
}

func (s *session) land(ctx context.Context, noMirror bool) (*landResult, error) {
	g := s.w.git
	lr, err := s.c.Land(ctx, s.w.cfg.Project, s.intent.ID)
	if api.IsCode(err, api.CodeNotAuthor) {
		return nil, errorf(api.CodeNotAuthor, ExitError,
			"only %s (who declared intent %s) or the admin key can land it; nothing was pushed. Ask %s to run: wayglass land %s",
			s.intent.Agent, s.intent.ID, s.intent.Agent, s.intent.ID)
	}
	if api.IsCode(err, api.CodePolicyChanged) {
		// The Worker's message names the next step; add the exact commands
		// only when it does not (a Worker from before wayglass sync).
		var ae *api.Error
		_ = errors.As(err, &ae)
		msg := strings.TrimSuffix(ae.Message, ".") + "; nothing was pushed"
		if !strings.Contains(msg, "wayglass sync") {
			msg += fmt.Sprintf(" (in the intent's clone: wayglass sync %s, then wayglass push %s, then wayglass propose %s)", s.intent.ID, s.intent.ID, s.intent.ID)
		}
		return nil, errorf(api.CodePolicyChanged, ExitError, "%s", msg)
	}
	if err != nil {
		return nil, err
	}
	res := &landResult{IntentID: s.intent.ID, DecisionID: lr.Decision.ID, Approved: lr.Fork.Commit, MergedPaths: []string{}, Warns: []string{}}
	tg := g
	tg.Token = lr.Token.Value // in memory only; expires in 300 s

	// Until the Worker has verified the trunk, any failure hands the land
	// back so the next agent does not wait for the token to expire.
	if err := s.landOnTrunk(ctx, tg, lr, res); err != nil {
		s.abortLand(ctx, res, err)
		return res, err
	}

	// The why travels with the commit: note the landed commit (and the
	// approved one, if a merge was needed), merged with the trunk's notes.
	approvals := s.approvals(ctx, lr.Decision, res)
	if err := writeLandNotes(ctx, g, s.intent, lr.Decision, approvals, res.Commit, res.Approved); err != nil {
		res.Warns = append(res.Warns, "writing the why note failed: "+err.Error())
	} else if err := tg.Push(ctx, lr.Trunk.Remote, notes.Refspec); err != nil {
		res.Warns = append(res.Warns, "pushing the why note to the trunk failed: "+err.Error())
	} else {
		res.Note = true
	}

	done, err := s.c.Landed(ctx, s.w.cfg.Project, s.intent.ID, res.Commit)
	if err != nil {
		return res, fmt.Errorf("trunk updated to %s but the Worker could not verify it: %w", short(res.Commit), err)
	}
	res.Verified = true
	if !done.TokenRevoked {
		res.Warns = append(res.Warns, "the Worker could not revoke the trunk token; it expires on its own within 300 s")
	}

	if mirror := s.w.cfg.Mirror; mirror != "" && !noMirror {
		res.Mirror = mirror
		mg := g // the mirror uses the user's own git credentials, never an Artifacts token
		if err := mg.Push(ctx, mirror, res.Commit+":refs/heads/"+res.Branch); err != nil {
			res.Warns = append(res.Warns, fmt.Sprintf("mirror %s rejected the push (it has commits the trunk lacks?): %v", mirror, err))
		} else {
			res.Mirrored = true
			if res.Note {
				if err := notes.Push(ctx, mg, mirror); err != nil {
					res.Warns = append(res.Warns, "pushing notes to the mirror failed: "+err.Error())
				}
			}
		}
	}

	switch fastForwardLocal(ctx, g, res.Branch, res.Commit) {
	case localMoved:
		res.LocalMoved = true
	case localCurrent:
		res.LocalCurrent = true
	default:
		res.Warns = append(res.Warns, fmt.Sprintf("local %s was not fast-forwarded to %s; run: git merge --ff-only %s", res.Branch, short(res.Commit), short(res.Commit)))
	}
	return res, nil
}

// landOnTrunk fetches the current trunk, puts the approved commit on top of
// it (fast-forward or merge) and pushes. It refuses, before pushing, anything
// the Worker would not verify as the approved change.
func (s *session) landOnTrunk(ctx context.Context, tg gitx.Git, lr *api.LandResult, res *landResult) error {
	g := s.w.git
	approved := res.Approved
	if err := s.fetchApproved(ctx, lr, res); err != nil {
		return err
	}
	branch, _ := tg.RemoteDefaultBranch(ctx, lr.Trunk.Remote)
	res.Branch = cmp.Or(branch, "main")
	if err := tg.Fetch(ctx, lr.Trunk.Remote, "+refs/heads/"+res.Branch+":"+trunkRef); err != nil {
		return err
	}
	// The trunk may have no notes yet; that is fine.
	_ = tg.Fetch(ctx, lr.Trunk.Remote, "+"+notes.Ref+":"+trunkNotesRef)

	trunkHead, _, err := g.Resolve(ctx, trunkRef)
	if err != nil {
		return err
	}
	if lr.TrunkBefore != nil && *lr.TrunkBefore != trunkHead {
		return errorf(CodeTrunkMoved, ExitError, "the trunk moved from %s to %s after the land began; nothing was pushed, run land again",
			short(*lr.TrunkBefore), short(trunkHead))
	}

	res.Commit = approved
	if isAncestor(ctx, g, trunkHead, approved) {
		res.FastForward = true
		// The Worker checks a fast-forward the same way as a merge: what the
		// trunk gains must be inside the decision. A truncated decision has
		// partial paths; a person approved it as too large to list.
		landing, err := g.ChangedFiles(ctx, trunkHead, approved)
		if err != nil {
			return err
		}
		if extra := outside(landing, lr.Decision.Paths); len(extra) > 0 && len(lr.Decision.Detail.Truncated) == 0 {
			return errorf(CodeRepropose, ExitError,
				"fast-forwarding the trunk to the approved commit also changes %s, which decision %s did not cover; nothing was pushed. In the intent's clone run wayglass push and wayglass propose again",
				strings.Join(extra, ", "), lr.Decision.ID)
		}
	} else {
		res.TrunkMoved = true
		mine, err := g.ChangedFiles(ctx, trunkHead, approved)
		if err != nil {
			return err
		}
		theirs, err := g.ChangedFiles(ctx, approved, trunkHead)
		if err != nil {
			return err
		}
		res.MergedPaths = intersectSorted(mine, theirs)
		var unsigned bool
		res.Commit, unsigned, err = mergeInWorktree(ctx, g, s.intent.ID, trunkHead, approved, landMessage(s.intent, lr.Decision))
		if err != nil {
			return err
		}
		if unsigned {
			res.MergeUnsigned = true
			res.Warns = append(res.Warns, "your git config asks to sign commits but signing failed here, so the land merge "+short(res.Commit)+" is unsigned; the approved commit it merges is unchanged")
		}
		landing, err := g.ChangedFiles(ctx, trunkHead, res.Commit)
		if err != nil {
			return err
		}
		if extra := outside(landing, lr.Decision.Paths); len(extra) > 0 {
			return errorf(CodeRepropose, ExitError,
				"merged onto the current trunk, the change also touches %s, which decision %s did not cover; nothing was pushed. In the intent's clone run wayglass sync %s, then wayglass push and wayglass propose again",
				strings.Join(extra, ", "), lr.Decision.ID, s.intent.ID)
		}
	}
	if err := tg.Push(ctx, lr.Trunk.Remote, res.Commit+":refs/heads/"+res.Branch); err != nil {
		return errorf(CodeTrunkMoved, ExitError, "pushing to the trunk failed, so nothing landed (did the trunk move? run land again): %v", err)
	}
	return nil
}

// fetchApproved makes sure the approved commit is in this checkout. A
// checkout other than the author's (a fresh trunk clone, the approver's)
// fetches the intent's fork with a short-lived read token, held in memory
// only, and accepts it only if the fork head is exactly the commit the
// decision covers: land never merges anything policy did not see.
func (s *session) fetchApproved(ctx context.Context, lr *api.LandResult, res *landResult) error {
	g := s.w.git
	approved := res.Approved
	if _, ok, err := g.Resolve(ctx, approved+"^{commit}"); err == nil && ok {
		return nil
	}
	rt, err := s.c.ReadToken(ctx, s.w.cfg.Project, s.intent.ID)
	if err != nil {
		return errorf(CodeMissing, ExitError, "approved commit %s is not in this checkout and the intent's fork could not be read: %v", short(approved), err)
	}
	remote := cmp.Or(lr.Fork.Remote, rt.Fork.Remote, s.intent.ForkRemote)
	fg := g
	fg.Token = rt.Token.Value // read-only, in memory only
	branch, _ := fg.RemoteDefaultBranch(ctx, remote)
	ref := landRef(s.intent.ID)
	if err := fg.Fetch(ctx, remote, "+refs/heads/"+cmp.Or(branch, "main")+":"+ref); err != nil {
		return errorf(CodeMissing, ExitError, "approved commit %s is not in this checkout and fetching the intent's fork failed: %v", short(approved), err)
	}
	head, ok, err := g.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	if !ok || head != approved {
		return errorf(CodeForkMoved, ExitError, "the intent's fork head is %s, but decision %s covers %s; nothing was pushed, propose again",
			cmp.Or(short(head), "(empty)"), lr.Decision.ID, short(approved))
	}
	res.FetchedFork = true
	return nil
}

// abortLand hands an unfinished land back to the Worker: it revokes the trunk
// token and frees the land slot. Workers without the route let the token
// expire instead (300 s), which blocks other lands until then.
func (s *session) abortLand(ctx context.Context, res *landResult, cause error) {
	ctx = context.WithoutCancel(ctx)
	reason := cause.Error()
	if len(reason) > 400 {
		reason = reason[:400]
	}
	_, err := s.c.AbortLand(ctx, s.w.cfg.Project, s.intent.ID, reason)
	switch {
	case err == nil:
		res.Aborted = true
	case api.IsCode(err, api.CodeNotFound) || api.IsCode(err, api.CodeMethodNotAllowed):
		res.Warns = append(res.Warns, "this Worker cannot abort a land; other lands wait until the trunk token expires (300 s)")
	default:
		res.Warns = append(res.Warns, "giving the land back failed; other lands wait until the trunk token expires (300 s): "+err.Error())
	}
}

// approvals fetches the verdicts and reasons behind a decision so the why
// note carries the approver's reason, not only their name. Policy-only
// decisions have none.
func (s *session) approvals(ctx context.Context, d api.Decision, res *landResult) []notes.Approval {
	if cmp.Or(deref(d.DecidedBy), "policy") == "policy" {
		return nil
	}
	wr, err := s.c.Why(ctx, s.w.cfg.Project, res.Approved)
	if err != nil || wr.Decision.ID != d.ID {
		res.Warns = append(res.Warns, "could not read the approvals for the why note; wayglass why will ask the Worker")
		return nil
	}
	return toNoteApprovals(wr.Approvals)
}

// deciderSource is how the person or agent who decided signed in: the
// source of their last verdict ("" when policy decided or it was not recorded).
func deciderSource(by string, approvals []notes.Approval) string {
	for i := len(approvals) - 1; i >= 0; i-- {
		if approvals[i].By == by {
			return approvals[i].IdentitySource
		}
	}
	return ""
}

func toNoteApprovals(in []api.Approval) []notes.Approval {
	var out []notes.Approval
	for _, a := range in {
		out = append(out, notes.Approval{By: a.By, ClaimedBy: a.ClaimedBy, Verdict: a.Verdict, Reason: a.Reason, At: a.At, IdentitySource: a.IdentitySource})
	}
	return out
}

// outside returns the paths in got that are not in allowed. An empty allowed
// list (a Worker that does not report decision paths) checks nothing; the
// Worker still verifies the landed commit itself.
func outside(got, allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	var extra []string
	for _, p := range got {
		if !slices.Contains(allowed, p) {
			extra = append(extra, p)
		}
	}
	return extra
}

func intersectSorted(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func isAncestor(ctx context.Context, g gitx.Git, a, b string) bool {
	_, err := g.Run(ctx, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// mergeInWorktree merges approved into trunkHead in a throwaway worktree, so
// the user's checkout and branches are never touched, and returns the merge.
// A conflict stops with land_conflict and nothing is pushed.
func mergeInWorktree(ctx context.Context, g gitx.Git, intentID, trunkHead, approved, message string) (sha string, unsigned bool, err error) {
	sha, unsigned, conflicts, err := mergeCommit(ctx, g, trunkHead, approved, message)
	if errors.Is(err, errMergeConflict) {
		if len(conflicts) == 0 {
			return "", false, errorf(CodeLandConflict, ExitError, "the approved change does not merge into the trunk; nothing was pushed (%v)", err)
		}
		return "", false, errorf(CodeLandConflict, ExitError,
			"the approved change conflicts with the trunk in %s; nothing was pushed. In the intent's clone run wayglass sync %s, resolve the conflicts it lists, then wayglass push and wayglass propose again",
			strings.Join(conflicts, ", "), intentID)
	}
	return sha, unsigned, err
}

// errMergeConflict means git could not merge the two commits; mergeCommit
// lists the conflicting files when git names them.
var errMergeConflict = errors.New("merge conflict")

// mergeCommit merges theirs into ours (ours is the first parent) in a
// throwaway worktree and returns the merge commit, never touching the
// user's checkout or branches.
//
// The merge is signed when the user's git config asks for it and signing
// works. When it asks but signing fails (an agent with no key, gpg-agent or
// ssh-agent reachable; seen in the dress rehearsal), the merge is made again
// with commit.gpgsign=false for this one command and reported as unsigned:
// the merge only joins two commits git already holds, both keep whatever
// signature they had, and the Worker classifies or verifies the result.
func mergeCommit(ctx context.Context, g gitx.Git, ours, theirs, message string) (sha string, unsigned bool, conflicts []string, err error) {
	dir, err := os.MkdirTemp("", "wayglass-merge-")
	if err != nil {
		return "", false, nil, err
	}
	defer os.RemoveAll(dir)
	if _, err := g.Run(ctx, "worktree", "add", "--quiet", "--detach", dir, ours); err != nil {
		return "", false, nil, err
	}
	defer func() { _, _ = g.Run(context.WithoutCancel(ctx), "worktree", "remove", "--force", dir) }()
	wt := g
	wt.Dir = dir
	unsigned, err = mergeSigned(ctx, wt, "--no-ff", "--no-edit", "-m", message, theirs)
	if err != nil {
		out, _ := wt.Run(ctx, "diff", "--name-only", "--diff-filter=U")
		_, _ = wt.Run(ctx, "merge", "--abort")
		return "", false, strings.Fields(out), fmt.Errorf("%w: %w", errMergeConflict, err)
	}
	sha, err = wt.Head(ctx)
	return sha, unsigned, nil, err
}

// mergeSigned runs git merge with args in wt. When the user's config asks to
// sign commits but signing fails, it aborts and merges again with
// commit.gpgsign=false for this one command, reporting unsigned. A conflict
// is left in place; the caller lists or aborts it.
func mergeSigned(ctx context.Context, wt gitx.Git, args ...string) (unsigned bool, err error) {
	merge := append([]string{"merge"}, args...)
	_, err = wt.Run(ctx, merge...)
	if err != nil && signingFailed(err) {
		_, _ = wt.Run(ctx, "merge", "--abort")
		unsigned = true
		_, err = wt.Run(ctx, append([]string{"-c", "commit.gpgsign=false"}, merge...)...)
	}
	return unsigned, err
}

// signingFailed reports whether a merge failed only because git could not
// write the (signed) commit: the tree merged, the commit object did not.
func signingFailed(err error) bool {
	var ge *gitx.Error
	if !errors.As(err, &ge) {
		return false
	}
	return strings.Contains(ge.Stderr, "failed to sign") || strings.Contains(ge.Stderr, "failed to write commit object")
}

func landMessage(it *api.Intent, d api.Decision) string {
	return fmt.Sprintf("Land %s: %s\n\nWhy: %s\nDecision %s: %s (%s)\n\nWayglass-Intent: %s",
		it.ID, it.Goal, it.Reason, d.ID, d.Action, d.Reason, it.ID)
}

// writeLandNotes merges the trunk's notes into the local notes ref, then
// notes each commit, so pushing the ref never drops another intent's note.
func writeLandNotes(ctx context.Context, g gitx.Git, it *api.Intent, d api.Decision, approvals []notes.Approval, commits ...string) error {
	if trunkNotes, ok, _ := g.Resolve(ctx, trunkNotesRef); ok {
		if have, _ := notes.Exists(ctx, g); !have {
			if _, err := g.Run(ctx, "update-ref", notes.Ref, trunkNotes); err != nil {
				return err
			}
		} else if _, err := g.Run(ctx, "notes", "--ref="+notes.Ref, "merge", "--strategy=theirs", "--quiet", trunkNotesRef); err != nil {
			return fmt.Errorf("merging the trunk's notes: %w", err)
		}
	}
	var tags classify.Tags
	_ = json.Unmarshal(d.Tags, &tags)
	n := notes.Note{
		IntentID: it.ID, Goal: it.Goal, Reason: it.Reason, Tags: tags,
		Rule: deref(d.MatchedRule), Action: d.Action, DecisionID: d.ID, Status: d.Status,
		DecidedBy: cmp.Or(deref(d.DecidedBy), "policy"), DecidedAt: parseTime(deref(d.DecidedAt)),
		Approvals: approvals,
	}
	n.IdentitySource = deciderSource(n.DecidedBy, approvals)
	if n.DecidedAt.IsZero() {
		n.DecidedAt = time.Now().UTC()
	}
	var errs []error
	seen := map[string]bool{}
	for _, c := range commits {
		if !seen[c] {
			seen[c] = true
			errs = append(errs, notes.Write(ctx, g, c, n))
		}
	}
	return errors.Join(errs...)
}

type localMove int

const (
	localStale   localMove = iota // left alone: diverged, dirty or another branch is checked out
	localMoved                    // fast-forwarded to the landed commit
	localCurrent                  // already contained the landed commit
)

// fastForwardLocal moves the local branch to landed when that loses nothing:
// the checkout is on branch, clean, and HEAD is an ancestor of landed. If the
// branch is not checked out, it is moved only when it is an ancestor. A
// branch that already contains landed is reported as current, not moved.
func fastForwardLocal(ctx context.Context, g gitx.Git, branch, landed string) localMove {
	tip, ok, err := g.Resolve(ctx, "refs/heads/"+branch)
	if err != nil || !ok {
		return localStale
	}
	if isAncestor(ctx, g, landed, tip) {
		return localCurrent
	}
	if !isAncestor(ctx, g, tip, landed) {
		return localStale
	}
	cur, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil && strings.TrimSpace(cur) == branch {
		if dirty, err := g.Dirty(ctx); err != nil || dirty {
			return localStale
		}
		if _, err := g.Run(ctx, "merge", "--ff-only", "--quiet", landed); err != nil {
			return localStale
		}
		return localMoved
	}
	if _, err := g.Run(ctx, "update-ref", "refs/heads/"+branch, landed, tip); err != nil {
		return localStale
	}
	return localMoved
}

func renderLand(out io.Writer, st styles, r landResult) error {
	how := "merge"
	if r.FastForward {
		how = "fast-forward"
	}
	_, _ = lipgloss.Fprintf(out, "%s %s on trunk %s at %s (%s)\n", st.good.Render("Landed"), st.id(r.IntentID), r.Branch, st.id(short(r.Commit)), how)
	if r.FetchedFork {
		_, _ = lipgloss.Fprintf(out, "  Fetched approved commit %s from the intent's fork (read token).\n", st.id(short(r.Approved)))
	}
	if r.TrunkMoved {
		_, _ = lipgloss.Fprintf(out, "  The trunk had moved; merged approved commit %s onto it unchanged.\n", st.id(short(r.Approved)))
		if len(r.MergedPaths) > 0 {
			_, _ = lipgloss.Fprintf(out, "  Both sides edited %s; git merged them cleanly.\n", strings.Join(r.MergedPaths, ", "))
		}
	}
	if r.Verified {
		_, _ = lipgloss.Fprintln(out, "  Worker verified the trunk holds exactly the approved change; trunk token revoked.")
	}
	if r.Note {
		_, _ = lipgloss.Fprintln(out, "  Why note pushed with it: wayglass why <file>:<line> answers from git.")
	}
	if r.Mirrored {
		_, _ = lipgloss.Fprintf(out, "  Mirrored to %s.\n", r.Mirror)
	}
	switch {
	case r.LocalMoved:
		_, _ = lipgloss.Fprintf(out, "  Local %s fast-forwarded.\n", r.Branch)
	case r.LocalCurrent:
		_, _ = lipgloss.Fprintf(out, "  Local %s already had it.\n", r.Branch)
	}
	return nil
}
