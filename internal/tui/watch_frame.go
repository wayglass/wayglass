package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/policy"
)

// frameWide is the width from which panes sit side by side; below it they
// stack in one column.
const frameWide = 100

// watchGeom is the pane layout of the live watch screen for one terminal
// size. Heights and widths are of whole panes, borders included.
type watchGeom struct {
	wide           bool
	bodyH          int // between the header strip and the footer
	leftW, rightW  int // wide only
	topH, botH     int // wide only: rows above and below the split
	inH, chH, evH  int // intents, changes, activity
	ovH, polH      int // wide: overlaps and policy panes; narrow: ovH is banner lines
	detailH        int
	helpH, warnH   int
	showActivity   bool
	showPolicyPane bool
}

func (m WatchModel) geom() watchGeom {
	w, h := max(m.width, 40), max(m.height, 8)
	s := m.snap
	_, full := m.helpLines(w - 4)
	g := watchGeom{wide: w >= frameWide, warnH: len(s.Warnings)}
	if full != "" {
		g.helpH = lineCount(full) + 2
	}
	g.bodyH = max(h-2-1-g.warnH-g.helpH, 4) // header bar + counters, footer
	nI := len(s.Intents)
	nC := max(len(s.Decisions), 1)
	wantIn, wantCh := max(nI, 1)+3, nC+3 // rows, column header, border

	if !g.wide {
		if m.detail {
			g.detailH = g.bodyH
			return g
		}
		g.ovH = min(len(s.Overlaps), 2)
		rest := g.bodyH - g.ovH
		g.inH, g.chH = split2(rest-3, wantIn, wantCh)
		if ev := rest - g.inH - g.chH; ev >= 3 {
			g.showActivity, g.evH = true, ev
		} else {
			g.inH, g.chH = split2(rest, wantIn, wantCh)
			g.inH = rest - g.chH
		}
		return g
	}

	// Wide: intents beside the selected intent's detail, the changes
	// across the full width, then activity beside overlaps and policy.
	// Focusing the detail (enter) gives it the changes' rows and more width.
	g.leftW = w / 2
	if m.detail {
		g.leftW = w * 2 / 5
	}
	g.rightW = w - g.leftW
	row1, row2 := max(wantIn, 8), wantCh
	g.botH = g.bodyH - row1 - row2
	if g.botH < 6 {
		g.botH = min(6, max(g.bodyH-8, 0))
		row1, row2 = split2(g.bodyH-g.botH, row1, row2)
	}
	if g.botH > 12 { // more room goes to the detail, which is long
		row1 += g.botH - 12
		g.botH = 12
	}
	g.inH, g.chH, g.detailH = row1, row2, row1
	if m.detail {
		g.inH, g.chH, g.detailH = row1+row2, 0, row1+row2
	}
	g.topH = row1 + row2
	g.showActivity, g.evH = g.botH >= 3, g.botH
	// Bottom right: overlaps, then the policy counts when they fit.
	ovWant := max(2*len(s.Overlaps), 1) + 2
	g.ovH = min(ovWant, g.botH)
	if pol := g.botH - g.ovH; pol >= 4 {
		g.showPolicyPane, g.polH = true, pol
	} else {
		g.ovH = g.botH
	}
	return g
}

// split2 shares total rows between two panes that want a and b, each
// getting at least 4 (a border, a header and one row) when possible.
func split2(total, a, b int) (int, int) {
	if a+b <= total {
		return a, b
	}
	half := total / 2
	switch {
	case a <= half:
		return a, total - a
	case b <= half:
		return total - b, b
	}
	return total - half, half
}

