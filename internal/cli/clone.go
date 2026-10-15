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
	"gopkg.in/yaml.v3"

	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/notes"
)

// CodeTokenOnDisk means a repo token was found in the new checkout's .git
// after cloning. It should never happen; the checkout is left for inspection.
const CodeTokenOnDisk = "token_on_disk"

type cloneResult struct {
	Project string `json:"project"`
	Dir     string `json:"dir"`
	// Trunk is the trunk repo name; empty for an intent fork checkout.
	Trunk string `json:"trunk"`
	// Intent and Fork are set when --intent cloned that intent's fork.
	Intent string `json:"intent"`
	Fork   string `json:"fork"`
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	// Head is empty when the trunk has no commits yet.
	Head         string `json:"head"`
	NotesFetched bool   `json:"notes_fetched"`
	// ConfigWritten is true when the trunk had no .wayglass/config.yaml
	// and clone wrote one (untracked) so land, review and why work here.
	ConfigWritten bool   `json:"config_written"`
	TokenScope    string `json:"token_scope"`
	TokenExpires  string `json:"token_expires_at"`
	// NoTokenOnDisk is the result of scanning .git after the clone.
	NoTokenOnDisk bool `json:"no_token_on_disk"`
	// CredentialHelper is the git config value (credential.<remote>.helper)
	// that lets plain git fetch here by minting a read token each time.
	CredentialHelper string `json:"credential_helper"`
	// NotesRefspec is the extra remote.origin.fetch entry that keeps the
	// why-notes current on plain git fetch; git log shows them (notes.displayRef).
	NotesRefspec string   `json:"notes_refspec"`
	Warnings     []string `json:"warnings"`
}

// notesFollowRefspec fetches the why-notes with every plain git fetch. The
// glob matters: an exact refspec makes fetch fail while the project has no
// notes yet, a pattern that matches nothing is fine.
const notesFollowRefspec = "+" + notes.Ref + "*:" + notes.Ref + "*"

func newCloneCmd(a *app) *cobra.Command {
	var intent string
	cmd := &cobra.Command{
		Use:   "clone <project> [dir]",
		Short: "Check out a project's trunk (or an intent's fork) with its why-notes; plain git fetch keeps working",
		Long: "Asks the Worker for a short-lived read-only trunk token, clones the trunk\n" +
			"into dir (default: the project name) and fetches refs/notes/wayglass, so\n" +
			"'wayglass why', 'review' and 'land <intent-id>' work in the new checkout.\n" +
			"With --intent <id> it clones that intent's fork instead (default dir:\n" +
			"<project>-<id>), so you can 'git pull' to follow an agent's work.\n" +
			"The token reaches git through its environment, never argv, a URL or disk;\n" +
			"clone checks .git afterwards and fails if a token is there. The checkout\n" +
			"keeps no credentials: it configures 'wayglass git-credential' as the\n" +
			"credential helper for this remote only (resetting inherited helpers such\n" +
			"as osxkeychain), so plain 'git fetch' and 'git pull' mint a fresh\n" +
			"read-only token with your Wayglass key each time, and they bring the\n" +
			"why-notes along (a fetch refspec for refs/notes/wayglass); 'git log', tig,\n" +
			"lazygit and the like show the why under each commit (notes.displayRef).\n" +
			"Writes still go through 'wayglass push' and 'wayglass land'.",
		Example: "  wayglass clone demo-1791938487\n  wayglass clone demo-1791938487 review-copy --json\n" +
			"  wayglass clone demo-1791938487 --intent i83f41dd279   # follow an agent's fork",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			project := args[0]
			if !validProjectName(project) {
				return UsageError("%q is not a project name", project)
			}
			if intent != "" && !intentIDRe.MatchString(intent) {
				return UsageError("%q is not an intent id", intent)
			}
			dir := project
			if intent != "" {
				dir = project + "-" + intent
			}
			if len(args) > 1 {
				dir = args[1]
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			// Check before minting anything, so a bad dir costs no token.
			if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
				return UsageError("%s already exists and is not empty; name another directory", abs)
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			src, err := mintReadToken(ctx, c, project, intent)
			if err != nil {
				return err
			}
			res, err := cloneSource(ctx, project, src, abs)
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				o.Warn("%s", w)
			}
			return o.Emit(res, func(out io.Writer) error { return renderClone(out, cliStyles(o), res) })
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "clone this intent's fork (read-only) instead of the trunk")
	return cmd
}

