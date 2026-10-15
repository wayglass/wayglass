package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

var sizes = []struct{ w, h int }{{80, 24}, {100, 30}, {120, 36}, {140, 40}, {160, 45}}

func testOptions(c *fixedClock) Options {
	return Options{Now: c.now, Location: time.UTC, Interval: time.Hour, User: "ana"}
}

// step feeds msg to a model and returns the new model, discarding commands.
func step[M tea.Model](m M, msg tea.Msg) M {
	next, _ := m.Update(msg)
	return next.(M)
}

func watchAt(t *testing.T, w, h int, offset time.Duration) (WatchModel, *fixedClock) {
	t.Helper()
	ds, c := demoAt(offset)
	m := NewWatch(ds, testOptions(c))
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})
	m = step(m, m.fetch()())
	return m, c
}

// screen is the visible text of a view, with styling stripped.
func screen(v tea.View) string { return ansi.Strip(v.Content) }

func checkFits(t *testing.T, s string, w, h int) {
	t.Helper()
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		t.Errorf("%d lines, terminal has %d", len(lines), h)
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("line %d is %d wide, terminal has %d: %q", i, ansi.StringWidth(l), w, l)
		}
	}
}

func TestWatchGolden(t *testing.T) {
	for _, sz := range sizes {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := watchAt(t, sz.w, sz.h, time.Minute)
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
	}
}

func TestWatchLabelsSampleData(t *testing.T) {
	m, _ := watchAt(t, 80, 24, time.Minute)
	if !strings.Contains(screen(m.View()), "SAMPLE DATA") {
		t.Fatal("sample data must be labelled on screen")
	}
}

func TestWatchRevealsTagsAsClassified(t *testing.T) {
	// Load at 5s, before the auth change is proposed at 5.4s.
	m, c := watchAt(t, 100, 30, 5*time.Second)
	if strings.Contains(screen(m.View()), "auth-needs-human") {
		t.Fatal("auth change shown before it was proposed")
	}
	c.t = t0.Add(6 * time.Second)
	m = step(m, m.fetch()())
	row := func() string {
		for _, l := range strings.Split(screen(m.View()), "\n") {
			if strings.HasPrefix(strings.TrimPrefix(l, "│ "), "i3f2a91  ") { // a row of the Changes pane
				return l
			}
		}
		t.Fatal("no change row for i3f2a91")
		return ""
	}
	if got := row(); !strings.Contains(got, "classifying…") || strings.Contains(got, "auth") {
		t.Fatalf("new change should start unclassified: %q", got)
	}
	c.t = c.t.Add(2 * revealStep)
	if got := row(); !strings.Contains(got, "auth,web") || strings.Contains(got, "auth-needs-human") {
		t.Fatalf("area should show before the decision: %q", got)
	}
	c.t = c.t.Add(revealDone * revealStep)
	if got := row(); !strings.Contains(got, "auth-needs-human") || strings.Contains(got, "classifying…") {
		t.Fatalf("decision should show once classified: %q", got)
	}
}

func TestWatchIsCalm(t *testing.T) {
	m, _ := watchAt(t, 140, 40, time.Minute)
	v := m.View().Content
	if strings.Contains(v, "\x1b[5m") || strings.Contains(v, ";5m") {
		t.Fatal("watch must never blink")
	}
	// Only rows waiting for a person carry the attention marker.
	for _, line := range strings.Split(ansi.Strip(v), "\n") {
		if !strings.Contains(line, "▲") {
			continue
		}
		if !strings.Contains(line, "needs a person") && !strings.Contains(line, "quarantined") && !strings.Contains(line, "wait") {
			t.Errorf("attention marker on a routine line: %q", line)
		}
	}
}

func TestWatchBacksOffOnErrors(t *testing.T) {
	m, _ := watchAt(t, 100, 30, time.Minute)
	m = step(m, snapshotMsg{err: fmt.Errorf("worker unreachable")})
	m = step(m, snapshotMsg{err: fmt.Errorf("worker unreachable")})
	got := screen(m.View())
	if !strings.Contains(got, "poll failed ×2") {
		t.Fatalf("poll failures should show in the header:\n%s", got)
	}
	if !strings.Contains(got, "acme-web") || !strings.Contains(got, "auth-needs-human") {
		t.Fatal("last good data should stay on screen")
	}
}

func reviewAt(t *testing.T, w, h int, intentID string) (ReviewModel, *DemoSource) {
	t.Helper()
	ds, c := demoAt(time.Minute)
	m := NewReview(ds, testOptions(c), intentID)
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})
	m = step(m, m.fetch()())
	return m, ds
}

