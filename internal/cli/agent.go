package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Markers around the block `wayglass agent setup` owns in AGENTS.md. Text
// outside them is never touched, so the command is safe to rerun.
const (
	agentBlockBegin = "<!-- wayglass:begin -->"
	agentBlockEnd   = "<!-- wayglass:end -->"
)

// CodeAgentBlock means the instructions file has a broken wayglass block.
const CodeAgentBlock = "agent_block_invalid"

// agentBlock is the loop every coding agent follows in a Wayglass repo. Keep
// it short: it is read by agents at every session start. Flags must match
// the real commands; TestAgentBlockFlagsExist checks them.
const agentBlock = agentBlockBegin + `
## Wayglass: how changes land in this repo

Every change goes through Wayglass policy. Pass ` + "`--json`" + ` to every ` + "`wayglass`" + ` command and branch on its exit code; it never prompts.

1. **At startup**, set ` + "`WAYGLASS_AGENT_FAMILY`" + ` to your model family (` + "`claude`" + `, ` + "`openai`" + `, ` + "`gemini`" + ` or ` + "`other`" + `) and run ` + "`wayglass session show --json`" + `. If it returns a session, continue that intent with its notes instead of declaring a new one.
2. **Declare before you code:** ` + "`wayglass intent declare --goal \"...\" --reason \"...\" --path \"<file or glob>\" --json`" + ` (one ` + "`--path`" + ` per file or glob you expect to touch). Read ` + "`overlaps`" + ` in the reply.
3. **Commit with the why:** the message says why, not only what, and ends with the trailer ` + "`Wayglass-Intent: <intent-id>`" + `. ` + "`wayglass push -m \"...\"`" + ` commits everything with the trailer for you.
4. **Push:** ` + "`wayglass push --json`" + `. If ` + "`overlaps`" + ` is not empty, follow each ` + "`replan`" + ` suggestion (land first, stack with ` + "`intent declare --on <id>`" + `, or split) before going on.
5. **Propose:** ` + "`wayglass propose --json`" + `, then obey the exit code. ` + "`0`" + `: may land once ` + "`status`" + ` is ` + "`approved`" + ` (` + "`pending_agent`" + ` waits for a reviewer agent). ` + "`3`" + `: blocked; change the code, do not retry it as is. ` + "`4`" + `: a person must act; stop and tell your user. ` + "`next`" + ` says what happens now.
6. **Check approval** with ` + "`wayglass watch --json`" + `: find this intent's latest entry in ` + "`data.decisions`" + ` and read its ` + "`status`" + `. Do not rerun ` + "`propose`" + ` to poll: it records a new decision and replaces the approval. **Land** only an approved change: ` + "`wayglass land --json`" + `. If it stops with ` + "`policy_changed`" + ` or ` + "`land_conflict`" + `, run ` + "`wayglass sync --json`" + ` (merges the trunk in), resolve any ` + "`conflicts`" + ` and commit, then push and propose again.
7. **Never approve your own work**, and never approve a change policy routed to a person (an agent key gets ` + "`needs_human`" + `, exit 4: leave it to the person).
8. **Reviewer agents:** ` + "`wayglass review --for agent --json`" + ` lists ` + "`agent_review`" + ` changes. Read the proposed commit with ` + "`wayglass diff <intent-id> --json`" + `, which fetches the fork with a read-only token; plain git fetch of another intent's URL has no credentials. Test that commit and decide one you did not author with ` + "`wayglass approve <intent-id> --reason \"what you checked\" --json`" + ` (or ` + "`reject`" + `). ` + "`same_family`" + ` or ` + "`family_required`" + ` means policy wants a reviewer from another model family: leave it to one.

Exit codes: 0 ok, 1 error, 2 usage, 3 blocked by policy, 4 needs a person. Errors arrive as ` + "`{\"ok\":false,\"error\":{\"code\":...}}`" + `.

The full loop and reviewer duty are also agent skills: ` + "`npx skills add wayglass/wayglass-skill`" + `.
` + agentBlockEnd + "\n"

type agentSetupResult struct {
	File    string `json:"file"`
	Action  string `json:"action"` // created, appended, updated, unchanged, printed
	Block   string `json:"block,omitempty"`
	Warning string `json:"warning,omitempty"`
}

func newAgentCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Set up coding agents to work through Wayglass",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  groupRunE(a),
	}
	cmd.AddCommand(newAgentSetupCmd(a))
	return cmd
}

