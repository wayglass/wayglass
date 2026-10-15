package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

func press(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func TestWatchDetailGolden(t *testing.T) {
	for _, sz := range sizes {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := watchAt(t, sz.w, sz.h, time.Minute)
			m = step(m, press("enter"))
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
	}
}

func TestWatchDetailShowsPathsTagsAndReplan(t *testing.T) {
	m, _ := watchAt(t, 140, 60, time.Minute)
	if in, _ := m.Selected(); in.ID != demoAuth {
		t.Fatalf("first selection = %s, want %s", in.ID, demoAuth)
	}
	m = step(m, press("enter"))
	got := screen(m.View())
	for _, want := range []string{
		"Add rate limiting to login", "Why: Credential stuffing",
		"web/middleware/**", "web/middleware/ratelimit.ts", "via glob", "⇄ overlap",
		"area auth,web", "jurisdiction US", "auth-needs-human", "Auth is a security boundary.",
		demoSession, "suggested re-plan: land " + demoAuth + " before " + demoSession,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail is missing %q\n%s", want, got)
		}
	}
	// Down moves to the next intent and keeps the detail open; esc goes back.
	m = step(m, press("j"))
	if in, _ := m.Selected(); in.ID != demoPII || !m.detail {
		t.Fatalf("after j: selected %s, detail %v", in.ID, m.detail)
	}
	m = step(m, press("esc"))
	if m.detail || !strings.Contains(screen(m.View()), "Changes") {
		t.Fatal("esc should return to the overview")
	}
}

func TestWatchFlagsUndeclaredPaths(t *testing.T) {
	s := Snapshot{Project: "p", Intents: []Intent{{
		ID: "iaaaaaaaaaa", Agent: "a", Goal: "g", Status: IntentOpen,
		DeclaredPaths: []string{"docs/**"}, ActualPaths: []string{"docs/a.md", "web/x.ts"},
	}}}
	st := NewStyles(true)
	if got := RenderIntentDetail(s, "iaaaaaaaaaa", st, 100, time.UTC, nil); !strings.Contains(ansi.Strip(got), "! undeclared") {
		t.Fatalf("undeclared push not flagged:\n%s", ansi.Strip(got))
	}
	if got := ansi.Strip(RenderWatch(s, WatchLayout{Styles: st, Width: 140})); !strings.Contains(got, "1 undecl.") {
		t.Fatalf("overview should count undeclared paths:\n%s", got)
	}
}

func TestWatchHelpToggles(t *testing.T) {
	m, _ := watchAt(t, 100, 30, time.Minute)
	if strings.Contains(screen(m.View()), "scroll down") {
		t.Fatal("full help should start hidden")
	}
	m = step(m, press("?"))
	out := screen(m.View())
	checkFits(t, out, 100, 30)
	if !strings.Contains(out, "scroll down") || !strings.Contains(out, "refresh") {
		t.Fatalf("? should show every key:\n%s", out)
	}
}

