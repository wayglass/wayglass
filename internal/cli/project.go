package cli

import (
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

func newProjectCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "List projects; delete or move one (admin key only)",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  groupRunE(a),
	}
	cmd.AddCommand(newProjectListCmd(a))
	cmd.AddCommand(newProjectDeleteCmd(a))
	cmd.AddCommand(newProjectMigrateResidencyCmd(a))
	return cmd
}

func newProjectMigrateResidencyCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-residency <name>",
		Short: "Move a project's Worker state into the Durable Object pinned to its jurisdiction",
		Long: "A project made before Oct 14, 2026 in a jurisdiction Durable Objects can pin to\n" +
			"(eu) keeps its repos in that jurisdiction's namespace, but its Worker state\n" +
			"(intents, decisions, audit log, sessions, cost meters) was created wherever it\n" +
			"was first reached. Until it is moved the Worker refuses the project with\n" +
			"residency_pending rather than serve it from the wrong place.\n\n" +
			"This copies every table row for row into the pinned object, verifies row\n" +
			"counts and a digest on both sides, wipes the old object and leaves a tombstone\n" +
			"that refuses every call, and appends project.residency_migrated to the audit\n" +
			"log. Safe to rerun: a finished move reports migrated: false. Needs the admin key.",
		Example: "  wayglass project migrate-residency eu-demo-1791797572\n" +
			"  wayglass project migrate-residency eu-demo-1791797572 --json",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			o := a.output(cmd)
			c, err := a.client()
			if err != nil {
				return err
			}
			m, err := c.MigrateResidency(cmd.Context(), name)
			if err != nil {
				return err
			}
			return o.Emit(m, func(out io.Writer) error {
				st := cliStyles(o)
				if !m.Migrated {
					_, err := lipgloss.Fprintln(out, st.muted.Render(fmt.Sprintf("%s already lives in the %s jurisdiction; nothing moved.", name, m.Jurisdiction)))
					return err
				}
				_, _ = lipgloss.Fprintf(out, "%s %s\n", st.title.Render("Moved "+name), st.muted.Render("to the "+m.Jurisdiction+" jurisdiction"))
				rows := 0
				for _, n := range m.Counts {
					rows += n
				}
				_, _ = fmt.Fprintf(out, "  %d rows in %d tables copied and verified (digest %s)\n", rows, len(m.Counts), m.Digest[:min(12, len(m.Digest))])
				if m.SourceTombstoned {
					_, _ = fmt.Fprintln(out, "  the old object was wiped and refuses every call")
				}
				return nil
			})
		},
	}
}

func newProjectDeleteCmd(a *app) *cobra.Command {
	var confirm string
	cmd := &cobra.Command{
		Use:   "delete <name> --confirm <name>",
		Short: "Delete a throwaway project: its repos, tokens and Worker state",
		Long: "Deletes the trunk and every fork repo the project recorded, revokes its live\n" +
			"tokens, removes its trunk push subscription and wipes its intents, decisions\n" +
			"and audit log on the Worker. The Worker logs the deletion.\n\n" +
			"Only throwaway projects can be deleted: names starting with " + strings.Join(api.DeletablePrefixes, " or ") + ".\n" +
			"Every other project keeps its repos and audit log. Needs the admin key.\n" +
			"Running it again on a deleted project succeeds with existed: false.",
		Example: "  wayglass project delete demo-test-1 --confirm demo-test-1\n" +
			"  wayglass project delete eu-demo-test-2 --confirm eu-demo-test-2 --json",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !api.IsDeletableProject(name) {
				return UsageError("only throwaway projects can be deleted (names starting with %s); %s keeps its repos and audit log",
					strings.Join(api.DeletablePrefixes, " or "), name)
			}
			o := a.output(cmd)
			if confirm == "" {
				// Agents and pipes never get a prompt: they must say --confirm.
				if !o.Interactive() {
					return UsageError("--confirm %s is required", name)
				}
				err := huh.NewInput().
					Title(fmt.Sprintf("Delete %s and all its repos? Type its name to confirm", name)).
					Value(&confirm).
					Run()
				if err != nil {
					return &Error{Code: CodeCanceled, Message: "canceled; nothing was deleted", Exit: ExitError}
				}
				confirm = strings.TrimSpace(confirm)
			}
			if confirm != name {
				return UsageError("--confirm must repeat the project name %q; nothing was deleted", name)
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			d, err := c.DeleteProject(cmd.Context(), name, confirm)
			if err != nil {
				return err
			}
			return o.Emit(d, func(out io.Writer) error {
				st := cliStyles(o)
				if !d.Existed {
					_, err := lipgloss.Fprintln(out, st.muted.Render(fmt.Sprintf("No project %s on the Worker; nothing to delete.", name)))
					return err
				}
				_, _ = lipgloss.Fprintf(out, "%s %s\n", st.title.Render("Deleted "+name), st.muted.Render("("+d.Namespace+")"))
				_, _ = fmt.Fprintf(out, "  repos deleted: %d, already gone: %d, tokens revoked: %d\n", len(d.ReposDeleted), len(d.ReposAlreadyGone), d.TokensRevoked)
				if len(d.SubscriptionsLeft) > 0 {
					o.Warn("push subscriptions not removed: %s", strings.Join(d.SubscriptionsLeft, ", "))
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "the project name again, to confirm (required without a terminal)")
	return cmd
}
