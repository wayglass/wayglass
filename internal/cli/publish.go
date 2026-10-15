package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/notes"
)

// CodePublishRejected means the target remote refused the trunk head: it
// has commits the trunk lacks.
const CodePublishRejected = "publish_rejected"

type publishResult struct {
	Project string `json:"project"`
	// Remote is the git remote published to and Branch the branch there.
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	// Commit is the trunk head that is now on the remote's branch.
	Commit string `json:"commit"`
	// TrunkBranch is the trunk's branch the head came from.
	TrunkBranch string `json:"trunk_branch"`
	// NotesPushed is false when the trunk has no why-notes yet or --no-notes.
	NotesPushed   bool     `json:"notes_pushed"`
	NoTokenOnDisk bool     `json:"no_token_on_disk"`
	Warnings      []string `json:"warnings"`
}

func newPublishCmd(a *app) *cobra.Command {
	var remote, branch string
	var noNotes bool
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Push the landed trunk and its why-notes to the repository's original remote (GitHub, GitLab, ...)",
		Long: "Fetches the trunk's head and refs/notes/wayglass from Artifacts with a\n" +
			"60-second read-only token (in memory only) and pushes them to a git remote\n" +
			"of this checkout with your own git credentials (ssh-agent, credential\n" +
			"helper); Wayglass never holds those. The remote is origin unless --remote,\n" +
			"and the branch is what that remote's HEAD points at (refs/remotes/<remote>/HEAD),\n" +
			"else the trunk's branch, unless --branch.\n\n" +
			"The push is never forced. If the remote has commits the trunk lacks, it is\n" +
			"refused with publish_rejected: bring those commits in through an intent\n" +
			"first (declare, merge them, push, propose, land), then publish again. The\n" +
			"notes ref is pushed as a whole; GitHub, GitLab and the like store it but\n" +
			"show it only through git (git fetch origin '+refs/notes/wayglass:refs/notes/wayglass',\n" +
			"then git log), not in their web views.\n\n" +
			"With mirror: <remote> in .wayglass/config.yaml, 'wayglass land' does this\n" +
			"after every land; publish is the by-hand or catch-up version.",
		Example: "  wayglass publish\n  wayglass publish --remote github --branch master --json",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			w, err := openWorkspace(ctx)
			if err != nil {
				return err
			}
			if w.cfg.TrunkRemote == "" {
				return errorf(CodeNotInitialized, ExitError, "%s names no trunk_remote; run publish in a checkout made by wayglass init or clone", w.path(configFile))
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			res, err := publish(ctx, w, c, remote, branch, noNotes)
			if err != nil {
				return err
			}
			for _, wmsg := range res.Warnings {
				o.Warn("%s", wmsg)
			}
			return o.Emit(res, func(out io.Writer) error {
				st := cliStyles(o)
				_, err := lipgloss.Fprintf(out, "%s trunk %s at %s to %s/%s.\n", st.title.Render("Published"), res.TrunkBranch, st.id(short(res.Commit)), res.Remote, res.Branch)
				if res.NotesPushed {
					fmt.Fprintf(out, "  %s went along; git log shows the why there after: git fetch %s '+%s:%s'\n", notes.Ref, res.Remote, notes.Ref, notes.Ref)
				} else {
					fmt.Fprintln(out, "  No why-notes pushed.")
				}
				return err
			})
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "origin", "git remote of this checkout to push to")
	cmd.Flags().StringVar(&branch, "branch", "", "branch on that remote (default: its HEAD, else the trunk's branch)")
	cmd.Flags().BoolVar(&noNotes, "no-notes", false, "push the branch only, not refs/notes/wayglass")
	return cmd
}

