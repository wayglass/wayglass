package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/wayglass/wayglass/internal/gitx"
)

func TestAgentBlockGolden(t *testing.T) {
	golden.RequireEqual(t, []byte(agentBlock))
}

// TestAgentBlockFlagsExist keeps the block honest: every `wayglass ...`
// command it shows must exist with the flags it uses.
func TestAgentBlockFlagsExist(t *testing.T) {
	root := NewRoot(BuildInfo{})
	spans := regexp.MustCompile("`(wayglass [^`]+)`").FindAllStringSubmatch(agentBlock, -1)
	if len(spans) < 8 {
		t.Fatalf("found only %d commands in the block", len(spans))
	}
	for _, m := range spans {
		words := strings.Fields(m[1])[1:]
		cmd := root
		for len(words) > 0 && !strings.HasPrefix(words[0], "-") {
			next, _, err := cmd.Find(words[:1])
			if err != nil || next == cmd {
				break
			}
			cmd, words = next, words[1:]
		}
		if cmd == root {
			t.Errorf("%q: no such command", m[1])
			continue
		}
		for _, w := range words {
			if !strings.HasPrefix(w, "-") {
				continue
			}
			name := strings.TrimLeft(strings.SplitN(w, "=", 2)[0], "-")
			var found bool
			if strings.HasPrefix(w, "--") {
				found = cmd.Flags().Lookup(name) != nil || cmd.InheritedFlags().Lookup(name) != nil
			} else {
				found = cmd.Flags().ShorthandLookup(name) != nil
			}
			if !found {
				t.Errorf("%q: %s has no flag %s", m[1], cmd.CommandPath(), w)
			}
		}
	}
}

func TestSpliceAgentBlock(t *testing.T) {
	stale := agentBlockBegin + "\nold loop\n" + agentBlockEnd + "\n"
	cases := []struct {
		name, in, want, action string
	}{
		{"empty", "", agentBlock, "appended"},
		{"append", "# Repo\n\nRules.", "# Repo\n\nRules.\n\n" + agentBlock, "appended"},
		{"append after newline", "# Repo\n", "# Repo\n\n" + agentBlock, "appended"},
		{"replace in middle", "# Repo\n\n" + stale + "\n## More\n", "# Repo\n\n" + agentBlock + "\n## More\n", "updated"},
		{"replace at end without newline", "# Repo\n\n" + strings.TrimSuffix(stale, "\n"), "# Repo\n\n" + agentBlock, "updated"},
		{"unchanged", "# Repo\n\n" + agentBlock + "tail\n", "# Repo\n\n" + agentBlock + "tail\n", "unchanged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, action, err := spliceAgentBlock([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if action != tc.action || string(got) != tc.want {
				t.Fatalf("action=%s got=%q, want %s %q", action, got, tc.action, tc.want)
			}
			// Idempotent: a second run changes nothing.
			again, action, err := spliceAgentBlock(got)
			if err != nil || action != "unchanged" || string(again) != string(got) {
				t.Fatalf("second run: action=%s err=%v", action, err)
			}
		})
	}
	for _, bad := range []string{
		agentBlockBegin + "\nno end\n",
		"no begin\n" + agentBlockEnd,
		agentBlockEnd + "\n" + agentBlockBegin,
		agentBlock + agentBlock,
	} {
		if _, _, err := spliceAgentBlock([]byte(bad)); err == nil {
			t.Errorf("want an error for %q", bad)
		}
	}
}

func TestAgentSetupCommand(t *testing.T) {
	hermetic(t)
	dir, _ := os.Getwd()
	if _, err := (gitx.Git{Dir: dir}).Run(t.Context(), "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"created", "unchanged"} {
		exit, stdout, stderr := run(t, "agent", "setup", "--json")
		env := checkEnvelope(t, stdout)
		if exit != ExitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q stdout=%s", exit, stderr, stdout)
		}
		var res agentSetupResult
		if err := json.Unmarshal(*env.Data, &res); err != nil {
			t.Fatal(err)
		}
		if res.Action != want || res.File != "AGENTS.md" || !strings.Contains(res.Warning, "CLAUDE.md") {
			t.Fatalf("result = %+v, want action %s", res, want)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil || string(b) != agentBlock {
		t.Fatalf("AGENTS.md = %q, %v", b, err)
	}

	exit, stdout, _ := run(t, "agent", "setup", "--file", "CLAUDE.md", "--json")
	if exit != ExitOK || !strings.Contains(stdout, `"appended"`) {
		t.Fatalf("exit=%d %s", exit, stdout)
	}

	exit, stdout, _ = run(t, "agent", "setup", "--print")
	if exit != ExitOK || stdout != agentBlock {
		t.Fatalf("--print: exit=%d %q", exit, stdout)
	}
}
