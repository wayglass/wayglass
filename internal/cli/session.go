package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/notes"
)

// Session error codes.
const (
	CodeNoSession   = "no_session"
	CodeWrongWorker = "wrong_worker"
)

// The context a forked session hands the next agent. Both live in
// .wayglass/state/, which is git-ignored: the Worker holds the authoritative,
// append-only copy, and a committed file would show up in the change's diff
// and be classified as part of it.
const (
	sessionJSONFile = "session.json"
	sessionMDFile   = "session.md"
)

func newSessionCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Hand agent sessions between people and agents",
		Long: "A session is an intent's code plus its context: goal, reason, paths,\n" +
			"decisions, overlaps, history and the agent's handoff notes.\n\n" +
			"  share  freezes the current intent's context and prints its URL\n" +
			"  fork   continues a shared session as a new intent with its own fork\n" +
			"  show   prints the context for the agent starting work here",
		Args: usageArgs(cobra.NoArgs),
		RunE: groupRunE(a),
	}
	cmd.AddCommand(newSessionShareCmd(a), newSessionForkCmd(a), newSessionShowCmd(a))
	return cmd
}

// ---- share ------------------------------------------------------------------

type shareResult struct {
	URL      string      `json:"url"`
	Session  api.Session `json:"session"`
	Warnings []string    `json:"warnings"`
}