// cloneSource clones the trunk or an intent fork into dir with the read
// token in memory only, brings the why-notes across, makes sure the checkout
// knows its project and configures the credential helper for later fetches.
func cloneSource(ctx context.Context, project string, src *readSource, dir string) (*cloneResult, error) {
	res := &cloneResult{
		Project: project, Dir: dir, Remote: src.Remote,
		TokenScope: src.Token.Scope, TokenExpires: src.Token.ExpiresAt, Warnings: []string{},
	}
	var intentID string
	if src.Intent != nil {
		intentID = src.Intent.ID
		res.Intent, res.Fork = intentID, src.Name
	} else {
		res.Trunk = src.Name
	}
	tg := gitx.Git{Token: src.Token.Value} // in memory only; expires within minutes
	if _, err := tg.Run(ctx, "clone", "--quiet", "--no-tags", src.Remote, dir); err != nil {
		return nil, err
	}
	g := gitx.Git{Dir: dir}
	tg.Dir = dir

	switch err := notes.Fetch(ctx, tg, src.Remote); {
	case err == nil:
		res.NotesFetched = true
	case isMissingRemoteRef(err):
		// Nothing has landed with a note yet: notes_fetched false.
	default:
		res.Warnings = append(res.Warnings, "fetching "+notes.Ref+" failed: "+err.Error())
	}

	if head, ok, err := g.Resolve(ctx, "HEAD"); err == nil && ok {
		res.Head = head
		if b, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			res.Branch = b
		}
	} else {
		res.Warnings = append(res.Warnings, "the trunk has no commits yet")
	}

	if err := ensureProjectConfig(dir, project, src, res); err != nil {
		return nil, err
	}

	res.CredentialHelper = credentialHelperValue(helperExecutable(), project, intentID)
	if err := configureCredentialHelper(ctx, g, src.Remote, res.CredentialHelper); err != nil {
		return nil, err
	}
	res.NotesRefspec = notesFollowRefspec
	if err := configureNotesFollow(ctx, g, "origin"); err != nil {
		return nil, err
	}

	found, err := tokenInGitDir(filepath.Join(dir, ".git"), src.Token.Value)
	if err != nil {
		return nil, err
	}
	if found != "" {
		return nil, errorf(CodeTokenOnDisk, ExitError, "a repo token was written to %s; delete %s (the token expires on its own within minutes)", found, dir)
	}
	res.NoTokenOnDisk = true
	return res, nil
}

// configureNotesFollow makes plain git fetch of remote keep refs/notes/wayglass
// current and git log show the why under each commit, so any git tool sees it.
func configureNotesFollow(ctx context.Context, g gitx.Git, remote string) error {
	if _, err := g.Run(ctx, "config", "--local", "--add", "remote."+remote+".fetch", notesFollowRefspec); err != nil {
		return err
	}
	_, err := g.Run(ctx, "config", "--local", "notes.displayRef", notes.Ref)
	return err
}

func isMissingRemoteRef(err error) bool {
	var ge *gitx.Error
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref")
}

// ensureProjectConfig writes .wayglass/config.yaml when the trunk lacks one
// (a project whose trunk was not made by 'wayglass init'), and warns when the
// committed one names another project.
func ensureProjectConfig(dir, project string, src *readSource, res *cloneResult) error {
	w := &workspace{root: dir}
	b, err := os.ReadFile(w.path(configFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		w.cfg = projectConfig{Version: 1, Project: project}
		if src.Intent == nil {
			w.cfg.TrunkRepo, w.cfg.TrunkRemote = src.Name, src.Remote
		}
		if err := w.saveConfig(); err != nil {
			return err
		}
		res.ConfigWritten = true
		res.Warnings = append(res.Warnings, "the trunk has no "+filepath.Join(dirName, configFile)+"; wrote one for project "+project+" (not committed)")
		return nil
	case err != nil:
		return err
	}
	var cfg projectConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s does not parse: %v", filepath.Join(dirName, configFile), err))
		return nil
	}
	if cfg.Project != project {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s names project %q, not %q; set %s=%s to use this checkout for %s",
			filepath.Join(dirName, configFile), cfg.Project, project, EnvProject, project, project))
	}
	return nil
}

// tokenInGitDir returns the first file under gitDir that holds token (or its
// part before "?expires="), skipping git's compressed objects.
func tokenInGitDir(gitDir, token string) (string, error) {
	base, _, _ := strings.Cut(token, "?")
	if base == "" {
		return "", nil
	}
	var found string
	err := filepath.WalkDir(gitDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "objects" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // our own fresh checkout
		if err != nil {
			return err
		}
		if strings.Contains(string(b), base) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

func renderClone(out io.Writer, st styles, r *cloneResult) error {
	rel := relDir(r.Dir)
	what, next := "trunk "+r.Trunk, "wayglass review"
	if r.Intent != "" {
		what, next = "intent "+r.Intent+", fork "+r.Fork, "git pull   # follow the agent's pushes"
	}
	if r.Head == "" {
		_, err := lipgloss.Fprintf(out, "%s the empty %s of project %s into %s.\n", st.title.Render("Cloned"), what, r.Project, rel)
		return err
	}
	_, _ = lipgloss.Fprintf(out, "%s project %s (%s) into %s at %s on %s.\n", st.title.Render("Cloned"), r.Project, what, rel, st.id(short(r.Head)), cmp.Or(r.Branch, "a detached HEAD"))
	if r.NotesFetched {
		_, _ = lipgloss.Fprintf(out, "  Fetched %s: wayglass why <file>:<line> answers from git, and git log shows the why under each commit.\n", notes.Ref)
	} else {
		_, _ = lipgloss.Fprintln(out, "  No why-notes here yet; git log will show them under each commit once there are.")
	}
	_, _ = lipgloss.Fprintln(out, "  Read token used in memory only; none in .git.")
	_, _ = lipgloss.Fprintf(out, "  Plain git fetch and pull work here and keep the notes current: %s mints a read-only token each time.\n", helperCommand(r.CredentialHelper)+" git-credential")
	_, err := lipgloss.Fprintf(out, "%s cd %s && %s\n", st.muted.Render("Next:"), rel, next)
	return err
}
