package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/examples"
	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/policy"
)

// Error codes init can answer with before anything is created.
const (
	// CodeDetachedHead: init pushes the current branch, and HEAD is on none.
	CodeDetachedHead = "detached_head"
	// CodeFileTooLarge: a file in the history is over Artifacts' per-file limit.
	CodeFileTooLarge = "file_too_large"
	// CodeBadSource: --from was given a URL git must not be handed.
	CodeBadSource = "bad_source"
)

// trunkRemoteName is the git remote init adds for the trunk in the
// checkout, so plain git fetch reaches it (read-only, through the
// credential helper) next to whatever origin the repository already has.
const trunkRemoteName = "wayglass"

// Artifacts limits (developers.cloudflare.com/artifacts/platform/limits):
// a push holding a larger object is refused without naming the file.
const (
	artifactsMaxBlob int64 = 32 << 20
	artifactsMaxRepo int64 = 1 << 30
)

// maxBlobSize is the per-file limit init checks before creating anything.
// A variable so tests can shrink it instead of writing 32 MB files.
var maxBlobSize = artifactsMaxBlob

type initResult struct {
	Project api.Project `json:"project"`
	Trunk   api.Repo    `json:"trunk"`
	// Dir is the checkout init ran in (the clone it made with --from).
	Dir string `json:"dir"`
	// Source is the URL --from cloned, as given (it never carries a credential).
	Source        string `json:"source,omitempty"`
	Jurisdiction  string `json:"jurisdiction"`
	Public        bool   `json:"public"`
	PolicyWritten bool   `json:"policy_written"`
	Committed     string `json:"committed,omitempty"`
	// Branch is the local branch whose HEAD went to the trunk; Pushed is the
	// trunk branch it went to. They differ for a repository whose default
	// branch is not the trunk's (for example master).
	Branch string `json:"branch"`
	Pushed string `json:"pushed,omitempty"`
	// Renamed is true when --from renamed the local branch to the trunk's
	// name, so the checkout and the trunk agree on what main is.
	Renamed    bool `json:"branch_renamed"`
	TagsPushed bool `json:"tags_pushed"`
	// Remote is the git remote added for the trunk, with the credential
	// helper and notes refspec that make plain git fetch work there.
	Remote           string           `json:"remote,omitempty"`
	CredentialHelper string           `json:"credential_helper,omitempty"`
	NotesRefspec     string           `json:"notes_refspec,omitempty"`
	Mirror           string           `json:"mirror,omitempty"`
	NoTokenOnDisk    bool             `json:"no_token_on_disk"`
	TrunkEvents      *api.TrunkEvents `json:"trunk_events,omitempty"`
	Warnings         []string         `json:"warnings"`
}

// initOptions are the flags of wayglass init.
type initOptions struct {
	project, jurisdiction, mirror string
	noPush, public, tags          bool
}

