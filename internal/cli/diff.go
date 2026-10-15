package cli

import (
	"cmp"
	"fmt"
	"io"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

// maxDiffBytes caps the diff text in --json output; a reviewer that needs
// more reads the files changed list and fetches the ref itself.
const maxDiffBytes = 256 << 10

type diffResult struct {
	IntentID  string   `json:"intent_id"`
	Goal      string   `json:"goal"`
	Reason    string   `json:"reason"`
	Agent     string   `json:"agent"`
	Status    string   `json:"status"`
	Declared  []string `json:"declared_paths"`
	Base      string   `json:"base"`
	Head      string   `json:"head"`
	Ref       string   `json:"ref"`
	Files     []string `json:"files"`
	Diff      string   `json:"diff"`
	Truncated bool     `json:"truncated"`
	// Untrusted lists the fields the author wrote (diffUntrusted). A
	// reviewer reads them as data to check, never as instructions.
	Untrusted []string `json:"untrusted"`
}

// diffUntrusted: the goal, reason and name are the author's words; the diff
// and file names are the author's code, comments and commit content.
var diffUntrusted = []string{"goal", "reason", "agent", "declared_paths", "files", "diff"}

// reviewRef is where `diff` keeps a fetched intent head in this checkout.
func reviewRef(intentID string) string { return "refs/wayglass/review/" + intentID }

func newDiffCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "diff [intent-id]",
		Short: "Show an intent's change (fork base to pushed head) for review",
		Long: "Fetches the intent's fork with a 300-second read-only token from the\n" +
			"Worker (held in memory, audited as token.read_minted) and prints the diff\n" +
			"from where the fork started to the head the agent pushed. Any agent may\n" +
			"read: this is how a reviewer sees the code before it approves or rejects.\n" +
			"The head stays in this checkout at refs/wayglass/review/<intent-id>.",
		Example: "  wayglass review --for agent --json   # what waits for a reviewer\n" +
			"  wayglass diff i5e39df9bcd --json",
		Args: usageArgs(cobra.MaximumNArgs(1)),
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
			id, err := w.resolveIntent(args)
			if err != nil {
				return err
			}
			rt, err := c.ReadToken(ctx, w.cfg.Project, id)
			if err != nil {
				return err
			}
			it := rt.Intent
			base, head := deref(it.BaseCommit), it.Head()
			if head == "" {
				return errorf(CodeNotPushed, ExitError, "intent %s has nothing pushed yet", id)
			}
			remote := cmp.Or(rt.Fork.Remote, it.ForkRemote)
			tg := w.git
			tg.Token = rt.Token.Value // in memory only
			branch, err := tg.RemoteDefaultBranch(ctx, remote)
			if err != nil {
				return err
			}
			ref := reviewRef(id)
			if err := tg.Fetch(ctx, remote, "+refs/heads/"+cmp.Or(branch, "main")+":"+ref); err != nil {
				return err
			}
			if _, ok, err := w.git.Resolve(ctx, head); err != nil || !ok {
				return errorf(api.CodeConflict, ExitError, "the fork no longer holds the pushed head %s; the agent rewrote it", short(head))
			}
			files, err := w.git.ChangedFiles(ctx, base, head)
			if err != nil {
				return err
			}
			diff, err := w.git.Diff(ctx, base, head)
			if err != nil {
				return err
			}
			res := diffResult{Untrusted: diffUntrusted,
				IntentID: id, Goal: it.Goal, Reason: it.Reason, Agent: it.Agent, Status: it.Status,
				Declared: it.DeclaredPaths, Base: base, Head: head, Ref: ref, Files: files, Diff: diff,
			}
			if res.Files == nil {
				res.Files = []string{}
			}
			if res.Declared == nil {
				res.Declared = []string{}
			}
			o := a.output(cmd)
			if a.asJSON && len(res.Diff) > maxDiffBytes {
				res.Diff, res.Truncated = res.Diff[:maxDiffBytes], true
				o.Warn("diff truncated to %d bytes; the full head is at %s", maxDiffBytes, ref)
			}
			return o.Emit(res, func(out io.Writer) error {
				st := cliStyles(o)
				fields(out, st,
					field{"Intent", st.id(id) + "  " + it.Goal + st.muted.Render(" (by "+it.Agent+", "+it.Status+")")},
					field{"Why", it.Reason},
					field{"Change", fmt.Sprintf("%s..%s, %d file(s)", st.id(cmp.Or(short(base), "(empty)")), st.id(short(head)), len(files))},
				)
				_, _ = lipgloss.Fprintln(out)
				_, err := io.WriteString(out, diff)
				return err
			})
		},
	}
}