// frame draws the live watch screen: header bar, panes, footer.
func (m WatchModel) frame() string {
	st, s := m.st, m.snap
	w, h := max(m.width, 40), max(m.height, 8)
	g := m.geom()

	parts := []string{m.headerBar(w), " " + firstLine(renderCounters(s.Counters, st, w-2, m.opt.Now()))}
	for _, wm := range s.Warnings {
		parts = append(parts, clip(" "+st.Error.Render("! "+wm), w))
	}

	intents := orderIntents(s.Intents)
	cur := max(0, slices.IndexFunc(intents, func(in Intent) bool { return in.ID == m.focusID }))
	overlapping := map[string]bool{}
	for _, o := range s.Overlaps {
		overlapping[o.A], overlapping[o.B] = true, true
	}
	intentsPane := func(pw, ph int) string {
		inner := paneInner(pw)
		n := max(ph-3, 1)
		a, b := window(cur, n, len(intents))
		hint := ""
		if b-a < len(intents) {
			hint = st.Muted.Render(fmt.Sprintf("%d–%d of %d", a+1, b, len(intents)))
		}
		open := 0
		for _, in := range intents {
			if in.Active() {
				open++
			}
		}
		rows := renderIntents(intents[a:b], len(intents), st, inner, m.focusID, overlapping)
		if inner < 56 {
			rows = renderIntentsCompact(intents[a:b], st, inner, m.focusID)
		}
		return st.pane(paneSpec{
			Title: "Intents", Meta: st.Muted.Render(fmt.Sprintf("%d · %d open", len(intents), open)), Hint: hint,
			Body: rows,
			W:    pw, H: ph, Focused: !m.detail,
		})
	}
	changesPane := func(pw, ph int) string {
		changes := orderChanges(s.Decisions)
		n := max(ph-3, 1)
		shown := changes[:min(n, len(changes))]
		hint := ""
		if len(shown) < len(changes) {
			hint = st.Muted.Render(fmt.Sprintf("%d of %d", len(shown), len(changes)))
		}
		meta := st.Muted.Render(fmt.Sprint(len(changes)))
		if k := s.Counters.AwaitingHuman; k > 0 {
			meta += st.Muted.Render(" · ") + st.Attention.Render(fmt.Sprintf("▲ %d wait for a person", k))
		}
		return st.pane(paneSpec{
			Title: "Changes", Meta: meta, Hint: hint,
			Body: renderChanges(shown, len(changes), st, paneInner(pw), m.stage),
			W:    pw, H: ph,
		})
	}
	activityPane := func(pw, ph int) string {
		n := max(ph-2, 1)
		ev := s.Events[max(0, len(s.Events)-n):]
		return st.pane(paneSpec{
			Title: "Activity", Meta: st.Muted.Render(fmt.Sprint(len(s.Events))),
			Body: renderActivity(ev, st, paneInner(pw), m.opt.Location),
			W:    pw, H: ph,
		})
	}
	detailPane := func(pw, ph int) string {
		in, _ := m.Selected()
		hint := st.Muted.Render("enter to focus")
		if m.detail {
			hint = ""
			if !m.vp.AtBottom() {
				hint = st.Text.Render("↓ more · pgdn")
			}
		}
		return st.pane(paneSpec{
			Title: "Intent", Meta: st.Muted.Render(in.ID), Hint: hint,
			Body: m.vp.View(), W: pw, H: ph, Focused: m.detail,
		})
	}

	var body string
	switch {
	case !g.wide && m.detail:
		body = detailPane(w, g.detailH)
	case !g.wide:
		blocks := []string{intentsPane(w, g.inH), changesPane(w, g.chH)}
		for _, o := range s.Overlaps[:g.ovH] {
			blocks = append(blocks, clip(" "+overlapLine(o, st), w))
		}
		if g.showActivity {
			blocks = append(blocks, activityPane(w, g.evH))
		}
		body = stack(blocks...)
	default:
		top := lipgloss.JoinHorizontal(lipgloss.Top, intentsPane(g.leftW, g.inH), detailPane(g.rightW, g.detailH))
		if g.chH > 0 {
			top = stack(top, changesPane(w, g.chH))
		}
		var bottom string
		if g.botH > 0 {
			// The bottom row keeps its split when the detail is focused.
			bl := w / 2
			br := w - bl
			right := st.pane(paneSpec{
				Title: "Overlaps", Meta: st.Muted.Render(fmt.Sprint(len(s.Overlaps))), Hint: overlapHint(s, st),
				Body: renderOverlapsFramed(s.Overlaps, st, paneInner(br)),
				W:    br, H: g.ovH,
			})
			if g.showPolicyPane {
				right = stack(right, st.pane(paneSpec{
					Title: "Policy", Meta: st.Muted.Render(fmt.Sprintf("%d decisions", s.Counters.Decisions)),
					Body: renderPolicyCounts(s.Counters, st, paneInner(br), g.polH-2),
					W:    br, H: g.polH,
				}))
			}
			bottom = lipgloss.JoinHorizontal(lipgloss.Top, activityPane(bl, g.evH), right)
		}
		body = stack(top, bottom)
	}
	parts = append(parts, body)

	short, full := m.helpLines(w - 4)
	if full != "" {
		parts = append(parts, st.pane(paneSpec{Title: "Keys", Body: full, W: w, H: g.helpH}))
	}
	more := ""
	if m.detail && !m.vp.AtBottom() {
		more = st.Text.Render("↓ more  ")
	}
	parts = append(parts, footerLine(w, more+short, m.refreshStatus()))
	return lipgloss.NewStyle().MaxHeight(h).Render(strings.Join(parts, "\n"))
}

