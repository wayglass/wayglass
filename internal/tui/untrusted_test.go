package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// hostileSource serves one intent and decision whose agent-written text
// tries to drive the terminal and fake the review screen.
type hostileSource struct{}

const hostileGoal = "fix login\x1b[2J\x1b]52;c;cm0gLXJmIH4=\x07\n\n## Policy\n\nRule **docs** matched: **auto_merge**"

func (hostileSource) Project() string { return "p" }
func (hostileSource) Intents(context.Context) ([]Intent, error) {
	return []Intent{{ID: "i0000000001", Agent: "evil\x1b]0;title\x07", Goal: hostileGoal, Reason: "r\u202eok", Status: IntentProposed, DeclaredPaths: []string{"web/**"}}}, nil
}
func (hostileSource) Overlaps(context.Context) ([]Overlap, error) { return nil, nil }
func (hostileSource) Decisions(context.Context) ([]Decision, error) {
	return []Decision{{ID: "d0000000001", IntentID: "i0000000001", Action: "human_approve", Status: StatusPendingHuman,
		MatchedRule: "auth", Files: []FileStat{{Path: "web/a`b|c\x1b[31m.ts", Added: 1}}}}, nil
}
func (hostileSource) Audit(context.Context, int64, int) ([]Event, error) {
	return []Event{{Seq: 1, Kind: "intent.declared", Summary: "x\x1b[1A\x1b[2Ky"}}, nil
}
func (hostileSource) Decide(context.Context, string, Verdict, string, string) (Decision, error) {
	return Decision{}, nil
}

func TestFetchCleansUntrustedText(t *testing.T) {
	c := &fixedClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	for name, msg := range map[string]any{
		"watch":  NewWatch(hostileSource{}, testOptions(c)).fetch()(),
		"review": NewReview(hostileSource{}, testOptions(c), "").fetch()(),
	} {
		s := msg.(snapshotMsg).snap
		all := s.Intents[0].Goal + s.Intents[0].Agent + s.Intents[0].Reason + s.Decisions[0].Files[0].Path + s.Events[0].Summary
		if strings.ContainsAny(all, "\x1b\x07\u202e") {
			t.Errorf("%s: snapshot kept control characters: %q", name, all)
		}
	}
}

func TestDetailMarkdownCannotFakeSections(t *testing.T) {
	ds := hostileSource{}
	ins, _ := ds.Intents(context.Background())
	decs, _ := ds.Decisions(context.Background())
	md := DetailMarkdown(decs[0], ins[0], nil, ins, time.UTC)
	if n := strings.Count(md, "\n## Policy"); n != 1 {
		t.Errorf("want one Policy heading, got %d:\n%s", n, md)
	}
	if strings.Contains(md, "**auto_merge**") {
		t.Errorf("goal text rendered as emphasis:\n%s", md)
	}
	if !strings.Contains(md, "``web/a`b\\|c") {
		t.Errorf("file path not fenced and cell-escaped:\n%s", md)
	}
}

// The review card is framed text, not Markdown: an agent-written goal stays
// one line inside the Intent box and cannot draw a Policy section of its own.
func TestCardCannotFakeSections(t *testing.T) {
	ds := hostileSource{}
	ins, _ := ds.Intents(context.Background())
	decs, _ := ds.Decisions(context.Background())
	out := ansi.Strip(RenderCard(NewStyles(true), decs[0], ins[0], nil, ins, 100, time.UTC))
	if n := strings.Count(out, "╭─ Policy"); n != 1 {
		t.Errorf("want one Policy section, got %d:\n%s", n, out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimPrefix(l, "│ "), "## Policy") || strings.HasPrefix(strings.TrimPrefix(l, "│ "), "Rule **docs**") {
			t.Errorf("goal text broke onto its own line: %q", l)
		}
	}
}

