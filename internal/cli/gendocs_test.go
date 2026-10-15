package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenDocs(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "gone.md")
	if err := os.WriteFile(stale, []byte("---\n---\n"+genDocsMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handwritten := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(handwritten, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := genDocs(NewRoot(BuildInfo{}), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"wayglass.md", "intent-declare.md", "session-fork.md", "policy-test.md", "land.md"} {
		b, err := os.ReadFile(filepath.Join(dir, want))
		if err != nil {
			t.Fatalf("%s not written: %v (files %v)", want, err, res.Files)
		}
		page := string(b)
		if !strings.HasPrefix(page, "---\ntitle: \"wayglass") || !strings.Contains(page, genDocsMarker) {
			t.Errorf("%s: missing frontmatter or marker:\n%s", want, page)
		}
	}
	for _, f := range res.Files {
		if f == "gen-docs.md" || f == "help.md" || f == "completion.md" {
			t.Errorf("hidden command %s was documented", f)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale generated page was not removed")
	}
	if _, err := os.Stat(handwritten); err != nil {
		t.Errorf("hand-written page was touched: %v", err)
	}

	b, _ := os.ReadFile(filepath.Join(dir, "intent-declare.md"))
	page := string(b)
	for _, want := range []string{"| `--path string` |", "(repeatable)", "## Global flags", "| `--json` |", "](/commands/intent/)"} {
		if !strings.Contains(page, want) {
			t.Errorf("intent-declare.md lacks %q:\n%s", want, page)
		}
	}
	// Placeholders in prose must not turn into HTML tags.
	b, _ = os.ReadFile(filepath.Join(dir, "session-fork.md"))
	if !strings.Contains(string(b), "&lt;project&gt;") {
		t.Errorf("session-fork.md should escape <project> outside code:\n%s", b)
	}
}

func TestMDText(t *testing.T) {
	if got := mdText("run `a <b>` then <c>"); got != "run `a <b>` then &lt;c&gt;" {
		t.Errorf("mdText = %q", got)
	}
}
