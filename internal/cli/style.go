package cli

import (
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/tui"
)

// styles is the plain-text palette. It is the TUI palette
// (internal/tui/styles.go): calm, one accent for titles, colour only where
// the reader must notice something. Every human rendering prints through
// lipgloss.Fprint*, which downsamples, so pipes, NO_COLOR and --json get no
// escape codes.
type styles struct {
	title, label, muted, attention, good, stopped lipgloss.Style
}

func cliStyles(o Output) styles {
	t := tui.NewStyles(isDark(o))
	return styles{
		title:     t.Title,
		label:     t.Muted.Width(11),
		muted:     t.Muted,
		attention: t.Attention,
		good:      t.Good,
		stopped:   t.Stopped,
	}
}

// isDark asks the terminal only when one is attached; otherwise colour is
// stripped anyway.
func isDark(o Output) bool {
	if !o.Interactive() {
		return true
	}
	return lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
}

func row(out io.Writer, st styles, label, value string) {
	_, _ = lipgloss.Fprintln(out, "  "+st.label.Render(label)+value)
}

// action colours a policy action by how much it asks of a person:
// auto_merge is good, review and approval need attention, quarantine and
// block are stopped.
func (st styles) action(a string) string {
	switch a {
	case "auto_merge":
		return st.good.Render(a)
	case "agent_review", "human_approve", "multi_approve":
		return st.attention.Render(a)
	case "quarantine", "block":
		return st.stopped.Render(a)
	}
	return a
}

// status colours a decision status the same way as action.
func (st styles) status(s string) string {
	switch s {
	case "approved":
		return st.good.Render(s)
	case "pending_agent", "pending_human":
		return st.attention.Render(s)
	case "quarantined", "blocked", "rejected":
		return st.stopped.Render(s)
	}
	return s
}

// id renders an intent, decision or commit id: present but quiet.
func (st styles) id(s string) string { return st.muted.Render(s) }

// field is one labelled row; an empty label continues the row above.
type field struct{ label, value string }

// fields prints rows with every value in one column: the widest label plus
// a colon and at least two spaces, never narrower than "Decision:" so
// blocks from different commands line up.
func fields(out io.Writer, st styles, rows ...field) {
	w := len("Decision:") + 1
	for _, r := range rows {
		w = max(w, len(r.label)+2)
	}
	for _, r := range rows {
		label := ""
		if r.label != "" {
			label = r.label + ":"
		}
		label += strings.Repeat(" ", w-len(label))
		for i, line := range strings.Split(r.value, "\n") {
			if i > 0 {
				label = strings.Repeat(" ", w)
			}
			_, _ = lipgloss.Fprintln(out, st.muted.Render(label)+line)
		}
	}
}

// localTime shows a Worker timestamp (RFC 3339) in the reader's zone with
// its abbreviation, like 'wayglass log'. Anything unparsable is shown as is.
func localTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return formatLocal(t)
}

func formatLocal(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02 15:04 MST")
}
