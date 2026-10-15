package tui

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/policy"
)

// This file holds the frame both TUIs share: a header bar, bordered panes
// with their title set into the top border, and the footer. The one-shot
// text output (no TTY) does not use it; pipes keep the plain layout.

// paneSpec describes one bordered pane. The pane is exactly W cells wide
// and H lines tall; the body is clipped (with an ellipsis) and padded to
// fit, so a pane can never wrap or push its border.
type paneSpec struct {
	Title   string // plain text, set into the top border
	Meta    string // styled, after the title (counts, action badge)
	Hint    string // styled, set into the bottom border on the right
	Body    string
	W, H    int
	Focused bool
}

// paneInner is the content width of a pane w cells wide: border plus one
// cell of padding on each side.
func paneInner(w int) int { return max(w-4, 0) }

// pane draws a rounded box with its title in the top border.
func (st Styles) pane(p paneSpec) string {
	w, h := p.W, p.H
	if w < 6 || h < 2 {
		return strings.TrimRight(strings.Repeat(strings.Repeat(" ", max(w, 0))+"\n", max(h, 0)), "\n")
	}
	bs, ts := st.Border, st.PaneTitle
	if p.Focused {
		bs, ts = st.BorderFocus, st.PaneTitleFocus
	}
	inner := paneInner(w)

	// Top: ╭─ Title meta ───────╮, dropping the meta before the title.
	title := ts.Render(" " + p.Title + " ")
	meta := ""
	if p.Meta != "" {
		meta = p.Meta + " "
	}
	room := w - 3 // ╭─ … ╮
	if lipgloss.Width(title)+lipgloss.Width(meta) > room {
		meta = ""
	}
	if lipgloss.Width(title) > room {
		title = ts.Render(" " + clip(p.Title, max(room-2, 1)) + " ")
	}
	top := bs.Render("╭─") + title + meta
	top += bs.Render(strings.Repeat("─", max(w-1-lipgloss.Width(top), 0)) + "╮")

	// Bottom: ╰──────── hint ─╯
	bottom := bs.Render("╰" + strings.Repeat("─", w-2) + "╯")
	if p.Hint != "" && lipgloss.Width(p.Hint)+6 <= w {
		hint := " " + p.Hint + " "
		bottom = bs.Render("╰"+strings.Repeat("─", w-3-lipgloss.Width(hint))) + hint + bs.Render("─╯")
	}

	lines := strings.Split(p.Body, "\n")
	side := bs.Render("│")
	rows := make([]string, 0, h)
	rows = append(rows, top)
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		rows = append(rows, side+" "+fit(l, inner)+" "+side)
	}
	rows = append(rows, bottom)
	return strings.Join(rows, "\n")
}

// barSeg is one segment of the header bar. Optional segments are the
// first to go when the terminal is narrow.
type barSeg struct {
	style    lipgloss.Style
	text     string
	optional bool
}

// headerBar draws the one-line bar across the top: the brand and view,
// then left segments, and right segments pinned to the right edge. When the
// terminal is too narrow, optional right segments drop first, then left
// segments from the end; required right segments (the sample-data label, a
// poll failure) always stay.
func (st Styles) headerBar(w int, view string, left, right []barSeg) string {
	on := func(s lipgloss.Style) lipgloss.Style { return s.Background(st.BarBG) }
	render := func(segs []barSeg, sep string, keepOptional bool) string {
		parts := make([]string, 0, len(segs))
		for _, s := range segs {
			if s.text != "" && (keepOptional || !s.optional) {
				parts = append(parts, on(s.style).Render(s.text))
			}
		}
		return strings.Join(parts, sep)
	}
	sep := on(st.Border).Render(" │ ")
	space := on(st.Text).Render("  ")
	brand := st.Brand.Render(" ◆ wayglass ") + on(st.PaneTitleFocus).Render(" "+view+" ")
	try := func(l, r string) (string, bool) {
		if l != "" {
			l = sep + l
		}
		if r != "" {
			r += on(st.Text).Render(" ")
		}
		gap := w - lipgloss.Width(brand) - lipgloss.Width(l) - lipgloss.Width(r)
		return brand + l + on(st.Text).Render(strings.Repeat(" ", max(gap, 0))) + r, gap >= 1
	}
	for _, keep := range []bool{true, false} {
		r := render(right, space, keep)
		for n := len(left); n >= 0; n-- {
			if bar, ok := try(render(left[:n], sep, true), r); ok || (!keep && n == 0) {
				return clip(bar, w)
			}
			if keep && n == len(left) {
				break // drop the optional right segments before any left one
			}
		}
	}
	return clip(brand, w)
}

// footerLine puts the key help on the left and a status on the right.
func footerLine(w int, left, right string) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 2 {
		left = clip(left, max(w-lipgloss.Width(right)-4, 0))
		gap = max(w-lipgloss.Width(left)-lipgloss.Width(right)-2, 1)
	}
	return clip(" "+left+strings.Repeat(" ", gap)+right+" ", w)
}

// ActionStyle colours a policy action by what it asks of a person:
// auto-merge is good, review and approval need attention, quarantine and
// block are stopped. The CLI's plain output uses the same mapping.
func (st Styles) ActionStyle(a policy.Action) lipgloss.Style {
	switch a {
	case policy.AutoMerge:
		return st.Good
	case policy.AgentReview, policy.HumanApprove, policy.MultiApprove:
		return st.Warn
	case policy.Quarantine, policy.Block:
		return st.Stopped
	}
	return st.Text
}

// jurisdictions names the jurisdictions present in a snapshot, "US" or
// "US · EU", from the intents and then the classified changes.
func jurisdictions(s Snapshot) string {
	var out []string
	add := func(j string) {
		j = strings.ToUpper(strings.TrimSpace(j))
		if j != "" && !slices.Contains(out, j) {
			out = append(out, j)
		}
	}
	for _, in := range s.Intents {
		add(in.Jurisdiction)
	}
	for _, d := range s.Decisions {
		add(d.Tags.Jurisdiction)
	}
	return strings.Join(out, " · ")
}

// stack joins blocks vertically.
func stack(blocks ...string) string {
	out := blocks[:0:0]
	for _, b := range blocks {
		if b != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n")
}

// window returns the slice bounds that show n of total rows and keep cur
// visible.
func window(cur, n, total int) (int, int) {
	if n >= total {
		return 0, total
	}
	start := min(max(0, cur-n+1), total-n)
	return start, start + n
}
