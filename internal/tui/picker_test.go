package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// namedSource is an empty project with a name; the picker's switcher wraps it.
type namedSource struct {
	DataSource
	name string
}

func (s namedSource) Project() string { return s.name }
func (namedSource) Intents(context.Context) ([]Intent, error) {
	return []Intent{{ID: "i0000000001", Agent: "a", Goal: "g", Status: IntentOpen}}, nil
}
func (namedSource) Overlaps(context.Context) ([]Overlap, error)        { return nil, nil }
func (namedSource) Decisions(context.Context) ([]Decision, error)      { return nil, nil }
func (namedSource) Audit(context.Context, int64, int) ([]Event, error) { return nil, nil }

// switcher lists fixed projects and switches to any of them.
type switcher struct {
	DataSource
	items    []ProjectItem
	err      error
	switched []string
}

func (s *switcher) Projects(context.Context) ([]ProjectItem, error) { return s.items, s.err }
func (s *switcher) Switch(it ProjectItem) DataSource {
	s.switched = append(s.switched, it.Name)
	return &switcher{DataSource: namedSource{name: it.Name}, items: s.items, switched: s.switched}
}

var pickerItems = []ProjectItem{
	{Name: "demo-1791938487", Jurisdiction: "us", Open: 2, PendingHuman: 1, Landed: 4, LastActivity: t0},
	{Name: "eu-demo-1791797572", Jurisdiction: "eu", Open: 1, Landed: 1},
	{Name: "eu-legacy", Jurisdiction: "eu", Residency: "residency_pending"},
}

// drive feeds msg to a model and runs the commands it returns, feeding
// their messages back (the list computes filter matches in a command).
func drive[M tea.Model](m M, msg tea.Msg) M {
	pending := []tea.Msg{msg}
	for n := 0; len(pending) > 0 && n < 50; n++ {
		next, cmd := m.Update(pending[0])
		m = next.(M)
		pending = pending[1:]
		if cmd == nil {
			continue
		}
		switch out := prompt(cmd).(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range out {
				if r := prompt(c); r != nil {
					pending = append(pending, r)
				}
			}
		default:
			pending = append(pending, out)
		}
	}
	return m
}

// prompt runs a command and returns its message, or nil for one that waits
// (a poll or reveal tick) longer than a test should.
func prompt(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	select {
	case msg := <-out:
		return msg
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

func pickerWatch(t *testing.T, sw *switcher) WatchModel {
	t.Helper()
	m := NewWatch(sw, testOptions(&fixedClock{t: t0}))
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, m.fetch()())
	return m
}

func TestPickerListsSwitchesAndFilters(t *testing.T) {
	sw := &switcher{DataSource: namedSource{name: "demo-1791938487"}, items: pickerItems}
	m := pickerWatch(t, sw)
	if strings.Contains(screen(m.View()), "Projects") {
		t.Fatal("picker shown before p was pressed")
	}
	next, cmd := m.Update(press("p"))
	m = next.(WatchModel)
	if cmd == nil {
		t.Fatal("p must load the directory")
	}
	m = step(m, cmd())
	out := screen(m.View())
	checkFits(t, out, 100, 30)
	for _, want := range []string{"projects", "demo-1791938487  US", "eu-demo-1791797572  EU", "▲ 1 for a person", "not served: residency_pending", "now: demo-1791938487"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker lacks %q:\n%s", want, out)
		}
	}

	// Type a filter: the list fuzzy-matches names; enter picks the match.
	for _, k := range []string{"/", "e", "u", "-", "d", "enter"} {
		m = drive(m, press(k))
	}
	next, cmd = m.Update(press("enter"))
	m = next.(WatchModel)
	if cmd == nil {
		t.Fatal("enter must pick the selected project")
	}
	picked, ok := cmd().(pickedMsg)
	if !ok || picked.item.Name != "eu-demo-1791797572" {
		t.Fatalf("picked %+v", picked)
	}
	next, cmd = m.Update(picked)
	m = next.(WatchModel)
	if cmd == nil || len(sw.switched) != 1 || sw.switched[0] != "eu-demo-1791797572" {
		t.Fatalf("switch: cmd=%v switched=%v", cmd != nil, sw.switched)
	}
	if m.ds.Project() != "eu-demo-1791797572" || m.loaded {
		t.Fatalf("after switch: project=%s loaded=%v", m.ds.Project(), m.loaded)
	}
	m = step(m, cmd())
	if !m.loaded || m.snap.Project != "eu-demo-1791797572" || !strings.Contains(screen(m.View()), "eu-demo-1791797572") {
		t.Fatalf("snapshot after switch: loaded=%v project=%s", m.loaded, m.snap.Project)
	}
	if strings.Contains(screen(m.View()), "SAMPLE DATA") {
		t.Fatal("a live project must not be labelled sample")
	}
}