func newSessionShareCmd(a *app) *cobra.Command {
	var intentID, notesFile string
	var noteList []string
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Freeze the current intent's context and print a URL to continue it elsewhere",
		Long: "Records an append-only snapshot of the intent in the Worker: goal, reason,\n" +
			"declared and changed paths, decisions and approvals so far, overlaps, its\n" +
			"audit history, your handoff notes, and the fork head the Worker reads from\n" +
			"Artifacts. HEAD must already be pushed. Anyone logged in to the same\n" +
			"Worker can continue it with 'wayglass session fork <url>'; nobody receives\n" +
			"this fork's write token.",
		Example: `  wayglass session share --note "Limiter works; lockout tests still missing"
  wayglass session share --notes-file HANDOFF.md --json
  git log -3 --format=%B | wayglass session share --notes-file -`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			text, err := readNotes(cmd.InOrStdin(), noteList, notesFile)
			if err != nil {
				return err
			}
			var args []string
			if intentID != "" {
				args = []string{intentID}
			}
			s, err := a.openSession(ctx, args)
			if err != nil {
				return err
			}
			res := shareResult{Warnings: []string{}}
			head, err := s.w.git.Head(ctx)
			if err != nil {
				return err
			}
			if dirty, err := s.w.git.Dirty(ctx); err == nil && dirty {
				res.Warnings = append(res.Warnings, "uncommitted changes are not part of the session; commit and push them first")
			}
			sh, err := s.c.ShareSession(ctx, s.intent.ID, api.ShareSessionInput{Project: s.w.cfg.Project, Notes: text, Commit: head})
			if api.IsCode(err, api.CodeConflict) && strings.Contains(err.Error(), "push") {
				return errorf(CodeNotPushed, ExitError, "HEAD %s is not on the fork yet; run 'wayglass push' before sharing", short(head))
			}
			if err != nil {
				return err
			}
			res.URL, res.Session = sh.URL, sh.Session
			for _, w := range res.Warnings {
				o.Warn("%s", w)
			}
			return o.Emit(res, func(out io.Writer) error {
				st := cliStyles(o)
				_, _ = lipgloss.Fprintf(out, "%s %s\n\n", st.title.Render("Shared session "+sh.Session.ID), st.muted.Render("for "+s.intent.ID))
				row(out, st, "Goal", s.intent.Goal)
				row(out, st, "Code", ptrOr(sh.Session.HeadCommit, "empty fork", short)+" on "+s.intent.ForkRepo)
				row(out, st, "Notes", noteSummary(sh.Session.Notes))
				row(out, st, "URL", sh.URL)
				_, err := lipgloss.Fprintf(out, "\n%s wayglass session fork %s\n", st.muted.Render("Continue elsewhere:"), sh.URL)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&intentID, "intent", "", "intent to share (default: this checkout's current intent)")
	cmd.Flags().StringArrayVar(&noteList, "note", nil, "handoff note for the next agent (repeatable; joined by blank lines)")
	cmd.Flags().StringVar(&notesFile, "notes-file", "", "read handoff notes from a file ('-' for stdin)")
	return cmd
}

// maxNotes matches the Worker's limit.
const maxNotes = 32 * 1024

func readNotes(stdin io.Reader, list []string, file string) (string, error) {
	parts := make([]string, 0, len(list)+1)
	for _, n := range list {
		if n = strings.TrimSpace(n); n != "" {
			parts = append(parts, n)
		}
	}
	if file != "" {
		var b []byte
		var err error
		if file == "-" {
			b, err = io.ReadAll(io.LimitReader(stdin, maxNotes+1))
		} else {
			b, err = os.ReadFile(file) //nolint:gosec // the user names the file to read
		}
		if err != nil {
			return "", UsageError("--notes-file: %v", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			parts = append(parts, t)
		}
	}
	text := strings.Join(parts, "\n\n")
	if len(text) > maxNotes {
		return "", UsageError("handoff notes are %d bytes; the limit is %d", len(text), maxNotes)
	}
	return text, nil
}

// ---- fork -------------------------------------------------------------------

// forkedSession is what `session fork` reports and stores in
// .wayglass/state/session.json for `session show`. It never holds a token.
type forkedSession struct {
	URL     string            `json:"url"`
	Session api.Session       `json:"session"`
	Intent  api.Intent        `json:"intent"`
	Parent  api.SessionParent `json:"parent"`
	Fork    api.Repo          `json:"fork"`
	// Dir is the new checkout.
	Dir string `json:"dir"`
	// Head is what the clone holds; SnapshotHead what was shared. They differ
	// when the source agent pushed again after sharing.
	Head         string `json:"head"`
	SnapshotHead string `json:"snapshot_head"`
	Moved        bool   `json:"moved"`
	// Base is the commit the intent's diff is measured from: the source's
	// fork head when stacked on it, else the trunk commit it started from.
	Base string `json:"base"`
	// NotesFetched says whether refs/notes/wayglass came across the fork.
	NotesFetched bool          `json:"notes_fetched"`
	TokenScope   string        `json:"token_scope"`
	TokenExpires string        `json:"token_expires_at"`
	ContextFile  string        `json:"context_file"`
	ForkedAt     string        `json:"forked_at"`
	Overlaps     []api.Overlap `json:"overlaps"`
	Warnings     []string      `json:"warnings"`
}

func newSessionForkCmd(a *app) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "fork <url>",
		Short: "Continue a session elsewhere with its code, intent, notes and context",
		Long: "Asks the Worker to fork the session's fork into a NEW intent owned by you\n" +
			"(same goal, reason and declared paths; the parent is recorded), stacked on\n" +
			"the source intent like 'intent declare --on': your change is classified on\n" +
			"what you add, and it lands after the source does. If the source is\n" +
			"abandoned, your intent takes over its commits. Then it clones\n" +
			"it with a short-lived write token scoped to the new fork only. The token\n" +
			"reaches git through its environment, never argv, a URL or disk. The\n" +
			"original fork's token is never handed out.\n\n" +
			"The context is written to .wayglass/state/session.md; read it with\n" +
			"'wayglass session show'. Then work, 'wayglass push' and 'wayglass propose'\n" +
			"as usual.",
		Example: "  wayglass session fork https://api.wayglass.dev/v1/projects/wayglass/sessions/s3f9c0a12b4 --dir login-work",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			ref, err := api.ParseSessionURL(args[0])
			if err != nil {
				return UsageError("%v", err)
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			if !c.SameWorker(ref) {
				return errorf(CodeWrongWorker, ExitError,
					"session is on %s but you are logged in to %s; your key is only sent to your own Worker. Run 'wayglass login --worker %s' first",
					ref.Worker, c.BaseURL, ref.Worker)
			}
			dir = cmp.Or(dir, ref.Project+"-"+ref.ID)
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			// Check before the Worker forks anything, so a bad --dir costs no repo.
			if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
				return UsageError("%s already exists and is not empty; pass --dir", abs)
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}

			f, err := c.ForkSession(ctx, ref.Project, ref.ID)
			if err != nil {
				return err
			}
			res, err := cloneSession(ctx, f, ref, abs)
			if err != nil {
				return fmt.Errorf("intent %s and fork %s were created, but setting up %s failed (refresh the token with 'wayglass push' from a clone of %s): %w",
					f.Intent.ID, f.Fork.Name, abs, f.Fork.Remote, err)
			}
			for _, w := range res.Warnings {
				o.Warn("%s", w)
			}
			warnOverlaps(o, f.Intent.ID, nonFamily(f.Overlaps))
			return o.Emit(res, func(out io.Writer) error { return renderForked(out, cliStyles(o), res) })
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to clone into (default <project>-<session id>)")
	return cmd
}

// cloneSession clones the new fork into dir with the fork token in memory
// only, brings the notes across, and writes the local intent state and the
// session context.
func cloneSession(ctx context.Context, f *api.ForkSessionResult, ref api.SessionRef, dir string) (*forkedSession, error) {
	res := &forkedSession{
		URL: cmp.Or(f.URL, ref.URL()), Session: f.Session, Intent: f.Intent, Parent: f.Parent, Fork: f.Fork,
		Dir: dir, SnapshotHead: ptrOr(f.Session.HeadCommit, "", nil), TokenScope: f.Token.Scope, TokenExpires: f.Token.ExpiresAt,
		ForkedAt: time.Now().UTC().Format(time.RFC3339), Overlaps: f.Overlaps, Warnings: []string{},
	}
	if res.Overlaps == nil {
		res.Overlaps = []api.Overlap{}
	}
	tg := gitx.Git{Token: f.Token.Value} // in memory only
	if err := tg.Clone(ctx, f.Fork.Remote, dir); err != nil {
		return nil, err
	}
	g := gitx.Git{Dir: dir}
	tg.Dir = dir

	// Unverified on real Artifacts whether a fork keeps refs/notes/*; say
	// which it was instead of assuming.
	err := notes.Fetch(ctx, tg, f.Fork.Remote)
	var ge *gitx.Error
	switch {
	case err == nil:
		res.NotesFetched = true
	case errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref"):
		// Reported as notes_fetched: false; earlier why-notes stay on the trunk.
	default:
		res.Warnings = append(res.Warnings, "fetching "+notes.Ref+" failed: "+err.Error())
	}

	head, err := g.Head(ctx)
	if err != nil {
		return nil, err
	}
	res.Head = head
	if res.SnapshotHead != "" && head != res.SnapshotHead {
		res.Moved = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("the source fork moved after the share: snapshot %s, you have %s (its latest)", short(res.SnapshotHead), short(head)))
	}

	// Measure this intent's changes from its base: the source's head when
	// the Worker stacked it there (the usual case), so push and propose cover
	// only the work done here; the source's own base when it had nothing
	// pushed yet.
	res.Base = head
	if b := ptrOr(f.Intent.BaseCommit, "", nil); b != "" {
		if sha, ok, err := g.Resolve(ctx, b+"^{commit}"); err == nil && ok {
			res.Base = sha
		}
	}
	if _, err := g.Run(ctx, "update-ref", baseRef(f.Intent.ID), res.Base); err != nil {
		return nil, err
	}

	w := &workspace{root: dir, git: g}
	if _, err := os.Stat(w.path(configFile)); errors.Is(err, fs.ErrNotExist) {
		w.cfg = projectConfig{Version: 1, Project: ref.Project}
		if err := w.saveConfig(); err != nil {
			return nil, err
		}
		res.Warnings = append(res.Warnings, "the fork had no "+filepath.Join(dirName, configFile)+"; wrote one for project "+ref.Project)
	}
	if err := w.setCurrentIntent(f.Intent.ID); err != nil {
		return nil, err
	}
	res.ContextFile = w.path(stateDir, sessionMDFile)
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := w.write(append(b, '\n'), stateDir, sessionJSONFile); err != nil {
		return nil, err
	}
	if err := w.write([]byte(sessionMarkdown(res.Session, res)), stateDir, sessionMDFile); err != nil {
		return nil, err
	}
	return res, nil
}

