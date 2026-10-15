package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// A snapshot that lands while the person types (a fetch already in flight
// when the form opened) can move the cursor; the verdict still goes to the
// decision the form was opened for, or nowhere.
func TestReviewFormIsPinnedToItsDecision(t *testing.T) {
	demo, c := demoAt(time.Minute)
	ds := &recordingSource{DataSource: demo, decided: make(chan struct{}, 1)}
	m := NewReview(ds, testOptions(c), demoAuth)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, m.fetch()())
	m = step(m, press("a"))
	if m.formFor != "d3f2a91c0d1" {
		t.Fatalf("form opened for %q", m.formFor)
	}
	// Someone else decides the auth change meanwhile; the next snapshot has
	// it approved and the cursor lands on another decision.
	if _, err := demo.Decide(context.Background(), "d3f2a91c0d1", Approve, "bo", "Checked the limits."); err != nil {
		t.Fatal(err)
	}
	m = step(m, m.fetch()())
	if d, _ := m.Selected(); d.ID == "d3f2a91c0d1" {
		t.Fatal("the approved decision should have left the queue")
	}
	*m.reason = "Limits match the incident."
	next, cmd := m.submit()
	m = next.(ReviewModel)
	if cmd != nil || len(ds.calls) != 0 {
		t.Fatalf("submit must not decide the decision under the cursor; calls = %q", ds.calls)
	}
	if !strings.Contains(screen(m.View()), "not recorded") {
		t.Fatalf("the person should be told nothing was recorded:\n%s", screen(m.View()))
	}
}

// While a verdict is on its way to the Worker, q waits and ctrl+c leaves.
func TestReviewQuitWaitsWhileRecording(t *testing.T) {
	m, _ := reviewAt(t, 100, 30, demoAuth)
	m.busy = true
	next, cmd := m.Update(press("q"))
	if cmd != nil || !strings.Contains(screen(next.(ReviewModel).View()), "recording your verdict") {
		t.Fatal("q must wait for the verdict to be recorded")
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+c must still leave")
	}
}

// ctrl+c inside the reason form cancels the form (huh's quit key) and
// records nothing; the program stays up.
func TestReviewCtrlCInFormCancels(t *testing.T) {
	m, ds := reviewAt(t, 100, 30, demoAuth)
	m = step(m, press("a"))
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = next.(ReviewModel)
	if cmd == nil {
		t.Fatal("huh answers ctrl+c with its cancel command")
	}
	m = step(m, cmd())
	if m.form != nil {
		t.Fatal("ctrl+c should close the form")
	}
	if n := len(Queue(load(t, ds).Decisions)); n != 3 {
		t.Fatalf("cancel must record nothing; queue = %d", n)
	}
}