// publish fetches the trunk head and notes with a read token, then pushes
// them to remote with the person's own git credentials.
func publish(ctx context.Context, w *workspace, c *api.Client, remote, branch string, noNotes bool) (*publishResult, error) {
	if !validRemoteName(remote) {
		return nil, UsageError("%q is not a git remote name", remote)
	}
	if remote == trunkRemoteName {
		return nil, UsageError("%s is the trunk itself; name the remote to publish to (e.g. origin)", remote)
	}
	if url, ok, err := w.git.RemoteURL(ctx, remote); err != nil {
		return nil, err
	} else if !ok {
		return nil, UsageError("this checkout has no git remote %s", remote)
	} else if url == w.cfg.TrunkRemote {
		return nil, UsageError("remote %s is the trunk itself; name the remote to publish to", remote)
	}
	if branch != "" && (!validRemoteName(branch) || strings.HasPrefix(branch, "refs/")) {
		return nil, UsageError("%q is not a branch name", branch)
	}
	src, err := mintReadToken(ctx, c, w.cfg.Project, "")
	if err != nil {
		return nil, err
	}
	res := &publishResult{Project: w.cfg.Project, Remote: remote, Warnings: []string{}}
	tg := w.git
	tg.Token = src.Token.Value // in memory only; expires in about a minute
	tb, _ := tg.RemoteDefaultBranch(ctx, src.Remote)
	res.TrunkBranch = cmp.Or(tb, "main")
	if err := tg.Fetch(ctx, src.Remote, "+refs/heads/"+res.TrunkBranch+":"+trunkRef); err != nil {
		return nil, err
	}
	hasNotes := false
	switch err := notes.Fetch(ctx, tg, src.Remote); {
	case err == nil:
		hasNotes = true
	case isMissingRemoteRef(err):
	default:
		res.Warnings = append(res.Warnings, "fetching "+notes.Ref+" from the trunk failed: "+err.Error())
	}
	gitDir, err := w.git.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	if found, err := tokenInGitDir(gitDir, src.Token.Value); err != nil {
		return nil, err
	} else if found != "" {
		return nil, errorf(CodeTokenOnDisk, ExitError, "the trunk token was written to %s; remove it (it expires within a minute)", found)
	}
	res.NoTokenOnDisk = true

	head, ok, err := w.git.Resolve(ctx, trunkRef)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf(CodeNotPushed, ExitError, "the trunk has no commits yet; nothing to publish")
	}
	res.Commit = head
	if branch == "" {
		branch = remoteHeadBranch(ctx, w.git, remote)
	}
	res.Branch = cmp.Or(branch, res.TrunkBranch)

	// The person's own credentials, never a token: no Token on this Git.
	if err := w.git.Push(ctx, remote, trunkRef+":refs/heads/"+res.Branch); err != nil {
		var ge *gitx.Error
		if errors.As(err, &ge) && (strings.Contains(ge.Stderr, "non-fast-forward") || strings.Contains(ge.Stderr, "fetch first") || strings.Contains(ge.Stderr, "rejected")) {
			return nil, errorf(CodePublishRejected, ExitError, "%s/%s has commits the trunk lacks; bring them in through an intent (git fetch %s, merge into an intent's branch, wayglass push, propose, land), then publish again: %v", remote, res.Branch, remote, err)
		}
		return nil, err
	}
	if hasNotes && !noNotes {
		if err := notes.Push(ctx, w.git, remote); err != nil {
			res.Warnings = append(res.Warnings, "the branch is published, but pushing "+notes.Ref+" failed: "+err.Error())
		} else {
			res.NotesPushed = true
		}
	}
	return res, nil
}

// remoteHeadBranch is the branch refs/remotes/<remote>/HEAD points at (set by
// git clone), or "" when the checkout does not know.
func remoteHeadBranch(ctx context.Context, g gitx.Git, remote string) string {
	out, err := g.Run(ctx, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return ""
	}
	b, _ := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/"+remote+"/")
	return b
}

// validRemoteName accepts what git accepts for a remote or branch name,
// minus anything git would read as an option or a refspec.
func validRemoteName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n:*~^?[\\") || strings.Contains(s, "..") {
		return false
	}
	return true
}
