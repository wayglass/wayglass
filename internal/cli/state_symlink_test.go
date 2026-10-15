package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shared fork can commit `.wayglass/state` (or `.wayglass` itself) as a
// symlink to anywhere. Forking that session must refuse to write through it
// instead of putting session files outside the new checkout.
func TestSessionForkRefusesStateSymlink(t *testing.T) {
	for _, tc := range []struct {
		name string
		link []string // path under .wayglass to replace with a symlink; nil: .wayglass itself
	}{
		{"state dir", []string{"state"}},
		{"wayglass dir", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlow(t)
			a := e.repo
			e.do(a, ExitOK, "init", "--project", "review-symlink")
			declared := e.do(a, ExitOK, "intent", "declare", "--goal", "Share context", "--reason", "Another agent continues this work", "--path", "docs/**")
			t.Setenv("WAYGLASS_INTENT", get(declared, "intent", "id").(string))
			// Everything is a disposable local fixture. A remote author can
			// commit the same absolute symlink, or a relative one targeting a
			// sibling folder.
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "current")
			const original = "preserve this unrelated file\n"
			if err := os.WriteFile(sentinel, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(append([]string{a, ".wayglass"}, tc.link...)...)
			if tc.link == nil {
				// Keep config.yaml so the fork still names its project: the
				// linked directory stands in for .wayglass wholesale. Its
				// state goes, so any state the fork writes is the fork's.
				if err := os.RemoveAll(filepath.Join(target, "state")); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(target, filepath.Join(outside, "wayglass")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "wayglass"), target); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			}
			e.git(a, "add", "-f", ".wayglass")
			e.git(a, "commit", "-qm", "test: carry a state symlink")
			e.do(a, ExitOK, "push")
			shared := e.do(a, ExitOK, "session", "share", "--note", "Continue the documentation")
			t.Setenv("WAYGLASS_AGENT", "receiver-agent")
			vm := t.TempDir()
			t.Chdir(vm)
			code, stdout, stderr := run(t, "session", "fork", get(shared, "url").(string), "--dir", "work", "--json")
			if code == ExitOK {
				t.Fatalf("session fork succeeded through a symlinked %s", filepath.Join(tc.link...))
			}
			// --json puts the refusal in the envelope, with its own code.
			if !strings.Contains(stdout, `"code":"`+CodeUnsafePath+`"`) || !strings.Contains(stdout, "symlink") {
				t.Fatalf("envelope does not name the symlink: stdout %q stderr %q", stdout, stderr)
			}
			got, err := os.ReadFile(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("session fork followed an untrusted state symlink and overwrote an external file: %q (exit %d, stderr %q)", got, code, stderr)
			}
			if entries, err := os.ReadDir(filepath.Join(outside, "wayglass")); err == nil {
				for _, en := range entries {
					if en.Name() == "state" {
						t.Fatalf("session fork wrote state through the symlinked .wayglass: %v", entries)
					}
				}
			}
		})
	}
}