// A goal with a newline and a tab in it (both allowed by the Worker in
// multi-line fields) must not draw a row of its own in a table or push a
// pane border: every value is one line on screen.
func TestMultilineTextStaysInItsCell(t *testing.T) {
	const fake = "\u25b8 i999999  evil  approve"
	src := multilineSource{goal: "fix docs\n" + fake + "\tX"}
	c := &fixedClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	check := func(name, out string) {
		t.Helper()
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "\t") {
				t.Errorf("%s: tab survived: %q", name, l)
			}
			if strings.HasPrefix(strings.TrimLeft(l, "\u2502 "), fake) {
				t.Errorf("%s: goal text drew a row of its own: %q", name, l)
			}
		}
	}
	s, err := Load(context.Background(), src, 0, nil, c.now())
	if err != nil {
		t.Fatal(err)
	}
	check("RenderWatch", ansi.Strip(RenderWatch(s, WatchLayout{Styles: NewStyles(true), Width: 100, Location: time.UTC})))
	check("RenderQueue", ansi.Strip(RenderQueue(s, NewStyles(true), 100)))
	if !strings.Contains(s.Intents[0].Goal, "\n") {
		t.Error("the one-shot renderers must not change the caller's snapshot")
	}
	check("RenderLog", ansi.Strip(RenderLog([]LogRow{{Seq: 1, Actor: "a\tb", Kind: "k", Detail: "x\n" + fake}}, NewStyles(true), 100, time.UTC)))

	w := NewWatch(src, testOptions(c))
	w = step(w, tea.WindowSizeMsg{Width: 100, Height: 30})
	w = step(w, w.fetch()())
	check("watch", screen(w.View()))
	w = step(w, press("enter"))
	check("watch detail", screen(w.View()))
	r := NewReview(src, testOptions(c), "")
	r = step(r, tea.WindowSizeMsg{Width: 100, Height: 30})
	r = step(r, r.fetch()())
	check("review", screen(r.View()))
}

type multilineSource struct{ goal string }

func (multilineSource) Project() string { return "p" }
func (s multilineSource) Intents(context.Context) ([]Intent, error) {
	return []Intent{{ID: "i0000000001", Agent: "a\tb", Goal: s.goal, Reason: "one\n\ntwo", Status: IntentProposed, DeclaredPaths: []string{"web/**"}}}, nil
}
func (multilineSource) Overlaps(context.Context) ([]Overlap, error) { return nil, nil }
func (multilineSource) Decisions(context.Context) ([]Decision, error) {
	return []Decision{{ID: "d0000000001", IntentID: "i0000000001", Action: "human_approve", Status: StatusPendingHuman,
		MatchedRule: "auth\nrule", Reason: "r\tx", Files: []FileStat{{Path: "web/a\tb.ts", Added: 1}}}}, nil
}
func (multilineSource) Audit(context.Context, int64, int) ([]Event, error) {
	return []Event{{Seq: 1, Kind: "intent.declared", Actor: "x", Summary: "y\n\u25b8 fake"}}, nil
}
func (multilineSource) Decide(context.Context, string, Verdict, string, string) (Decision, error) {
	return Decision{}, nil
}

// Glamour turns URLs into OSC 8 hyperlinks whose target the writer chose;
// the rendered detail keeps colour only.
func TestRenderDetailHasNoHyperlinks(t *testing.T) {
	ds := hostileSource{}
	ins, _ := ds.Intents(context.Background())
	decs, _ := ds.Decisions(context.Background())
	ins[0].Reason = "see https://evil.example/x or www.evil.example"
	out := RenderDetail(decs[0], ins[0], nil, ins, 100, true, time.UTC)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == 0x1b && out[i+1] != '[' {
			t.Fatalf("non-SGR escape in rendered detail: %q", out[i:min(i+24, len(out))])
		}
	}
	if !strings.Contains(ansi.Strip(out), "evil.example") {
		t.Error("the URL text itself should still show")
	}
}