// nonFamily drops the expected overlap with the session's own parent or
// child from the human warning; --json keeps it, labelled by relation.
func nonFamily(ovs []api.Overlap) []api.Overlap {
	out := make([]api.Overlap, 0, len(ovs))
	for _, ov := range ovs {
		if ov.Relation == "" {
			out = append(out, ov)
		}
	}
	return out
}

func renderForked(out io.Writer, st styles, res *forkedSession) error {
	_, _ = lipgloss.Fprintf(out, "%s %s\n\n", st.title.Render("Forked session "+res.Session.ID), st.muted.Render("into "+res.Intent.ID+" (yours, as "+res.Intent.Agent+")"))
	row(out, st, "Continues", fmt.Sprintf("%s by %s (%s)", res.Parent.ID, res.Parent.Agent, res.Parent.Status))
	if p := deref(res.Intent.ParentID); p != "" {
		row(out, st, "Stacked", fmt.Sprintf("on %s at %s", p, short(res.Base))+st.muted.Render("; your change is only what you add, and it lands after "+p))
	}
	row(out, st, "Goal", res.Intent.Goal)
	row(out, st, "Why", res.Intent.Reason)
	code := short(res.Head)
	if res.Moved {
		code += st.attention.Render(" (moved since the share: snapshot " + short(res.SnapshotHead) + ")")
	} else {
		code += st.muted.Render(" (as shared)")
	}
	rel := relDir(res.Dir)
	row(out, st, "Code", rel+" at "+code)
	row(out, st, "Fork", res.Fork.Name+st.muted.Render(" (new; write token for it only, expires "+res.TokenExpires+")"))
	notesLine := "fetched " + notes.Ref
	if !res.NotesFetched {
		notesLine = st.muted.Render("none on the fork")
	}
	row(out, st, "Git notes", notesLine)
	row(out, st, "Handoff", noteSummary(res.Session.Notes))
	row(out, st, "Context", filepath.Join(rel, dirName, stateDir, sessionMDFile)+st.muted.Render(" (git-ignored; 'wayglass session show')"))
	_, err := lipgloss.Fprintf(out, "\n%s cd %s && wayglass session show\n", st.muted.Render("Next:"), rel)
	return err
}