func TestReviewGolden(t *testing.T) {
	for _, sz := range sizes {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m, _ := reviewAt(t, sz.w, sz.h, demoInfra)
			out := screen(m.View())
			checkFits(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
	}
}

func TestReviewDetailCarriesTheWhy(t *testing.T) {
	m, _ := reviewAt(t, 140, 60, demoInfra)
	got := screen(m.View())
	for _, want := range []string{
		"Pin Worker compatibility date",                       // intent goal
		"Unpinned date changed runtime behaviour in staging.", // intent reason
		"infra-two-people", "Infra changes affect everyone.",  // rule and its reason
		"1 of 2", "maya", // approvals so far
		"worker/wrangler.jsonc", // diff summary
		"SAMPLE DATA",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail is missing %q", want)
		}
	}
}

func TestReviewPreselectsIntent(t *testing.T) {
	m, _ := reviewAt(t, 100, 30, demoPII)
	d, ok := m.Selected()
	if !ok || d.IntentID != demoPII {
		t.Fatalf("selected %+v, want %s", d, demoPII)
	}
}

func TestReviewFormNeedsReasonAndEscCancels(t *testing.T) {
	m, ds := reviewAt(t, 100, 30, demoAuth)
	m = step(m, tea.KeyPressMsg{Code: 'a', Text: "a"})
	if !strings.Contains(screen(m.View()), "why?") {
		t.Fatal("approve must ask why")
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.form != nil {
		t.Fatal("esc should close the form")
	}
	if n := len(Queue(load(t, ds).Decisions)); n != 3 {
		t.Fatalf("cancel must record nothing; queue = %d", n)
	}
	// An empty reason submitted straight away is refused.
	m = step(m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	m.reason = new(string)
	m2, cmd := m.submit()
	if cmd != nil || !strings.Contains(screen(m2.(ReviewModel).View()), "reason is required") {
		t.Fatal("empty reason must not be submitted")
	}
}

// recordingSource wraps a DataSource and signals each Decide.
type recordingSource struct {
	DataSource
	mu      sync.Mutex
	calls   []string
	decided chan struct{}
}

func (r *recordingSource) Sample() bool { return true }

func (r *recordingSource) Decide(ctx context.Context, id string, v Verdict, by, reason string) (Decision, error) {
	d, err := r.DataSource.Decide(ctx, id, v, by, reason)
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf("%s %s %s %q", id, v, by, reason))
	r.mu.Unlock()
	r.decided <- struct{}{}
	return d, err
}

func TestReviewApproveEndToEnd(t *testing.T) {
	demo, c := demoAt(time.Minute)
	ds := &recordingSource{DataSource: demo, decided: make(chan struct{}, 1)}
	m := NewReview(ds, testOptions(c), demoAuth)

	var out bytes.Buffer
	p := tea.NewProgram(m,
		tea.WithInput(nil), tea.WithOutput(&out), tea.WithWindowSize(100, 30),
		tea.WithColorProfile(colorprofile.Ascii), tea.WithoutSignalHandler(),
		tea.WithContext(t.Context()))
	done := make(chan tea.Model, 1)
	go func() {
		final, err := p.Run()
		if err != nil {
			t.Error(err)
		}
		done <- final
	}()

	time.Sleep(100 * time.Millisecond) // let the first snapshot load
	p.Send(tea.KeyPressMsg{Code: 'a', Text: "a"})
	time.Sleep(50 * time.Millisecond)
	for _, r := range "Limits match the incident." {
		p.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case <-ds.decided:
	case <-time.After(5 * time.Second):
		p.Quit()
		t.Fatal("approve was never recorded")
	}
	time.Sleep(100 * time.Millisecond) // let the refresh land
	p.Quit()
	final := (<-done).(ReviewModel)

	ds.mu.Lock()
	defer ds.mu.Unlock()
	want := `d3f2a91c0d1 approve ana "Limits match the incident."`
	if len(ds.calls) != 1 || ds.calls[0] != want {
		t.Fatalf("calls = %q, want [%s]", ds.calls, want)
	}
	if len(final.Queue()) != 2 {
		t.Fatalf("queue after approve = %d, want 2", len(final.Queue()))
	}
	if !strings.Contains(final.notice, "approved i3f2a91") {
		t.Fatalf("notice = %q", final.notice)
	}
}

func TestValidateReason(t *testing.T) {
	for in, ok := range map[string]bool{"": false, "  ": false, "ok": false, "yes": true, " fine by me ": true} {
		if err := ValidateReason(in); (err == nil) != ok {
			t.Errorf("ValidateReason(%q) = %v", in, err)
		}
	}
}