func newAgentSetupCmd(a *app) *cobra.Command {
	var file string
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Teach coding agents the Wayglass loop in AGENTS.md",
		Long: "Writes a short block between " + agentBlockBegin + " and " + agentBlockEnd + "\n" +
			"into the repo's AGENTS.md (or --file): declare an intent, commit with the why,\n" +
			"push, propose and obey the exit code, land only when approved, never approve\n" +
			"your own work. Rerunning replaces only that block. Nothing is committed.",
		Example: "  wayglass agent setup\n  wayglass agent setup --file CLAUDE.md\n  wayglass agent setup --print >> notes.md",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			o := a.output(cmd)
			if printOnly {
				return o.Emit(agentSetupResult{Action: "printed", Block: agentBlock}, func(out io.Writer) error {
					_, err := io.WriteString(out, agentBlock)
					return err
				})
			}
			w, err := openRepo(cmd.Context())
			if err != nil {
				return err
			}
			path := file
			if !filepath.IsAbs(path) {
				path = filepath.Join(w.root, path)
			}
			res, err := writeAgentBlock(path)
			if err != nil {
				return err
			}
			if rel, err := filepath.Rel(w.root, path); err == nil {
				res.File = rel
			}
			if filepath.Base(path) == "AGENTS.md" {
				res.Warning = claudeMDWarning(w.root)
			}
			if res.Warning != "" {
				o.Warn("%s", res.Warning)
			}
			return o.Emit(res, func(out io.Writer) error {
				var err error
				switch res.Action {
				case "unchanged":
					_, err = fmt.Fprintf(out, "%s already has the current Wayglass block.\n", res.File)
				default:
					_, err = fmt.Fprintf(out, "Wrote the Wayglass block to %s (%s). Commit it so every agent reads it.\n", res.File, res.Action)
				}
				return err
			})
		},
	}
	cmd.Flags().StringVar(&file, "file", "AGENTS.md", "instructions file to update, relative to the repo root (e.g. CLAUDE.md)")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the block instead of writing it")
	return cmd
}

// writeAgentBlock creates the file, appends the block, or replaces the
// existing block in place.
func writeAgentBlock(path string) (agentSetupResult, error) {
	res := agentSetupResult{File: path}
	old, err := os.ReadFile(path) //nolint:gosec // the user's own instructions file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		res.Action = "created"
		return res, writeFileNoFollow(path, []byte(agentBlock))
	case err != nil:
		return res, err
	}
	updated, action, err := spliceAgentBlock(old)
	if err != nil {
		return res, errorf(CodeAgentBlock, ExitError, "%s: %v", path, err)
	}
	res.Action = action
	if action == "unchanged" {
		return res, nil
	}
	return res, writeFileNoFollow(path, updated)
}

// spliceAgentBlock returns doc with the current block in place of any old
// one, or appended after a blank line if there is none.
func spliceAgentBlock(doc []byte) ([]byte, string, error) {
	begin := bytes.Index(doc, []byte(agentBlockBegin))
	end := bytes.Index(doc, []byte(agentBlockEnd))
	switch {
	case begin < 0 && end < 0:
		var b bytes.Buffer
		b.Write(doc)
		if len(doc) > 0 {
			if !bytes.HasSuffix(doc, []byte("\n")) {
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}
		b.WriteString(agentBlock)
		return b.Bytes(), "appended", nil
	case begin < 0 || end < 0 || end < begin:
		return nil, "", fmt.Errorf("found only one of %s and %s; fix or remove the block by hand", agentBlockBegin, agentBlockEnd)
	case bytes.Count(doc, []byte(agentBlockBegin)) > 1:
		return nil, "", fmt.Errorf("more than one %s; keep one block", agentBlockBegin)
	}
	end += len(agentBlockEnd)
	if end < len(doc) && doc[end] == '\n' {
		end++
	}
	if string(doc[begin:end]) == agentBlock {
		return doc, "unchanged", nil
	}
	var b bytes.Buffer
	b.Write(doc[:begin])
	b.WriteString(agentBlock)
	b.Write(doc[end:])
	return b.Bytes(), "updated", nil
}

// claudeMDWarning notes that Claude Code reads CLAUDE.md, not AGENTS.md,
// when a CLAUDE.md exists that does not import AGENTS.md.
func claudeMDWarning(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "CLAUDE.md")) //nolint:gosec // the user's own instructions file
	if err != nil || strings.Contains(string(b), "AGENTS.md") {
		return ""
	}
	return "CLAUDE.md exists and does not mention AGENTS.md; Claude Code reads only CLAUDE.md, so add a line '@AGENTS.md' to it or rerun with --file CLAUDE.md"
}