func (m WatchModel) refreshStatus() string {
	if m.err != nil {
		return m.st.Error.Render(clip("✗ "+m.err.Error(), 48))
	}
	return m.st.Muted.Render("updated " + clock(m.snap.FetchedAt, m.opt.Location))
}

func (m WatchModel) headerBar(w int) string {
	st, s := m.st, m.snap
	left := []barSeg{{st.Section, s.Project, false}}
	if j := jurisdictions(s); j != "" {
		left = append(left, barSeg{st.Muted, j, false})
	}
	switch n := s.Counters.AwaitingHuman; {
	case len(s.Warnings) > 0:
		left = append(left, barSeg{st.Error, "partial: no decisions", false})
	case n > 0:
		left = append(left, barSeg{st.Attention, fmt.Sprintf("▲ %d waiting for a person", n), false})
	default:
		left = append(left, barSeg{st.Muted, "nothing waits for a person", false})
	}
	var right []barSeg
	if m.err != nil {
		right = append(right, barSeg{st.Error, fmt.Sprintf("poll failed ×%d, retrying", m.backoff.Failures()), false})
	} else if !s.Sample {
		right = append(right, barSeg{st.Good, "● live", false})
	}
	if s.Sample {
		right = append(right, barSeg{st.Sample, " SAMPLE DATA ", false}, barSeg{st.Muted, "--demo-data, not live", true})
	}
	return st.headerBar(w, "watch", left, right)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// overlapLine is one overlap on a single line, for the narrow banner.
func overlapLine(o Overlap, st Styles) string {
	return st.Title.Render("⇄ ") + st.Text.Render(shortID(o.A)+" ↔ "+shortID(o.B)) + "  " +
		st.Muted.Render(strings.Join(o.Paths, ", ")) + st.Muted.Render("  → ") + st.Text.Render(replanText(o.Replan))
}

func overlapHint(s Snapshot, st Styles) string {
	if len(s.Overlaps) == 0 {
		return ""
	}
	return st.Muted.Render("re-planned, not failed at merge")
}

// renderOverlapsFramed gives each overlap two lines: who and where, then
// the suggested re-plan.
func renderOverlapsFramed(os []Overlap, st Styles, w int) string {
	if len(os) == 0 {
		return st.Muted.Render("none: every open intent has its own paths")
	}
	rows := make([]string, 0, 2*len(os))
	for _, o := range os {
		rows = append(rows,
			clip(st.Title.Render("⇄ ")+st.Text.Render(shortID(o.A)+" ↔ "+shortID(o.B))+st.Muted.Render(" on ")+st.Text.Render(strings.Join(o.Paths, ", ")), w),
			clip(st.Muted.Render("  → ")+st.Text.Render(replanText(o.Replan)), w))
	}
	return strings.Join(rows, "\n")
}

// renderActivity is the event list sized for a pane: the kind column only
// when there is room for it.
func renderActivity(es []Event, st Styles, w int, loc *time.Location) string {
	if len(es) == 0 {
		return st.Muted.Render("no activity yet")
	}
	rows := make([]string, 0, len(es))
	for _, e := range es {
		id := ""
		if e.IntentID != "" {
			id = shortID(e.IntentID) + " "
		}
		cols := []string{st.Muted.Render(clock(e.At, loc))}
		if w >= 90 {
			cols = append(cols, st.Text.Render(fit(e.Kind, 17)))
		}
		cols = append(cols, st.Muted.Render(fit(e.Actor, 14)), st.Muted.Render(id)+st.Text.Render(e.Summary))
		rows = append(rows, clip(joinCols(cols...), w))
	}
	return strings.Join(rows, "\n")
}

// renderPolicyCounts is decisions by action, one per line with a small bar
// in the action's colour, or packed onto fewer lines when short.
func renderPolicyCounts(c Counters, st Styles, w, h int) string {
	acts := policy.Actions()
	most := 1
	for _, a := range acts {
		most = max(most, c.ByAction[a])
	}
	if h >= len(acts) {
		barW := max(w-14-4-2, 0)
		if most <= barW {
			most = barW // one block per decision while they fit
		}
		rows := make([]string, 0, len(acts))
		for _, a := range acts {
			n := c.ByAction[a]
			bar := strings.Repeat("■", min(barW, (n*barW+most-1)/most))
			val := st.Muted.Render(fmt.Sprintf("%3d", n))
			if n > 0 {
				val = st.Text.Render(fmt.Sprintf("%3d", n))
			}
			rows = append(rows, st.ActionStyle(a).Render(fit(actionLabel(a), 14))+val+"  "+st.ActionStyle(a).Render(bar))
		}
		return strings.Join(rows, "\n")
	}
	// Short pane: a grid, as many per line as fit.
	const cellW = 18
	per := max(w/cellW, 1)
	var rows []string
	for i := 0; i < len(acts); i += per {
		var cells []string
		for _, a := range acts[i:min(i+per, len(acts))] {
			cells = append(cells, fit(st.ActionStyle(a).Render(actionLabel(a))+" "+st.Text.Render(fmt.Sprint(c.ByAction[a])), cellW))
		}
		rows = append(rows, strings.Join(cells, ""))
	}
	return strings.Join(rows, "\n")
}

// renderIntentsCompact is the intent list for a narrow pane beside the
// focused detail: id, goal and status, no column header.
func renderIntentsCompact(in []Intent, st Styles, w int, selected string) string {
	if len(in) == 0 {
		return st.Muted.Render("no intents yet")
	}
	rows := make([]string, 0, len(in))
	statusW := 9
	for _, it := range in {
		mark, idStyle := "  ", st.Muted
		if it.ID == selected {
			mark, idStyle = "▸ ", st.Selected
		}
		agent := ""
		goalW := max(w-2-7-2-2-statusW, 4)
		if w >= 44 {
			agent = st.Muted.Render(fit(it.Agent, 9)) + "  "
			goalW -= 11
		}
		rows = append(rows, clip(idStyle.Render(mark+fit(shortID(it.ID), 7))+"  "+agent+st.Text.Render(fit(it.Goal, goalW))+"  "+
			st.intentStatus(it.StatusText()).Render(it.StatusText()), w))
	}
	return strings.Join(rows, "\n")
}
