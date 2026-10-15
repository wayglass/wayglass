package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/notes"
)

// Error codes for sync.
const (
	// CodeSyncConflict means the trunk did not merge cleanly into the
	// intent; the merge is left in the checkout for the agent to resolve.
	CodeSyncConflict = "sync_conflict"
	// CodeNotForkClone means the checkout is not the intent's own clone, so
	// merging the trunk here would not reach the intent's fork.
	CodeNotForkClone = "not_fork_clone"
	// CodeDirty means tracked files have uncommitted changes.
	CodeDirty = "dirty"
)

type syncResult struct {
	IntentID string `json:"intent_id"`
	Branch   string `json:"branch"` // the trunk branch that was fetched
	// TrunkHead is empty when the trunk has no commits yet.
	TrunkHead string `json:"trunk_head"`
	// Head is HEAD after the sync (before it, on a conflict).
	Head   string `json:"head"`
	Merged bool   `json:"merged"`
	// UpToDate is true when HEAD already contained the trunk head.
	UpToDate     bool     `json:"up_to_date"`
	FastForward  bool     `json:"fast_forward"`
	Conflicts    []string `json:"conflicts"`
	NotesFetched bool     `json:"notes_fetched"`
	// MergeUnsigned is true when commit.gpgsign is set but signing failed,
	// so the merge commit was made unsigned.
	MergeUnsigned bool     `json:"merge_unsigned,omitempty"`
	NoTokenOnDisk bool     `json:"no_token_on_disk"`
	Next          string   `json:"next"`
	Warnings      []string `json:"warnings"`
}

func newSyncCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "sync [intent-id]",
		Short: "Merge the current trunk into the intent's checkout, before pushing and proposing again",
		Long: "Run in the intent's own clone (the checkout that declared it, or one whose\n" +
			"HEAD contains the intent's pushed head). Asks the Worker for a 60-second\n" +
			"read-only trunk token, fetches the trunk head into " + trunkRef + " and\n" +
			"the trunk's why-notes into " + trunkNotesRef + ", then merges the trunk into\n" +
			"the current branch. It merges, never rebases, so commits already pushed to\n" +
			"the fork keep their ids. The token reaches git through its environment,\n" +
			"never argv, a URL or disk, and sync checks .git afterwards.\n\n" +
			"This is the recovery when land refuses with policy_changed (the trunk's\n" +
			"policy moved after the decision) or land_conflict (the change no longer\n" +
			"merges into the trunk). A conflict stops with sync_conflict, lists the\n" +
			"files and leaves the merge in progress: resolve them, git add, git commit\n" +
			"--no-edit. Then run wayglass push and wayglass propose, so the change is\n" +
			"decided again against the current trunk and its policy. The proposal\n" +
			"covers only what landing would add to the trunk: merged trunk files do\n" +
			"not count, edits made while resolving the merge do.\n\n" +
			"A stacked intent takes the trunk through its parent: sync refuses until\n" +
			"the parent has landed.",
		Example: "  wayglass sync\n  wayglass sync i83f41dd279 --json",
		Args:    usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			s, err := a.openSession(ctx, args)
			if err != nil {
				return err
			}
			res, err := s.sync(ctx)
			if res != nil {
				for _, m := range res.Warnings {
					o.Warn("%s", m)
				}
			}
			if err != nil {
				return err
			}
			return o.Emit(res, func(out io.Writer) error { return renderSync(out, cliStyles(o), *res) })
		},
	}
}