func newInitCmd(a *app) *cobra.Command {
	var opts initOptions
	var from string
	var allowLocal bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Bring a repository under Wayglass: create its trunk in Artifacts and write .wayglass/policy.yaml",
		Long: "Creates the project and its trunk repo through the Worker, writes\n" +
			".wayglass/policy.yaml (from the built-in example, if missing) and\n" +
			".wayglass/config.yaml, commits them, and pushes the current branch's\n" +
			"history to the trunk with a short-lived trunk token that is never written\n" +
			"to disk. Run it inside the repository you want agents to work on; an\n" +
			"existing repository with its history, branches and remotes is the normal\n" +
			"case. Any remote it already has (origin on GitHub, say) stays as it is.\n\n" +
			"With --from <url>, init first clones that repository into dir (default:\n" +
			"the repository's name) with your own git credentials (ssh-agent, your\n" +
			"credential helper; Wayglass never sees or stores them), so a private\n" +
			"repository works, then does the same in the clone. The source stays as\n" +
			"remote origin.\n\n" +
			"Before creating anything it checks what Artifacts would refuse: a file\n" +
			"over 32 MB anywhere in the history is named here instead of failing the\n" +
			"push, and a detached HEAD is refused because the trunk takes a branch.\n" +
			"The trunk branch is main whatever the local branch is called; with\n" +
			"--from the local branch is renamed to match. Tags stay local unless --tags.\n\n" +
			"The checkout gets a remote named wayglass for the trunk, with the same\n" +
			"credential helper 'wayglass clone' configures, so plain 'git fetch wayglass'\n" +
			"works read-only and brings the why-notes along. Pushes to the trunk still\n" +
			"go through 'wayglass land'; 'wayglass publish' sends landed work back to\n" +
			"origin.",
		Example: "  wayglass init                                   # in an existing checkout\n" +
			"  wayglass init --from https://github.com/junegunn/fzf.git\n" +
			"  wayglass init --from git@github.com:you/private-app.git app --project app\n" +
			"  wayglass init --jurisdiction eu --tags --mirror origin",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			opts.jurisdiction = strings.ToLower(strings.TrimSpace(opts.jurisdiction))
			if opts.jurisdiction == "" {
				return UsageError("--jurisdiction must not be empty")
			}
			if opts.project != "" && !validProjectName(opts.project) {
				return UsageError("project %q must be lowercase letters, digits, '.', '_' or single '-' (max 63)", opts.project)
			}
			if from == "" && len(args) > 0 {
				return UsageError("a directory only goes with --from <url>; run init inside the repository otherwise")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			res := initResult{Warnings: []string{}}
			var w *workspace
			if from != "" {
				src, err := gitx.ParseSource(from, allowLocal)
				if err != nil {
					return errorf(CodeBadSource, ExitUsage, "%v", err)
				}
				dir := src.Name
				if len(args) > 0 {
					dir = args[0]
				}
				abs, err := filepath.Abs(dir)
				if err != nil {
					return err
				}
				if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
					return UsageError("%s already exists and is not empty; name another directory", abs)
				} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if opts.project == "" {
					opts.project = projectNameFrom(src.Name)
				}
				if !validProjectName(opts.project) {
					return UsageError("%q does not make a project name; pass --project", src.Name)
				}
				o.Warn("cloning %s with your own git credentials", src.URL)
				if err := (gitx.Git{}).CloneSource(ctx, src, abs); err != nil {
					return fmt.Errorf("cloning %s failed (your git credentials for it are what count here): %w", src.URL, err)
				}
				res.Source = src.URL
				w = &workspace{root: abs, git: gitx.Git{Dir: abs}}
			} else {
				w, err = openRepo(ctx)
				if err != nil {
					return err
				}
				if opts.project == "" {
					opts.project = projectNameFrom(filepath.Base(w.root))
				}
				if !validProjectName(opts.project) {
					return UsageError("project %q must be lowercase letters, digits, '.', '_' or single '-' (max 63)", opts.project)
				}
			}
			if err := runInit(ctx, c, w, opts, from != "", &res); err != nil {
				return err
			}
			for _, wmsg := range res.Warnings {
				o.Warn("%s", wmsg)
			}
			return o.Emit(res, func(out io.Writer) error { return renderInit(out, cliStyles(o), &res) })
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: the directory or repository name)")
	cmd.Flags().StringVar(&opts.jurisdiction, "jurisdiction", "us", "where the project's repos live (us or eu); tags every change and cannot be changed later")
	cmd.Flags().BoolVar(&opts.noPush, "no-push", false, "create the project but do not push the branch to the trunk")
	cmd.Flags().BoolVar(&opts.public, "public", false, "opt the project in to the public read-only audit view (the Worker's PUBLIC_PROJECTS must also match it)")
	cmd.Flags().BoolVar(&opts.tags, "tags", false, "push the repository's tags to the trunk as well")
	cmd.Flags().StringVar(&opts.mirror, "mirror", "", "git remote that 'wayglass land' pushes the trunk and its why-notes to after landing (e.g. origin)")
	cmd.Flags().StringVar(&from, "from", "", "clone this repository first (https://, ssh:// or git@host:path) and bring the clone under Wayglass")
	cmd.Flags().BoolVar(&allowLocal, "allow-local", false, "with --from: accept a local path or file:// URL")
	return cmd
}