// relDir shows dir relative to the working directory when it is below it.
func relDir(dir string) string {
	wd, err := os.Getwd()
	if err != nil {
		return dir
	}
	rel, err := filepath.Rel(wd, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return dir
	}
	return rel
}

// ---- show -------------------------------------------------------------------

func newSessionShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show [url]",
		Short: "Print the session context for the agent starting work here",
		Long: "Without a URL, prints the context 'wayglass session fork' saved in this\n" +
			"checkout (.wayglass/state/session.json). With a URL, fetches that session\n" +
			"from the Worker without forking it. Markdown for people and agents;\n" +
			"--json for the full record.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			o := a.output(cmd)
			if len(args) == 1 {
				ref, err := api.ParseSessionURL(args[0])
				if err != nil {
					return UsageError("%v", err)
				}
				c, err := a.client()
				if err != nil {
					return err
				}
				if !c.SameWorker(ref) {
					return errorf(CodeWrongWorker, ExitError, "session is on %s but you are logged in to %s", ref.Worker, c.BaseURL)
				}
				v, err := c.GetSession(ctx, ref.Project, ref.ID)
				if err != nil {
					return err
				}
				if v.Forks == nil {
					v.Forks = []api.Intent{}
				}
				return o.Emit(v, func(out io.Writer) error { return printMarkdown(out, o, sessionMarkdown(v.Session, nil)) })
			}
			w, err := openRepo(ctx)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(w.path(stateDir, sessionJSONFile))
			if errors.Is(err, fs.ErrNotExist) {
				return errorf(CodeNoSession, ExitError, "no forked session in this checkout; run 'wayglass session fork <url>' or pass a session URL")
			}
			if err != nil {
				return err
			}
			var res forkedSession
			if err := json.Unmarshal(b, &res); err != nil {
				return fmt.Errorf("%s: %w", w.path(stateDir, sessionJSONFile), err)
			}
			return o.Emit(res, func(out io.Writer) error { return printMarkdown(out, o, sessionMarkdown(res.Session, &res)) })
		},
	}
}

// printMarkdown renders through Glamour on a terminal; pipes and agents get
// the Markdown source, which reads fine as plain text.
func printMarkdown(out io.Writer, o Output, md string) error {
	if o.Interactive() {
		style := "light"
		if isDark(o) {
			style = "dark"
		}
		r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(100))
		if err == nil {
			if s, err := r.Render(md); err == nil {
				_, err := io.WriteString(out, s)
				return err
			}
		}
	}
	_, err := io.WriteString(out, md)
	return err
}