// TestViewsFitEveryResize resizes each screen through a range of sizes and
// checks nothing overflows the terminal.
func TestViewsFitEveryResize(t *testing.T) {
	type view struct {
		name string
		keys []string
	}
	watchViews := []view{{"overview", nil}, {"detail", []string{"enter"}}, {"help", []string{"?"}}, {"detail+help", []string{"enter", "?"}}}
	reviewViews := []view{{"queue", nil}, {"all", []string{"tab"}}, {"form", []string{"a"}}, {"help", []string{"?"}}}
	resizes := []struct{ w, h int }{{40, 12}, {60, 16}, {80, 24}, {99, 30}, {100, 30}, {120, 20}, {160, 50}}
	for _, v := range watchViews {
		m, _ := watchAt(t, 80, 24, time.Minute)
		for _, k := range v.keys {
			m = step(m, press(k))
		}
		for _, sz := range resizes {
			m = step(m, tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
			t.Run(fmt.Sprintf("watch/%s/%dx%d", v.name, sz.w, sz.h), func(t *testing.T) { checkFits(t, screen(m.View()), sz.w, sz.h) })
		}
	}
	for _, v := range reviewViews {
		m, _ := reviewAt(t, 80, 24, demoAuth)
		for _, k := range v.keys {
			m = step(m, press(k))
		}
		for _, sz := range resizes {
			m = step(m, tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
			t.Run(fmt.Sprintf("review/%s/%dx%d", v.name, sz.w, sz.h), func(t *testing.T) { checkFits(t, screen(m.View()), sz.w, sz.h) })
		}
	}
}

func TestReviewFilterShowsAll(t *testing.T) {
	m, _ := reviewAt(t, 100, 30, demoAuth)
	if n := len(m.Queue()); n != 3 {
		t.Fatalf("default queue = %d, want the 3 routed to a person", n)
	}
	m = step(m, press("tab"))
	if n := len(m.Queue()); n != 5 {
		t.Fatalf("all = %d, want 5", n)
	}
	if d, _ := m.Selected(); d.IntentID != demoAuth {
		t.Fatalf("filter must keep the selection, got %s", d.IntentID)
	}
	got := screen(m.View())
	if !strings.Contains(got, "showing all 5") || !strings.Contains(got, "▲ 3 waiting for a person") {
		t.Fatalf("header should say the filter is off:\n%s", got)
	}
	// A landed change cannot be decided here.
	for i := 0; i < 5; i++ {
		m = step(m, press("j"))
	}
	if d, _ := m.Selected(); d.AwaitsHuman() {
		t.Fatalf("expected a decided change at the end of the list, got %+v", d)
	}
	m = step(m, press("a"))
	if m.form != nil || !strings.Contains(screen(m.View()), "only changes waiting for a person") {
		t.Fatal("approve on a decided change must explain, not open a form")
	}
	m = step(m, press("tab"))
	if n := len(m.Queue()); n != 3 {
		t.Fatalf("tab back = %d", n)
	}
}

// refusingSource refuses every verdict the way the Worker refuses a
// placeholder reason.
type refusingSource struct{ DataSource }

func (refusingSource) Sample() bool { return true }

func (refusingSource) Decide(context.Context, string, Verdict, string, string) (Decision, error) {
	return Decision{}, errors.New(`bad_request: reason looks like a placeholder ("<what you checked>"); say what you checked or why`)
}

func TestReviewShowsWorkerRefusalInline(t *testing.T) {
	demo, c := demoAt(time.Minute)
	m := NewReview(refusingSource{demo}, testOptions(c), demoAuth)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, m.fetch()())
	m = step(m, press("a"))
	*m.reason = "<what you checked>"
	next, cmd := m.submit()
	m = next.(ReviewModel)
	if cmd == nil {
		t.Fatal("a reason that passes local checks goes to the Worker")
	}
	m = step(m, cmd())
	if m.form == nil {
		t.Fatal("a refused verdict reopens the form")
	}
	if *m.reason != "<what you checked>" {
		t.Fatalf("the reason must be kept for editing, got %q", *m.reason)
	}
	out := screen(m.View())
	checkFits(t, out, 100, 30)
	if !strings.Contains(out, "not recorded") || !strings.Contains(out, "placeholder") {
		t.Fatalf("the Worker's error should show under the form:\n%s", out)
	}
	golden.RequireEqual(t, out)
	m = step(m, press("esc"))
	if m.form != nil || m.formErr != "" {
		t.Fatal("esc clears the form and its error")
	}
}

// partialSource is a source that could not read decisions.
type partialSource struct{ DataSource }

func (partialSource) Warnings() []string { return []string{"this Worker has no GET /v1/decisions yet"} }
func (partialSource) Decisions(context.Context) ([]Decision, error) {
	return []Decision{}, nil
}

func TestPartialDataIsNeverShownAsAllClear(t *testing.T) {
	demo, c := demoAt(time.Minute)
	ds := partialSource{demo}
	w := NewWatch(ds, testOptions(c))
	w = step(w, tea.WindowSizeMsg{Width: 100, Height: 30})
	w = step(w, w.fetch()())
	got := screen(w.View())
	checkFits(t, got, 100, 30)
	if strings.Contains(got, "nothing waits for a person") || !strings.Contains(got, "no GET /v1/decisions") {
		t.Fatalf("watch must say decisions are missing:\n%s", got)
	}
	r := NewReview(ds, testOptions(c), "")
	r = step(r, tea.WindowSizeMsg{Width: 100, Height: 30})
	r = step(r, r.fetch()())
	if got := screen(r.View()); strings.Contains(got, "Nothing is waiting") || !strings.Contains(got, "no GET /v1/decisions") {
		t.Fatalf("review must say decisions are missing:\n%s", got)
	}
}

func TestWatchShowsMeasuredCostOnly(t *testing.T) {
	st := NewStyles(true)
	s := Snapshot{Project: "p"}
	if got := ansi.Strip(RenderWatch(s, WatchLayout{Styles: st, Width: 200})); strings.Contains(got, "ops") {
		t.Fatalf("no cost measured, none shown:\n%s", got)
	}
	s.Counters.Cost = &Cost{Ops: 41, EstUSD: 0.00615, Landed: 2, PerLandedOps: 18.5, PerLandedUSD: 0.002775}
	got := ansi.Strip(RenderWatch(s, WatchLayout{Styles: st, Width: 200}))
	for _, want := range []string{"artifacts ops 41 ≈$0.0062 est.", "per landed change 18.5 ops ≈$0.0028"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A decision proposed before the Worker counted lines has paths only; the
// review must not present that as a change of +0 -0.
func TestReviewDiffSummaryWithoutLineCounts(t *testing.T) {
	d := Decision{ID: "d1", IntentID: "i1", Commit: "abc", Files: []FileStat{{Path: "src/auth/login.ts"}}, LinesUnknown: true}
	got := DetailMarkdown(d, Intent{ID: "i1"}, nil, nil, time.UTC)
	if strings.Contains(got, "+0 -0") || !strings.Contains(got, "line counts not recorded") || !strings.Contains(got, "`src/auth/login.ts`") {
		t.Fatalf("diff summary:\n%s", got)
	}
	d.Files, d.LinesUnknown = []FileStat{{Path: "src/auth/login.ts", Added: 11}}, false
	if got := DetailMarkdown(d, Intent{ID: "i1"}, nil, nil, time.UTC); !strings.Contains(got, "1 file(s), +11 -0") {
		t.Fatalf("diff summary with counts:\n%s", got)
	}
}

// The reason form is framed at the bottom of the card, at the stacked
// (narrow) and side-by-side layouts.
func TestReviewFormGolden(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 36}} {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := reviewAt(t, sz.w, sz.h, demoAuth)
			m = step(m, press("a"))
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			if !strings.Contains(out, "why?") || !strings.Contains(out, "SAMPLE DATA") {
				t.Fatalf("form or sample label missing:\n%s", out)
			}
			golden.RequireEqual(t, out)
		})
	}
}

// Under the stacked width the watch keeps every pane inside the terminal
// and the sample label in the header.
func TestWatchNarrowGolden(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{60, 20}, {80, 24}} {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := watchAt(t, sz.w, sz.h, time.Minute)
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			if !strings.Contains(out, "SAMPLE DATA") || !strings.Contains(out, "Changes") {
				t.Fatalf("narrow watch lost the sample label or the changes:\n%s", out)
			}
			golden.RequireEqual(t, out)
		})
	}
}
