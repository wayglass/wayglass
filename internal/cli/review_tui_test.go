package cli

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/tui"
)

// syncBuffer is a bytes.Buffer safe to read while the program writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestReviewTUIApprovesOnFakeWorker drives the review TUI against the fake
// Worker (never the live one): a placeholder reason is refused by the Worker
// and the refusal shows under the form, then a real reason is recorded.
func TestReviewTUIApprovesOnFakeWorker(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo")
	t.Setenv("WAYGLASS_AGENT", "agent-b")
	id := get(e.do(a, ExitOK, "intent", "declare", "--goal", "Rate-limit login", "--reason", "r", "--path", "web/login.ts"), "intent", "id").(string)
	e.write(a, "web/login.ts", "export const maxAttempts = 5;\n")
	e.do(a, ExitOK, "push", "-m", "feat: limit login attempts")
	e.do(a, ExitNeedsHuman, "propose")

	c := api.New(os.Getenv("WAYGLASS_URL"), os.Getenv("WAYGLASS_KEY"))
	c.Agent = "john"
	ds := &liveSource{c: c, project: "demo", jurisdiction: "us"}
	m := tui.NewReview(ds, tui.Options{Interval: time.Hour, User: "john"}, id)

	var out syncBuffer
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
	typeText := func(s string) {
		for _, r := range s {
			p.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				p.Quit()
				<-done
				t.Fatalf("timed out waiting for %s\n%s", what, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	status := func() string {
		e.worker.mu.Lock()
		defer e.worker.mu.Unlock()
		for _, d := range e.worker.projects["demo"].decisions {
			if d.IntentID == id {
				return d.Status
			}
		}
		return ""
	}

	waitFor("the queue", func() bool { return strings.Contains(out.String(), "Rate-limit login") })
	p.Send(tea.KeyPressMsg{Code: 'a', Text: "a"})
	waitFor("the reason form", func() bool { return strings.Contains(out.String(), "why?") })
	typeText("<what you checked>")
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	waitFor("the Worker's refusal", func() bool { return strings.Contains(out.String(), "placeholder") })
	if s := status(); s != api.StatusPendingHuman {
		t.Fatalf("a refused reason must record nothing; status %s", s)
	}
	for range len("<what you checked>") {
		p.Send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeText("Checked the limit against the incident report.")
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	waitFor("the approval", func() bool { return status() == api.StatusApproved })
	p.Quit()
	<-done

	e.worker.mu.Lock()
	defer e.worker.mu.Unlock()
	d := e.worker.projects["demo"].decisions[0]
	if len(d.Approvals) != 1 || d.Approvals[0].By != "admin" || d.Approvals[0].ClaimedBy != "john" || d.Approvals[0].Reason != "Checked the limit against the incident report." {
		t.Fatalf("approvals = %+v", d.Approvals)
	}
}
