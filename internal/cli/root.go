// Package cli builds the wayglass command tree. Every command supports
// --json and reports through Output, so agents always get the same envelope
// and exit codes.
package cli

import (
	"context"
	"io"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// BuildInfo is stamped into the binary at link time.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
}

// app is the state shared by one execution of the command tree.
type app struct {
	info   BuildInfo
	asJSON bool
}

func (a *app) output(cmd *cobra.Command) Output {
	return Output{JSON: a.asJSON, Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
}

// NewRoot builds a fresh command tree. Build a new one per execution so flag
// state never leaks between runs (or tests).
func NewRoot(info BuildInfo) *cobra.Command {
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Go == "" {
		info.Go = runtime.Version()
	}
	a := &app{info: info}

	root := &cobra.Command{
		Use:   "wayglass",
		Short: "Zero Trust for agentic development",
		Long: "Put coding agents to work in parallel under your policy.\n\n" +
			"You direct the work and set the policy. Agents declare what they intend to\n" +
			"change and why. Wayglass classifies each proposal and applies your rules\n" +
			"to determine whether it can proceed, needs review, or must be held or\n" +
			"blocked. The intent, decision, and reason are recorded in an audit log,\n" +
			"and wayglass why shows them for any line of landed code.\n\n" +
			"Trust is evaluated for each proposed change.\n\n" +
			"Every command takes --json for agents. Exit codes: 0 ok, 1 error, 2 usage,\n" +
			"3 blocked by policy, 4 needs a human.",
		Version:       info.Version,
		Args:          usageArgs(cobra.NoArgs),
		SilenceErrors: true, // Execute reports errors once, as JSON or text
		SilenceUsage:  true,
		RunE:          groupRunE(a),
	}
	root.SetVersionTemplate("wayglass {{.Version}}\n")
	root.PersistentFlags().BoolVar(&a.asJSON, "json", false, "print a JSON envelope for agents (no colour, prompts or spinners)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError("%s", err.Error())
	})
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(
		newVersionCmd(a),
		newInitCmd(a),
		newLoginCmd(a),
		newIntentCmd(a),
		newPushCmd(a),
		newSyncCmd(a),
		newOverlapsCmd(a),
		newProposeCmd(a),
		newReviewCmd(a),
		newDiffCmd(a),
		newApproveCmd(a),
		newRejectCmd(a),
		newAIReviewCmd(a),
		newAIAdviceCmd(a),
		newLandCmd(a),
		newPublishCmd(a),
		newWhyCmd(a),
		newPolicyCmd(a),
		newLogCmd(a),
		newWatchCmd(a),
		newSessionCmd(a),
		newCloneCmd(a),
		newGitCredentialCmd(a),
		newGenDocsCmd(a),
		newKeysCmd(a),
		newDoctorCmd(a),
		newAgentCmd(a),
		newProjectCmd(a),
		newProjectsCmd(a),
	)
	return root
}

// Execute runs the CLI and returns the process exit code. It is the single
// place that prints errors.
func Execute(ctx context.Context, info BuildInfo, args []string, stdout, stderr io.Writer) int {
	root := NewRoot(info)
	root.SetArgs(args)
	stdout, stderr = safeStream(stdout), safeStream(stderr)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	return report(err, jsonRequested(root, args), stdout, stderr)
}

// jsonRequested decides the error format even when flag parsing failed
// before --json was applied.
func jsonRequested(root *cobra.Command, args []string) bool {
	if f := root.PersistentFlags().Lookup("json"); f != nil && f.Value.String() == "true" {
		return true
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			return true
		}
		if v, ok := strings.CutPrefix(arg, "--json="); ok {
			b, err := strconv.ParseBool(v)
			return err == nil && b
		}
	}
	return false
}

// usageArgs makes positional-argument errors usage errors (exit 2).
func usageArgs(fn cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := fn(cmd, args); err != nil {
			return UsageError("%s", err.Error())
		}
		return nil
	}
}

// groupRunE is the RunE of a command that only groups subcommands: help for
// people, a usage error for agents.
func groupRunE(a *app) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if a.asJSON {
			return UsageError("%s needs a subcommand; see '%s --help'", cmd.CommandPath(), cmd.CommandPath())
		}
		return cmd.Help()
	}
}