// sessionMarkdown is the context an agent reads at startup. local is nil
// when the session is only viewed, not forked here.
func sessionMarkdown(s api.Session, local *forkedSession) string {
	in := s.Snapshot.Intent
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	p("# Wayglass session %s\n\n", s.ID)
	if local != nil {
		p("You are continuing intent **%s** as **%s**. It was forked from intent %s (by %s) at %s.\n",
			local.Intent.ID, local.Intent.Agent, in.ID, in.Agent, local.ForkedAt)
		p("Every commit you make should end with the trailer `Wayglass-Intent: %s`.\n\n", local.Intent.ID)
	} else {
		p("Shared from intent %s (by %s). Continue it with `wayglass session fork <url>`.\n\n", in.ID, in.Agent)
	}
	p("Shared by %s at %s.\n\n", s.SharedBy, s.CreatedAt)

	p("## Goal\n\n%s\n\n## Why\n\n%s\n\n", in.Goal, in.Reason)

	p("## Paths\n\n")
	p("- Declared: %s\n", codeList(in.DeclaredPaths))
	p("- Changed so far: %s\n\n", codeList(in.ActualPaths))

	p("## Handoff notes from %s\n\n", s.SharedBy)
	if strings.TrimSpace(s.Notes) == "" {
		p("_No notes were left._\n\n")
	} else {
		p("%s\n\n", strings.TrimSpace(s.Notes))
	}

	p("## Decisions so far\n\n")
	if len(s.Snapshot.Decisions) == 0 {
		p("_None yet: the change has not been proposed._\n\n")
	}
	for _, d := range s.Snapshot.Decisions {
		rule := ptrOr(d.MatchedRule, "default", nil)
		p("- **%s** %s, %s (rule `%s`, commit %s): %s\n", d.ID, d.Action, d.Status, rule, ptrOr(d.CommitSHA, "?", short), d.Reason)
		for _, ap := range d.Approvals {
			p("  - %s by %s: %s\n", ap.Verdict, approverName(ap.By, ap.ClaimedBy, ap.IdentitySource), ap.Reason)
		}
	}
	if len(s.Snapshot.Decisions) > 0 {
		p("\n")
	}

	if len(s.Snapshot.Overlaps) > 0 {
		p("## Overlapping intents at the share\n\n")
		for _, ov := range s.Snapshot.Overlaps {
			pairs := make([]string, 0, len(ov.Paths))
			for _, pp := range ov.Paths {
				pairs = append(pairs, "`"+pp.Mine+"` ~ `"+pp.Theirs+"`")
			}
			p("- %s (%s, %s): %s. %s\n", ov.IntentID, ov.Agent, ov.Status, ov.Goal, strings.Join(pairs, ", "))
		}
		p("\n")
	}

	p("## Code\n\n")
	p("- Shared at fork head %s\n", ptrOr(s.HeadCommit, "(empty fork)", short))
	if local != nil {
		p("- Your checkout: `%s` at %s", local.Dir, short(local.Head))
		if local.Moved {
			p(" (the source pushed again after the share)")
		}
		p("\n- Your fork: %s (a new repo; the source fork is untouched)\n", local.Fork.Name)
		p("- Diff base (trunk): %s\n", short(local.Base))
		if local.NotesFetched {
			p("- Why-notes: `%s` fetched from the fork\n", notes.Ref)
		} else {
			p("- Why-notes: none on the fork\n")
		}
	}
	p("\n")

	if len(s.Snapshot.History) > 0 {
		p("## History\n\n")
		for _, e := range s.Snapshot.History {
			p("- %s %s by %s\n", e.At, e.Kind, e.Actor)
		}
		p("\n")
	}

	if local != nil {
		p("## Next\n\n")
		p("1. Read the notes above, then work in `%s`.\n", local.Dir)
		p("2. Commit with `Wayglass-Intent: %s`, then `wayglass push --json` (it mints a fresh token for your fork).\n", local.Intent.ID)
		p("3. `wayglass propose --json` when done. Policy decides where it goes; the parent intent %s is separate and may land on its own.\n", in.ID)
	}
	return b.String()
}

func codeList(items []string) string {
	if len(items) == 0 {
		return "_none_"
	}
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = "`" + s + "`"
	}
	return strings.Join(out, ", ")
}

func noteSummary(n string) string {
	n = strings.TrimSpace(n)
	if n == "" {
		return "none"
	}
	first, _, _ := strings.Cut(n, "\n")
	if lines := strings.Count(n, "\n") + 1; lines > 1 {
		return fmt.Sprintf("%s (+%d more lines)", truncate(first, 60), lines-1)
	}
	return truncate(first, 80)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ptrOr dereferences p, or returns def; f, if set, formats the value.
func ptrOr(p *string, def string, f func(string) string) string {
	if p == nil || *p == "" {
		return def
	}
	if f != nil {
		return f(*p)
	}
	return *p
}
