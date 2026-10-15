package cli

import (
	"cmp"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

func newKeysCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Mint, list and revoke per-agent keys (admin key only)",
		Long: "An agent key is bound to one name: everything done with it is recorded\n" +
			"under that name, whatever X-Wayglass-Agent or WAYGLASS_AGENT say, so an\n" +
			"agent cannot act or approve as someone else. The Worker stores only a hash.\n" +
			"Only the admin key can manage keys; the admin key keeps working.\n\n" +
			"Keys are agent keys unless minted with --human. Only a person (the admin\n" +
			"key or a human key) can decide a change policy routed to a person.",
		Args: usageArgs(cobra.NoArgs),
		RunE: groupRunE(a),
	}
	cmd.AddCommand(newKeysCreateCmd(a), newKeysListCmd(a), newKeysRevokeCmd(a))
	return cmd
}

func newKeysCreateCmd(a *app) *cobra.Command {
	var agent, note string
	var human bool
	cmd := &cobra.Command{
		Use:   "create --agent <name>",
		Short: "Mint a key bound to an agent or person; the key is shown once",
		Example: "  wayglass keys create --agent claude-ci --note \"nightly runs\"\n" +
			"  wayglass keys create --agent alice@laptop --human --json | jq -r .data.key",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			agent = strings.TrimSpace(agent)
			if agent == "" {
				return UsageError("--agent is required")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			kind := api.KindAgent
			if human {
				kind = api.KindHuman
			}
			nk, err := c.CreateAgentKey(cmd.Context(), agent, note, kind)
			if err != nil {
				return err
			}
			o := a.output(cmd)
			return o.Emit(nk, func(out io.Writer) error {
				st := cliStyles(o)
				_, _ = lipgloss.Fprintf(out, "%s %s\n\n", st.title.Render("Key "+nk.Info.ID), st.muted.Render("bound to "+nk.Info.Agent+", "+keyKind(nk.Info.Kind)))
				_, _ = fmt.Fprintf(out, "  %s\n\n", nk.Key)
				_, _ = lipgloss.Fprintln(out, st.muted.Render("Shown once; the Worker keeps only a hash. On the agent's machine:"))
				_, err := fmt.Fprintf(out, "  printf %%s \"$KEY\" | wayglass login --worker %s --key-stdin\n", c.BaseURL)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "name the key acts as in the audit log (letters, digits, . _ @ + -)")
	cmd.Flags().StringVar(&note, "note", "", "what the key is for (shown in 'keys list')")
	cmd.Flags().BoolVar(&human, "human", false, "the key belongs to a person, who may decide changes policy routed to a person")
	return cmd
}

// keyKind names a key's kind for people; keys from older Workers are agents.
func keyKind(kind string) string {
	if kind == api.KindHuman {
		return "person"
	}
	return "agent"
}

func newKeysListCmd(a *app) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agent keys (never the keys themselves)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			keys, err := c.ListAgentKeys(cmd.Context(), all)
			if err != nil {
				return err
			}
			o := a.output(cmd)
			return o.Emit(map[string]any{"keys": keys}, func(out io.Writer) error {
				st := cliStyles(o)
				if len(keys) == 0 {
					_, err := lipgloss.Fprintln(out, st.muted.Render("No agent keys. Mint one with 'wayglass keys create --agent <name>'."))
					return err
				}
				for _, k := range keys {
					state := "used " + cmp.Or(deref(k.LastUsedAt), "never")
					if k.RevokedAt != nil {
						state = "revoked " + *k.RevokedAt
					}
					line := fmt.Sprintf("%s  %-24s  %-6s  %s", k.ID, k.Agent, keyKind(k.Kind), st.muted.Render("created "+k.CreatedAt+" by "+k.CreatedBy+", "+state))
					if k.Note != "" {
						line += st.muted.Render("  " + k.Note)
					}
					_, _ = lipgloss.Fprintln(out, line)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include revoked keys")
	return cmd
}

func newKeysRevokeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an agent key; it stops working on its next request",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			k, err := c.RevokeAgentKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.output(cmd).Emit(k, func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "Revoked %s (%s).\n", k.ID, k.Agent)
				return err
			})
		},
	}
}
