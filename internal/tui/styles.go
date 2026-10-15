package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/policy"
)

// Styles is the TUI palette. It is calm on purpose: most things are plain or
// muted, and only changes routed to a person use the attention colour. Every
// coloured signal also has a text marker, so NO_COLOR and pipes lose nothing.
type Styles struct {
	Title     lipgloss.Style
	Section   lipgloss.Style
	Header    lipgloss.Style // column headers
	Muted     lipgloss.Style
	Text      lipgloss.Style
	Attention lipgloss.Style // routed to a person and still waiting
	Good      lipgloss.Style // landed
	Stopped   lipgloss.Style // blocked or rejected
	Sample    lipgloss.Style // the SAMPLE DATA badge
	Selected  lipgloss.Style
	Error     lipgloss.Style
	Key       lipgloss.Style
	Warn      lipgloss.Style // amber, not bold: an action that asks for review
	AI        lipgloss.Style // the AI reviewer: distinct from people and agents

	// The frame: pane borders, their titles, and the header bar.
	Border         lipgloss.Style
	BorderFocus    lipgloss.Style
	PaneTitle      lipgloss.Style
	PaneTitleFocus lipgloss.Style
	Brand          lipgloss.Style
	BarBG          color.Color
}

// NewStyles builds the palette for a dark or light background.
func NewStyles(isDark bool) Styles {
	ld := lipgloss.LightDark(isDark)
	muted := ld(lipgloss.Color("#6B6B6B"), lipgloss.Color("#8A8A8A"))
	text := ld(lipgloss.Color("#1F1F1F"), lipgloss.Color("#DADADA"))
	accent := ld(lipgloss.Color("#1D5E8C"), lipgloss.Color("#8CC4E8"))
	attention := ld(lipgloss.Color("#9A5B00"), lipgloss.Color("#F2B84B"))
	good := ld(lipgloss.Color("#2E6B3A"), lipgloss.Color("#86C995"))
	stopped := ld(lipgloss.Color("#A33A3A"), lipgloss.Color("#D98C8C"))
	border := ld(lipgloss.Color("#B8BCC2"), lipgloss.Color("#4A505A"))
	barBG := ld(lipgloss.Color("#E6E9ED"), lipgloss.Color("#22262D"))
	ai := ld(lipgloss.Color("#5B3FA8"), lipgloss.Color("#C2AEF5"))
	return Styles{
		Title:     lipgloss.NewStyle().Bold(true).Foreground(accent),
		Section:   lipgloss.NewStyle().Bold(true).Foreground(text),
		Header:    lipgloss.NewStyle().Foreground(muted),
		Muted:     lipgloss.NewStyle().Foreground(muted),
		Text:      lipgloss.NewStyle().Foreground(text),
		Attention: lipgloss.NewStyle().Bold(true).Foreground(attention),
		Good:      lipgloss.NewStyle().Foreground(good),
		Stopped:   lipgloss.NewStyle().Foreground(stopped),
		Sample:    lipgloss.NewStyle().Bold(true).Reverse(true),
		Selected:  lipgloss.NewStyle().Bold(true).Foreground(accent),
		Error:     lipgloss.NewStyle().Foreground(stopped),
		Key:       lipgloss.NewStyle().Foreground(text),
		Warn:      lipgloss.NewStyle().Foreground(attention),
		AI:        lipgloss.NewStyle().Foreground(ai),

		Border:         lipgloss.NewStyle().Foreground(border),
		BorderFocus:    lipgloss.NewStyle().Foreground(accent),
		PaneTitle:      lipgloss.NewStyle().Bold(true).Foreground(text),
		PaneTitleFocus: lipgloss.NewStyle().Bold(true).Foreground(accent),
		Brand:          lipgloss.NewStyle().Bold(true).Foreground(barBG).Background(accent),
		BarBG:          barBG,
	}
}

// fit truncates s to w cells (with an ellipsis) and pads it to exactly w.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}

// clip truncates s to w cells without padding.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		return ansi.Truncate(s, w, "…")
	}
	return s
}

// shortID shortens an intent or decision id for narrow columns.
func shortID(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// tagParts renders the four tag kinds in a fixed order: area, data class,
// risk, jurisdiction. A dash means "classified, none found".
func tagParts(t classify.Tags) [4]string {
	dc := "-"
	if len(t.DataClasses) > 0 {
		dc = strings.Join(t.DataClasses, ",")
	}
	area := "-"
	if len(t.Areas) > 0 {
		area = strings.Join(t.Areas, ",")
	}
	return [4]string{area, dc, orDash(t.Risk), strings.ToUpper(orDash(t.Jurisdiction))}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// actionLabel is the short on-screen name of an action.
func actionLabel(a policy.Action) string {
	switch a {
	case policy.AutoMerge:
		return "auto-merge"
	case policy.AgentReview:
		return "agent review"
	case policy.HumanApprove:
		return "human"
	case policy.MultiApprove:
		return "multi-approve"
	case policy.Quarantine:
		return "quarantine"
	case policy.Block:
		return "block"
	}
	return string(a)
}

// aiMark is the text marker on a change the AI reviewer decided, so the
// signal survives without colour.
const aiMark = " ·AI"

// aiName is the AI reviewer as the screen names it: "AI reviewer (<model>)".
func aiName(model string) string {
	if model == "" {
		return "AI reviewer"
	}
	return "AI reviewer (" + model + ")"
}

// statusLabel is the decision status in words, with a text marker for the
// ones that need a person so the signal survives without colour, and one
// for the ones the AI reviewer decided.
func statusLabel(d Decision) string {
	switch d.Status {
	case StatusApproved:
		if IsAIReviewer(d.DecidedBy) {
			return "landed" + aiMark
		}
		return "landed"
	case StatusRejected:
		return "rejected"
	case StatusPendingAgent:
		return "agent reviewing"
	case StatusPendingHuman:
		if d.Escalation != nil {
			return "▲ AI escalated"
		}
		if d.Action == policy.MultiApprove {
			return fmt.Sprintf("▲ needs a person %d/%d", d.Approved(), d.ApprovalsNeeded)
		}
		return "▲ needs a person"
	case StatusQuarantined:
		return "▲ quarantined"
	case StatusBlocked:
		return "blocked"
	case StatusSuperseded:
		return "superseded (propose again)"
	}
	return d.Status
}

func (st Styles) status(d Decision) lipgloss.Style {
	switch {
	case d.AwaitsHuman():
		return st.Attention
	case d.Status == StatusApproved:
		return st.Good
	case d.Status == StatusBlocked || d.Status == StatusRejected:
		return st.Stopped
	}
	return st.Muted
}

func (st Styles) intentStatus(s string) lipgloss.Style {
	switch s {
	case IntentLanded:
		return st.Good
	case IntentAbandoned, "stale":
		// Calm: neither needs a person now; the word itself is the marker.
		return st.Muted
	}
	return st.Text
}

// minutes formats a duration in minutes as "45m" or "2h05m".
func minutes(m int) string {
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

func clock(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "--:--:--"
	}
	return t.In(loc).Format("15:04:05")
}

// joinCols joins rendered columns with a two-space gutter.
func joinCols(cols ...string) string { return strings.Join(cols, "  ") }