// runInit brings the checkout w under Wayglass. Everything that can be
// checked locally is checked before the project exists on the Worker, so a
// refusal costs nothing and leaves nothing behind.
func runInit(ctx context.Context, c *api.Client, w *workspace, opts initOptions, cloned bool, res *initResult) error {
	res.Dir, res.Jurisdiction = w.root, opts.jurisdiction
	if _, err := os.Stat(w.path(configFile)); err == nil {
		return errorf(api.CodeConflict, ExitError, "%s already exists; this checkout is already initialised", w.path(configFile))
	}
	branch, err := w.git.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	if branch == "" {
		return errorf(CodeDetachedHead, ExitError, "HEAD is detached; init pushes the current branch to the trunk, so check one out first (git switch <branch>)")
	}
	res.Branch = branch
	if opts.mirror != "" {
		if opts.mirror == trunkRemoteName {
			return UsageError("--mirror %s is the trunk itself; name the remote to publish to (e.g. origin)", opts.mirror)
		}
		if _, ok, err := w.git.RemoteURL(ctx, opts.mirror); err != nil {
			return err
		} else if !ok {
			return UsageError("--mirror %s: this checkout has no such git remote", opts.mirror)
		}
	}
	if _, hasHead, err := w.git.Resolve(ctx, "HEAD"); err != nil {
		return err
	} else if hasHead && !opts.noPush {
		if err := checkPushable(ctx, w.git, opts.tags, res); err != nil {
			return err
		}
	}
	existingRemote, hasRemote, err := w.git.RemoteURL(ctx, trunkRemoteName)
	if err != nil {
		return err
	}

	// Validate the policy before creating anything remote.
	policyPath := w.path(policyFile)
	if _, err := os.Stat(policyPath); errors.Is(err, fs.ErrNotExist) {
		if err := w.write(examples.Policy, policyFile); err != nil {
			return err
		}
		res.PolicyWritten = true
	}
	if _, err := policy.LoadFile(policyPath); err != nil {
		return errorf(CodePolicy, ExitError, "%v", err)
	}

	created, err := c.CreateProjectWith(ctx, api.CreateProjectInput{Name: opts.project, Jurisdiction: opts.jurisdiction, Public: opts.public})
	if err != nil {
		return err
	}
	res.Public = created.Public
	if opts.public && !created.Public {
		res.Warnings = append(res.Warnings, "this Worker did not confirm --public; opt in with PATCH /v1/projects/"+opts.project+` {"public": true}`)
	}
	// The Worker's namespace is authoritative for tags, so record what it used.
	if j := created.Project.Jurisdiction; j != "" {
		res.Jurisdiction = j
	}
	res.Project, res.Trunk, res.TrunkEvents = created.Project, created.Trunk, created.Events
	if ev := created.Events; ev != nil && !ev.Subscribed {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"trunk push events not subscribed (%s: %s); pushes that skip `wayglass land` will not be flagged",
			ev.Reason, ev.Detail))
	}
	w.cfg = projectConfig{Version: 1, Project: opts.project, TrunkRepo: created.Trunk.Name,
		TrunkRemote: created.Trunk.Remote, Jurisdiction: res.Jurisdiction, Mirror: opts.mirror}
	res.Mirror = opts.mirror
	if err := w.saveConfig(); err != nil {
		return err
	}
	if err := w.write([]byte(ignoreContents), ignoreFile); err != nil {
		return err
	}

	// The trunk must hold the policy: the Worker re-derives decisions
	// from what is in Artifacts (D3). Commit only our own files, so a
	// dirty working tree stays the person's business.
	files := []string{
		filepath.Join(dirName, policyFile), filepath.Join(dirName, configFile), filepath.Join(dirName, ignoreFile),
	}
	sha, err := w.git.CommitPaths(ctx, initCommitMessage, files...)
	if err != nil {
		return err
	}
	res.Committed = sha

	// The trunk remote: plain git fetch works there through the credential
	// helper, read-only, like a 'wayglass clone' checkout. Pushing through
	// the remote name (not the URL) also sets wayglass/<branch>.
	target := created.Trunk.Remote
	switch {
	case hasRemote && existingRemote != created.Trunk.Remote:
		res.Warnings = append(res.Warnings, fmt.Sprintf("git remote %s already exists (%s); left as it is, so plain git fetch of the trunk is not configured", trunkRemoteName, existingRemote))
	case hasRemote:
		res.Remote, target = trunkRemoteName, trunkRemoteName
	default:
		if err := w.git.AddRemote(ctx, trunkRemoteName, created.Trunk.Remote); err != nil {
			return err
		}
		res.Remote, target = trunkRemoteName, trunkRemoteName
		res.CredentialHelper = credentialHelperValue(helperExecutable(), opts.project, "")
		if err := configureCredentialHelper(ctx, w.git, created.Trunk.Remote, res.CredentialHelper); err != nil {
			return err
		}
		res.NotesRefspec = notesFollowRefspec
		if err := configureNotesFollow(ctx, w.git, trunkRemoteName); err != nil {
			return err
		}
	}

	trunkBranch := cmp.Or(created.Trunk.DefaultBranch, "main")
	if opts.noPush {
		res.Warnings = append(res.Warnings, "trunk not pushed (--no-push); forks will start empty")
	} else {
		refspecs := []string{"HEAD:refs/heads/" + trunkBranch}
		if opts.tags {
			refspecs = append(refspecs, "refs/tags/*:refs/tags/*")
		}
		tg := w.git
		tg.Token = created.Token.Value // in memory only
		if err := tg.Push(ctx, target, refspecs...); err != nil {
			return fmt.Errorf("project %s was created, but pushing to the trunk failed: %w", opts.project, err)
		}
		res.Pushed, res.TagsPushed = trunkBranch, opts.tags
	}

	// A clone made for this init can take the trunk's branch name, so the
	// checkout, wayglass/main and 'wayglass land' agree on what main is. A
	// repository someone already works in keeps its branch names.
	if cloned && branch != trunkBranch {
		if _, taken, _ := w.git.Resolve(ctx, "refs/heads/"+trunkBranch); taken {
			res.Warnings = append(res.Warnings, fmt.Sprintf("local branch %s kept its name: a %s branch already exists; the trunk branch is %s", branch, trunkBranch, trunkBranch))
		} else if _, err := w.git.Run(ctx, "branch", "-m", branch, trunkBranch); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not rename local branch %s to %s: %v", branch, trunkBranch, err))
		} else {
			res.Renamed = true
		}
	} else if branch != trunkBranch && !opts.noPush {
		res.Warnings = append(res.Warnings, fmt.Sprintf("the trunk branch is %s; your local branch is %s (git branch -m %s %s makes 'wayglass land' fast-forward it)", trunkBranch, branch, branch, trunkBranch))
	}

	gitDir, err := w.git.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	found, err := tokenInGitDir(gitDir, created.Token.Value)
	if err != nil {
		return err
	}
	if found != "" {
		return errorf(CodeTokenOnDisk, ExitError, "the trunk token was written to %s; remove it (it expires on its own within minutes)", found)
	}
	res.NoTokenOnDisk = true
	return nil
}

