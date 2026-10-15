package cli

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

type loginResult struct {
	URL      string `json:"url"`
	Agent    string `json:"agent,omitempty"`
	Path     string `json:"path"`
	Verified bool   `json:"verified"`
	// Role is "admin" or "agent" when the Worker said (GET /v1/whoami).
	Role string `json:"role,omitempty"`
	// Bound means the key fixes the agent name; Agent is that name.
	Bound bool `json:"bound"`
	// Kind, Source and ExpiresAt describe a human session from --human.
	Kind      string   `json:"kind,omitempty"`
	Source    string   `json:"source,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Warnings  []string `json:"warnings"`
}

func newLoginCmd(a *app) *cobra.Command {
	var worker, agent string
	var keyStdin, noVerify, human, noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store the Worker URL and your Wayglass key, or sign in as a person",
		Long: "Stores the Worker URL and key in ~/.config/wayglass/credentials.yaml (mode 0600).\n" +
			"The key is the Worker's admin key or an agent key from 'wayglass keys create'; an\n" +
			"agent key fixes the name the audit log records for you.\n" +
			"The key is read from WAYGLASS_KEY or, with --key-stdin, from standard input, never\n" +
			"from a flag, so it stays out of shell history and the process list.\n" +
			"WAYGLASS_URL and WAYGLASS_KEY override the stored values on every command.\n\n" +
			"With --human, a person signs in through Cloudflare Access instead: the browser\n" +
			"opens the Worker's Access-protected sign-in page, and the Worker hands back a\n" +
			"human key bound to your email that expires in 12 hours. The key comes back to a\n" +
			"one-time listener on 127.0.0.1 in a POST body, never in a URL. The URL is also\n" +
			"printed, for machines without a browser. Agents keep using keys; the Worker's\n" +
			"admin turns Access on (docs/SELF-HOSTING.md).",
		Example: "  printf %s \"$KEY\" | wayglass login --worker https://wayglass.<you>.workers.dev --key-stdin\n" +
			"  wayglass login --human --worker https://wayglass.<you>.workers.dev",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			worker = strings.TrimRight(cmp.Or(worker, os.Getenv(api.EnvURL)), "/")
			if worker == "" && human {
				if c, err := api.LoadCredentials(); err == nil {
					worker = c.URL
				}
			}
			if worker == "" {
				return UsageError("--worker is required (or set %s)", api.EnvURL)
			}
			if err := api.ValidateURL(worker); err != nil {
				return UsageError("%v", err)
			}
			if human {
				if keyStdin {
					return UsageError("--human signs in through the browser; it takes no --key-stdin")
				}
				return runHumanLogin(cmd.Context(), a.output(cmd), worker, noBrowser)
			}
			key := os.Getenv(api.EnvKey)
			if keyStdin {
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				key = strings.TrimSpace(line)
			}
			if key == "" {
				return UsageError("no key: pipe it with --key-stdin or set %s", api.EnvKey)
			}
			res := loginResult{URL: worker, Agent: agent, Warnings: []string{}}
			o := a.output(cmd)
			if !noVerify {
				c := api.New(worker, key)
				who, err := c.CheckAuth(cmd.Context())
				if err != nil {
					return err
				}
				res.Verified = true
				if who != nil {
					res.Role, res.Bound = who.Role, who.Bound
					// An agent key's name is fixed by the Worker; record it so
					// the CLI never claims a different one.
					if who.Bound {
						if agent != "" && agent != who.Agent {
							res.Warnings = append(res.Warnings, fmt.Sprintf("--agent %q ignored: this key is bound to %q", agent, who.Agent))
						}
						agent, res.Agent = who.Agent, who.Agent
					}
				}
			}
			for _, w := range res.Warnings {
				o.Warn("%s", w)
			}
			p, err := api.SaveCredentials(api.Credentials{URL: worker, Key: key, Agent: agent})
			if err != nil {
				return err
			}
			res.Path = p
			return o.Emit(res, func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "Logged in to %s. Saved to %s.\n", worker, p)
				if res.Bound {
					fmt.Fprintf(out, "This key acts as %s; the Worker records that name on everything it does.\n", res.Agent)
				}
				return err
			})
		},
	}
	cmd.Flags().StringVar(&worker, "worker", "", "Wayglass Worker URL (https)")
	cmd.Flags().StringVar(&agent, "agent", "", "name recorded in the audit log for this machine (default user@host)")
	cmd.Flags().BoolVar(&keyStdin, "key-stdin", false, "read the key from standard input")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "store without checking the key against the Worker")
	cmd.Flags().BoolVar(&human, "human", false, "sign in as a person through Cloudflare Access (opens a browser; the key expires)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "with --human: only print the sign-in URL")
	return cmd
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