func (s *session) sync(ctx context.Context) (*syncResult, error) {
	g := s.w.git
	it := s.intent
	res := &syncResult{IntentID: it.ID, Conflicts: []string{}, Warnings: []string{}}
	next := fmt.Sprintf("wayglass push %s, then wayglass propose %s", it.ID, it.ID)

	if !slices.Contains(activeStatuses, it.Status) {
		return nil, errorf(api.CodeConflict, ExitError, "intent %s is %s; there is nothing to sync", it.ID, it.Status)
	}
	head, err := g.Head(ctx)
	if err != nil {
		return nil, err
	}
	res.Head = head
	if err := s.checkForkClone(ctx, head); err != nil {
		return nil, err
	}
	if pid := deref(it.ParentID); pid != "" {
		parent, err := s.c.GetIntent(ctx, s.w.cfg.Project, pid)
		if err != nil {
			return nil, err
		}
		if parent.Status != "landed" {
			return nil, errorf(api.CodeConflict, ExitError,
				"intent %s is stacked on %s, which is %s; it gets the trunk through %s. Run wayglass sync in %s's checkout, or restack %s",
				it.ID, pid, parent.Status, pid, pid, it.ID)
		}
	}
	if out, err := g.Run(ctx, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return nil, err
	} else if out != "" {
		return nil, errorf(CodeDirty, ExitError, "tracked files have uncommitted changes; commit or stash them, then run wayglass sync again")
	}

	tok, err := s.c.TrunkReadToken(ctx, s.w.cfg.Project)
	if err != nil {
		return nil, err
	}
	tg := g
	tg.Token = tok.Token.Value // read-only, in memory only; expires in about a minute
	remote := tok.Trunk.Remote
	branch, _ := tg.RemoteDefaultBranch(ctx, remote)
	res.Branch = cmp.Or(branch, "main")
	fetchErr := tg.Fetch(ctx, remote, "+refs/heads/"+res.Branch+":"+trunkRef)
	switch {
	case fetchErr == nil:
		switch err := tg.Fetch(ctx, remote, "+"+notes.Ref+":"+trunkNotesRef); {
		case err == nil:
			res.NotesFetched = true
		case isMissingRemoteRef(err):
			// Nothing has landed with a note yet.
		default:
			res.Warnings = append(res.Warnings, "fetching the trunk's "+notes.Ref+" failed: "+err.Error())
		}
	case isMissingRemoteRef(fetchErr):
		res.Warnings = append(res.Warnings, "the trunk has no commits yet; nothing to merge")
	default:
		return nil, fetchErr
	}
	if err := s.checkNoToken(ctx, tok.Token.Value, res); err != nil {
		return nil, err
	}
	res.Next = next
	if fetchErr != nil {
		res.UpToDate = true
		return res, nil
	}

	trunkHead, _, err := g.Resolve(ctx, trunkRef)
	if err != nil {
		return nil, err
	}
	res.TrunkHead = trunkHead
	if isAncestor(ctx, g, trunkHead, head) {
		res.UpToDate = true
		return res, nil
	}
	res.FastForward = isAncestor(ctx, g, head, trunkHead)
	msg := fmt.Sprintf("Merge trunk %s into intent %s\n\nWhy: the trunk moved after this intent forked; the change must be\ndecided again against the current trunk and its policy before it lands.\n\nWayglass-Intent: %s\n",
		short(trunkHead), it.ID, it.ID)
	unsigned, err := mergeSigned(ctx, g, "--no-edit", "-m", msg, trunkRef)
	if err != nil {
		out, _ := g.Run(ctx, "diff", "--name-only", "--diff-filter=U")
		conflicts := strings.Fields(out)
		if len(conflicts) == 0 {
			return nil, err
		}
		res.Conflicts = conflicts
		res.Next = fmt.Sprintf("resolve %s, git add them, git commit --no-edit, then %s", strings.Join(conflicts, ", "), next)
		return res, &Error{
			Code: CodeSyncConflict, Exit: ExitError, Details: res,
			Message: fmt.Sprintf("the trunk (%s) conflicts with intent %s in %s; the merge is left in progress. Next: %s",
				short(trunkHead), it.ID, strings.Join(conflicts, ", "), res.Next),
		}
	}
	if unsigned {
		res.MergeUnsigned = true
		res.Warnings = append(res.Warnings, "your git config asks to sign commits but signing failed here, so the sync merge is unsigned")
	}
	res.Merged = true
	if res.Head, err = g.Head(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// checkForkClone refuses a checkout that is not the intent's own clone: one
// that declared it, or whose HEAD already contains the intent's pushed head.
// Merging the trunk anywhere else would not reach the fork with the next push.
func (s *session) checkForkClone(ctx context.Context, head string) error {
	it := s.intent
	pushed := it.Head()
	if pushed != "" {
		if _, ok, _ := s.w.git.Resolve(ctx, pushed+"^{commit}"); ok && isAncestor(ctx, s.w.git, pushed, head) {
			return nil
		}
		return errorf(CodeNotForkClone, ExitUsage,
			"HEAD %s does not contain intent %s's pushed head %s, so this is not its fork clone; run wayglass sync in the checkout that declared %s",
			short(head), it.ID, short(pushed), it.ID)
	}
	if s.w.currentIntent() == it.ID {
		return nil
	}
	return errorf(CodeNotForkClone, ExitUsage,
		"this checkout did not declare intent %s and the intent has no pushed head yet; run wayglass sync in the checkout that declared it", it.ID)
}

// checkNoToken scans .git for the read token after the fetch, like clone.
func (s *session) checkNoToken(ctx context.Context, token string, res *syncResult) error {
	gitDir, err := s.w.git.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	found, err := tokenInGitDir(filepath.Clean(strings.TrimSpace(gitDir)), token)
	if err != nil {
		return err
	}
	if found != "" {
		return errorf(CodeTokenOnDisk, ExitError, "a repo token was written to %s; delete that file (the token expires on its own within minutes)", found)
	}
	res.NoTokenOnDisk = true
	return nil
}

func renderSync(out io.Writer, st styles, r syncResult) error {
	switch {
	case r.TrunkHead == "":
		_, _ = lipgloss.Fprintf(out, "The trunk has no commits yet; %s needs nothing from it.\n", st.id(r.IntentID))
	case r.UpToDate:
		_, _ = lipgloss.Fprintf(out, "%s already contains trunk %s at %s.\n", st.id(r.IntentID), r.Branch, st.id(short(r.TrunkHead)))
	case r.FastForward:
		_, _ = lipgloss.Fprintf(out, "%s %s to trunk %s at %s (fast-forward).\n", st.good.Render("Synced"), st.id(r.IntentID), r.Branch, st.id(short(r.TrunkHead)))
	default:
		_, _ = lipgloss.Fprintf(out, "%s trunk %s at %s into %s (merge %s).\n", st.good.Render("Merged"), r.Branch, st.id(short(r.TrunkHead)), st.id(r.IntentID), st.id(short(r.Head)))
	}
	_, _ = lipgloss.Fprintf(out, "Next: %s\n", r.Next)
	return nil
}