// initCommitMessage is the why of the first Wayglass commit in a repository.
const initCommitMessage = "chore: bring this repository under Wayglass\n\n" +
	"From here on every change carries its why. .wayglass/policy.yaml routes\n" +
	"each change to an agent or a person by its tags (area, data class, risk,\n" +
	"jurisdiction) instead of trusting whoever pushes, and config.yaml names\n" +
	"the project and its trunk in Artifacts so `wayglass why` can answer for\n" +
	"any line. Committed on the trunk because the Worker re-reads the policy\n" +
	"from there for every decision."

// checkPushable refuses, before anything exists on the Worker, a history
// Artifacts would reject, naming the files; a repository near its size
// limit gets a warning.
func checkPushable(ctx context.Context, g gitx.Git, tags bool, res *initResult) error {
	big, err := g.LargeBlobs(ctx, maxBlobSize, "HEAD", tags)
	if err != nil {
		return err
	}
	if len(big) > 0 {
		names := make([]string, 0, len(big))
		for i, b := range big {
			if i == 5 {
				names = append(names, fmt.Sprintf("and %d more", len(big)-5))
				break
			}
			names = append(names, fmt.Sprintf("%s (%s)", cmp.Or(b.Path, b.Hash), sizeMB(b.Size)))
		}
		scope := "the current branch's history"
		if tags {
			scope += " or a tag"
		}
		return &Error{Code: CodeFileTooLarge, Exit: ExitError, Details: map[string]any{"limit": maxBlobSize, "files": big},
			Message: fmt.Sprintf("%d file(s) in %s exceed Artifacts' %s per-file limit, so the push would be refused: %s. Remove them from the history (git filter-repo) or start the trunk from a fresh history",
				len(big), scope, sizeMB(maxBlobSize), strings.Join(names, ", "))}
	}
	if n, err := g.ObjectsSize(ctx); err == nil && n > artifactsMaxRepo {
		res.Warnings = append(res.Warnings, fmt.Sprintf("this repository's objects take %s; Artifacts allows %s per repo, so the push may be refused", sizeMB(n), sizeMB(artifactsMaxRepo)))
	}
	return nil
}