func TestPickerDropsRepliesForTheProjectLeft(t *testing.T) {
	sw := &switcher{DataSource: namedSource{name: "demo-1791938487"}, items: pickerItems}
	m := pickerWatch(t, sw)
	stale := m.fetch() // fetched for generation 0
	m = step(m, projectsMsg{items: pickerItems})
	next, _ := m.Update(pickedMsg{item: pickerItems[1]})
	m = next.(WatchModel)
	m = step(m, stale())
	if m.loaded || m.snap.Project != "" {
		t.Fatalf("a snapshot for the old project was applied: %q", m.snap.Project)
	}
	m = step(m, pollMsg{gen: 0})
	if m.loaded {
		t.Fatal("an old poll must not fetch")
	}
	if next, cmd := m.Update(pollMsg{gen: m.gen}); cmd == nil || next.(WatchModel).gen != 1 {
		t.Fatal("the current generation's poll must fetch")
	}
}

func TestPickerRefusesUnservedAndClosesOnEsc(t *testing.T) {
	sw := &switcher{DataSource: namedSource{name: "demo-1791938487"}, items: pickerItems}
	m := pickerWatch(t, sw)
	m = step(m, press("p"))
	m = step(m, projectsMsg{items: pickerItems})
	m = step(m, press("down"))
	m = step(m, press("down"))
	next, cmd := m.Update(press("enter"))
	m = next.(WatchModel)
	if !m.pick.open || len(sw.switched) != 0 {
		t.Fatal("an unserved project must not be opened")
	}
	if cmd == nil {
		t.Fatal("the refusal must be shown as a status message")
	}
	if !strings.Contains(screen(m.View()), "not served yet") {
		t.Errorf("no refusal shown:\n%s", screen(m.View()))
	}
	m = step(m, press("esc"))
	if m.pick.open {
		t.Fatal("esc must close the picker")
	}
	if !strings.Contains(screen(m.View()), "demo-1791938487") || !m.loaded {
		t.Fatal("closing the picker must leave the watched project as it was")
	}
}

func TestPickerSaysWhenThereIsNoDirectory(t *testing.T) {
	m, _ := watchAt(t, 100, 30, time.Minute) // the sample source cannot list
	next, cmd := m.Update(press("p"))
	m = next.(WatchModel)
	m = step(m, cmd())
	if !strings.Contains(screen(m.View()), "no project directory") {
		t.Errorf("no explanation shown:\n%s", screen(m.View()))
	}
	sw := &switcher{DataSource: namedSource{name: "demo-1791938487"}, err: errors.New("worker unreachable")}
	m = pickerWatch(t, sw)
	m = step(m, press("p"))
	m = step(m, projectsMsg{err: sw.err})
	if !strings.Contains(screen(m.View()), "worker unreachable") {
		t.Errorf("listing error not shown:\n%s", screen(m.View()))
	}
}

func TestPickFirstOpensThePickerBeforeTheProject(t *testing.T) {
	sw := &switcher{DataSource: namedSource{name: "demo-1791938487"}, items: pickerItems}
	opt := testOptions(&fixedClock{t: t0})
	opt.Pick = true
	w := NewWatch(sw, opt)
	w = step(w, tea.WindowSizeMsg{Width: 100, Height: 30})
	if !w.pick.open || w.Init() == nil {
		t.Fatal("--pick must open the picker and load the directory")
	}
	if !strings.Contains(screen(w.View()), "listing projects") {
		t.Errorf("loading state not shown:\n%s", screen(w.View()))
	}
	r := NewReview(sw, opt, "")
	r = step(r, tea.WindowSizeMsg{Width: 100, Height: 30})
	r = step(r, projectsMsg{items: pickerItems})
	if !strings.Contains(screen(r.View()), "eu-demo-1791797572") {
		t.Errorf("review picker lacks the listing:\n%s", screen(r.View()))
	}
	r = step(r, press("down"))
	next, cmd := r.Update(press("enter"))
	r = next.(ReviewModel)
	r = step(r, cmd())
	if r.ds.Project() != "eu-demo-1791797572" || r.pick.open {
		t.Fatalf("review switch: project=%s open=%v", r.ds.Project(), r.pick.open)
	}
	if r.View().WindowTitle != "wayglass review · eu-demo-1791797572" {
		t.Errorf("title = %q", r.View().WindowTitle)
	}
}