func sizeMB(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
}

func renderInit(out io.Writer, st styles, r *initResult) error {
	if r.Source != "" {
		_, _ = lipgloss.Fprintf(out, "%s %s into %s (your git credentials; origin stays there).\n", st.title.Render("Cloned"), r.Source, relDir(r.Dir))
	}
	_, err := lipgloss.Fprintf(out, "%s %s (trunk %s, jurisdiction %s).\n", st.title.Render("Created project"), r.Project.Name, r.Trunk.Name, r.Jurisdiction)
	if r.PolicyWritten {
		fmt.Fprintf(out, "  Wrote %s from the example policy.\n", filepath.Join(dirName, policyFile))
	}
	if r.Pushed != "" {
		what := "branch " + r.Branch
		if r.TagsPushed {
			what += " and tags"
		}
		_, _ = lipgloss.Fprintf(out, "  Pushed %s at %s to the trunk's %s.\n", what, st.id(short(r.Committed)), r.Pushed)
	}
	if r.Renamed {
		fmt.Fprintf(out, "  Renamed local branch %s to %s to match the trunk.\n", r.Branch, r.Pushed)
	}
	if r.Remote != "" {
		_, _ = lipgloss.Fprintf(out, "  Remote %s is the trunk: plain git fetch %s works read-only and brings the why-notes along.\n", r.Remote, r.Remote)
	}
	if r.Mirror != "" {
		fmt.Fprintf(out, "  'wayglass land' will push the trunk and its notes to %s.\n", r.Mirror)
	}
	if r.Public {
		fmt.Fprintf(out, "  Opted in to the public audit view; it shows only if the Worker's PUBLIC_PROJECTS also matches %s.\n", r.Project.Name)
	}
	next := "'wayglass agent setup' teaches coding agents the loop in AGENTS.md."
	if r.Source != "" {
		next = "cd " + relDir(r.Dir) + " && wayglass agent setup"
	}
	_, _ = lipgloss.Fprintln(out, st.muted.Render("Next:")+" "+next)
	return err
}
