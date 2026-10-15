package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/policy"
	"github.com/wayglass/wayglass/internal/textsafe"
)

// queuePaneW is the width of the queue pane when it sits beside the card.
const queuePaneW = 38

// reviewGeom is the pane layout of the review screen for one size.
type reviewGeom struct {
	wide           bool
	bodyH          int
	queueW, queueH int
	cardW, cardH   int // the scrolling card, above the form
	formH          int
	helpH          int
}

func (m ReviewModel) geom() reviewGeom {
	w, h := max(m.width, 40), max(m.height, 8)
	g := reviewGeom{wide: w >= frameWide}
	if _, full := m.helpLines(w - 4); full != "" {
		g.helpH = lineCount(full) + 2
	}
	g.bodyH = max(h-1-lineCount(m.footer(w))-g.helpH, 4)
	if m.form != nil {
		g.formH = lipgloss.Height(m.formBody()) + 2
	}
	if g.wide {
		g.queueW, g.queueH = queuePaneW, g.bodyH
		g.cardW = w - queuePaneW
		g.cardH = max(g.bodyH-g.formH, 3)
		return g
	}
	g.queueW, g.queueH = w, min(max(len(m.queue), 1), 4)+2
	g.cardW = w
	g.cardH = max(g.bodyH-g.queueH-g.formH, 3)
	return g
}

// frame draws the review screen: header bar, queue pane, the decision card
// in framed sections, the reason form framed under it, and the footer.
func (m ReviewModel) frame() string {
	st := m.st
	w, h := max(m.width, 40), max(m.height, 8)
	g := m.geom()

	var body string
	switch {
	case len(m.queue) == 0:
		msg := st.Muted.Render("Nothing is waiting for a person. Policy routed every change to an agent or it already landed.")
		switch {
		case len(m.snap.Warnings) > 0:
			msg = st.Error.Render("! " + strings.Join(m.snap.Warnings, "; "))
		case m.all:
			msg = st.Muted.Render("No changes proposed yet.")
		case len(m.snap.Decisions) > 0:
			msg += st.Muted.Render(" tab shows all.")
		}
		body = st.pane(paneSpec{
			Title: "Queue", Meta: st.Muted.Render("0"), Focused: true,
			Body: lipgloss.NewStyle().Width(paneInner(w)).Render(msg), W: w, H: g.bodyH,
		})
	default:
		queue := m.queuePane(g)
		// Size the card around the form as it renders now; huh's height
		// can change after its first render. m is a copy, so this is local.
		m.vp.SetHeight(g.cardH)
		card := m.vp.View()
		if d, ok := m.Selected(); ok {
			secs := cardSections(st, d, m.intent(d.IntentID), overlapsOf(m.snap.Overlaps, d.IntentID), m.snap.Intents, g.cardW, m.opt.Location)
			switch {
			case m.form != nil:
				card = cardWhole(st, secs, g.cardH)
			case m.vp.TotalLineCount() < g.cardH:
				card = renderCard(st, d, m.intent(d.IntentID), overlapsOf(m.snap.Overlaps, d.IntentID), m.snap.Intents, g.cardW, g.cardH, m.opt.Location)
			}
		}
		if m.form != nil {
			card = stack(card, m.formPane(g))
		}
		if g.wide {
			body = lipgloss.JoinHorizontal(lipgloss.Top, queue, lipgloss.NewStyle().Height(g.bodyH).MaxHeight(g.bodyH).Render(card))
		} else {
			body = stack(queue, lipgloss.NewStyle().Height(g.bodyH-g.queueH).MaxHeight(g.bodyH-g.queueH).Render(card))
		}
	}

	parts := []string{m.headerBar(w), body}
	if _, full := m.helpLines(w - 4); full != "" {
		parts = append(parts, st.pane(paneSpec{Title: "Keys", Body: full, W: w, H: g.helpH}))
	}
	parts = append(parts, m.footer(w))
	return lipgloss.NewStyle().MaxHeight(h).Render(strings.Join(parts, "\n"))
}

func (m ReviewModel) headerBar(w int) string {
	st, s := m.st, m.snap
	left := []barSeg{{st.Section, s.Project, false}}
	if n := m.waiting(); n > 0 {
		left = append(left, barSeg{st.Attention, fmt.Sprintf("▲ %d waiting for a person", n), false})
	} else {
		left = append(left, barSeg{st.Muted, "queue empty", false})
	}
	if m.all {
		left = append(left, barSeg{st.Muted, fmt.Sprintf("showing all %d", len(m.queue)), false})
	}
	if j := jurisdictions(s); j != "" {
		left = append(left, barSeg{st.Muted, j, false})
	}
	var right []barSeg
	if m.err != nil {
		right = append(right, barSeg{st.Error, fmt.Sprintf("poll failed ×%d", m.backoff.Failures()), false})
	} else if !s.Sample {
		right = append(right, barSeg{st.Good, "● live", false})
	}
	if s.Sample {
		right = append(right, barSeg{st.Sample, " SAMPLE DATA ", false}, barSeg{st.Muted, "--demo-data", true})
	}
	return st.headerBar(w, "review", left, right)
}

func (m ReviewModel) footer(w int) string {
	st := m.st
	status := st.Muted.Render("updated " + clock(m.snap.FetchedAt, m.opt.Location))
	if m.err != nil {
		status = st.Error.Render(clip("✗ "+m.err.Error(), 48))
	}
	switch {
	case m.notice != "":
		s := st.Good
		if m.noticeE {
			s = st.Error
		}
		// A refusal says why, which can run long: up to three lines.
		return lipgloss.NewStyle().MaxHeight(3).Render(s.Width(max(w-2, 10)).PaddingLeft(1).Render(m.notice))
	case m.busy:
		return footerLine(w, st.Muted.Render("recording…"), status)
	case m.asking:
		return footerLine(w, st.AI.Render("asking the AI reviewer…"), status)
	}
	short, _ := m.helpLines(w - 4)
	more := ""
	if !m.vp.AtBottom() && m.form == nil {
		more = st.Text.Render("↓ more  ")
	}
	return footerLine(w, more+short, status)
}

func (m ReviewModel) queuePane(g reviewGeom) string {
	st := m.st
	inner := paneInner(g.queueW)
	var rows []string
	if g.wide {
		n := max((g.queueH-2)/3, 1)
		a, b := window(m.cursor, n, len(m.queue))
		for i := a; i < b; i++ {
			rows = append(rows, m.listRow(i, inner, true), "")
		}
	} else {
		a, b := window(m.cursor, g.queueH-2, len(m.queue))
		for i := a; i < b; i++ {
			rows = append(rows, m.listRow(i, inner, false))
		}
	}
	meta := st.Muted.Render(fmt.Sprint(len(m.queue)))
	if m.all {
		meta = st.Muted.Render(fmt.Sprintf("all %d", len(m.queue)))
	}
	return st.pane(paneSpec{
		Title: "Queue", Meta: meta, Body: strings.Join(rows, "\n"),
		W: g.queueW, H: g.queueH, Focused: m.form == nil,
	})
}

// formBody is the reason form and, when the Worker refused the last
// verdict, its reason in red under it.
func (m ReviewModel) formBody() string {
	f := strings.TrimRight(m.form.View(), "\n ")
	if m.formErr != "" {
		f = stack(f, "", m.st.Error.Width(max(paneInner(m.formWidth()), 10)).Render("✗ not recorded: "+m.formErr))
	}
	return f
}

// formWidth is the width of the form pane.
func (m ReviewModel) formWidth() int {
	w := max(m.width, 40)
	if w >= frameWide {
		return w - queuePaneW
	}
	return w
}

func (m ReviewModel) formPane(g reviewGeom) string {
	verb := "Approve"
	if m.verdict == Reject {
		verb = "Reject"
	}
	d, _ := m.Selected()
	return m.st.pane(paneSpec{
		Title: verb, Meta: m.st.Muted.Render(shortID(d.IntentID) + " · a reason is required"),
		Body: m.formBody(), W: g.cardW, H: g.formH, Focused: true,
	})
}

// RenderCard draws one decision as framed sections: the intent and its
// why, the policy rule and its reason, the tags with their evidence, the
// paths and diff, and any overlaps. Each section is a pane w cells wide.
func RenderCard(st Styles, d Decision, in Intent, overlaps []Overlap, intents []Intent, w int, loc *time.Location) string {
	return renderCard(st, d, in, overlaps, intents, w, 0, loc)
}

// renderCard is RenderCard with the last section stretched so the card is
// at least minH lines tall: a short card still fills its column.
func renderCard(st Styles, d Decision, in Intent, overlaps []Overlap, intents []Intent, w, minH int, loc *time.Location) string {
	secs := cardSections(st, d, in, overlaps, intents, w, loc)
	total := 0
	for _, p := range secs {
		total += p.H
	}
	if total < minH {
		secs[len(secs)-1].H += minH - total
	}
	return joinPanes(st, secs)
}

// cardWhole is the card cut to h lines at a section boundary, the last
// whole section stretched to fill: used while the reason form is open,
// when the card cannot scroll.
func cardWhole(st Styles, secs []paneSpec, h int) string {
	n, used := 0, 0
	for n < len(secs) && used+secs[n].H <= h {
		used += secs[n].H
		n++
	}
	if n == 0 {
		return joinPanes(st, secs[:1]) // taller than the room: clipped by the caller
	}
	secs = secs[:n]
	secs[n-1].H += h - used
	return joinPanes(st, secs)
}

func joinPanes(st Styles, ps []paneSpec) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = st.pane(p)
	}
	return strings.Join(out, "\n")
}

// cardSections are the card's framed sections at their natural heights.
func cardSections(st Styles, d Decision, in Intent, overlaps []Overlap, intents []Intent, w int, loc *time.Location) []paneSpec {
	if loc == nil {
		loc = time.Local
	}
	inner := paneInner(w)
	wrap := func(s string, style lipgloss.Style) string { return style.Width(inner).Render(s) }
	type sec struct{ title, meta, body string }
	section := func(title, meta, body string) sec { return sec{title, meta, body} }

	// Intent and why.
	// Agent-written text is kept to one line, so a goal or reason cannot
	// draw what looks like another section inside its box.
	line := textsafe.Line
	meta := []string{"by " + cmpOr(line(in.Agent), "unknown agent")}
	if in.Fork != "" {
		meta = append(meta, "fork "+in.Fork)
	}
	if !in.CreatedAt.IsZero() {
		meta = append(meta, "declared "+clock(in.CreatedAt, loc))
	}
	intent := stack(
		wrap(cmpOr(line(in.Goal), "(intent not loaded)"), st.Section),
		wrap(strings.Join(meta, " · "), st.Muted),
		"",
		wrap("Why: "+cmpOr(line(in.Reason), "(no reason recorded)"), st.Text),
	)

	// Policy: the rule, its reason, approvals so far.
	as := st.ActionStyle(d.Action)
	label := func(l string) string { return st.Muted.Render(fit(l, 10)) }
	pol := []string{
		clip(label("rule")+st.Text.Render(d.MatchedRule)+st.Muted.Render(" → ")+as.Render(actionLabel(d.Action)), inner),
		label("reason") + st.Text.Width(max(inner-10, 1)).Render(cmpOr(line(d.Reason), "(no reason)")),
		clip(label("status")+st.status(d).Render(statusLabel(d)), inner),
	}
	switch d.Action {
	case policy.MultiApprove:
		pol = append(pol, label("approved")+st.Text.Render(fmt.Sprintf("%d of %d", d.Approved(), d.ApprovalsNeeded)))
	case policy.Quarantine:
		pol = append(pol, wrap("Quarantined: it stays in its fork until a person releases or rejects it.", st.Attention))
	}
	if d.ReviewerDifferentFamily {
		pol = append(pol, wrap("Needs an approver from a model family other than the author's.", st.Muted))
	}
	if e := d.Escalation; e != nil {
		// The AI reviewer could not approve this on its own and said why;
		// its words are model-written and shown as text like an agent's.
		pol = append(pol, wrap(st.AI.Render("◆ Escalated by "+aiName(cmpOr(e.Model, AIModel(e.By))))+st.Muted.Render(": ")+st.Text.Render(line(e.Reason)), lipgloss.NewStyle()))
		for _, c := range e.Concerns {
			pol = append(pol, wrap("  • "+line(c), st.Text))
		}
		if e.FromAction != "" {
			pol = append(pol, wrap(fmt.Sprintf("  was %s; a person decides it now", actionLabel(policy.Action(e.FromAction))), st.Muted))
		}
	}
	for _, a := range d.Approvals {
		mark, ms := "✓", st.Good
		if a.Verdict == Reject {
			mark, ms = "✗", st.Stopped
		}
		by := st.Text.Render(line(a.By))
		if a.AI() {
			by = st.AI.Render("◆ " + aiName(AIModel(a.By)))
		}
		pol = append(pol, wrap(ms.Render(mark+" "+string(a.Verdict))+" "+by+st.Muted.Render(" at "+a.At.In(loc).Format("15:04")+": ")+st.Text.Render(line(a.Reason)), lipgloss.NewStyle()))
	}

	// AI advice: what the AI reviewer saw in a change it may not decide.
	// Labelled as advice at the top and the bottom; it never reads as a verdict.
	var advice []string
	if a := d.AIAdvice; a != nil {
		advice = append(advice, wrap(line(a.Summary), st.Text))
		for _, grp := range []struct {
			name  string
			items []string
		}{{"Risks", a.Risks}, {"Concerns", a.Concerns}, {"Questions", a.Questions}} {
			if len(grp.items) == 0 {
				continue
			}
			advice = append(advice, "", st.Muted.Render(grp.name))
			for _, it := range grp.items {
				advice = append(advice, wrap("• "+line(it), st.Text))
			}
		}
		advice = append(advice, "", wrap("Advice only, from "+aiName(a.Model)+": the decision and its reason are yours.", st.Muted))
	}

	// Tags as a small table, then the evidence behind them.
	t := tagParts(d.Tags)
	colW := max((inner-6)/4, 6)
	tags := []string{
		st.Header.Render(joinCols(fit("AREA", colW), fit("DATA CLASS", colW), fit("RISK", colW), "JURISDICTION")),
		joinCols(st.Text.Render(fit(t[0], colW)), dataClassStyle(st, t[1]).Render(fit(t[1], colW)), st.Text.Render(fit(t[2], colW)), st.Text.Render(t[3])),
	}
	if len(d.Tags.Evidence) > 0 {
		tags = append(tags, "")
	}
	for _, e := range d.Tags.Evidence {
		ev := line(e.Tag + " " + e.Value)
		if e.Path != "" {
			ev += " ← " + line(e.Path)
			if e.Line > 0 {
				ev += fmt.Sprintf(" (added line %d)", e.Line)
			}
		}
		if e.Pattern != "" && e.Pattern != e.Path {
			ev += " matched " + line(e.Pattern)
		}
		if e.Detail != "" {
			ev += ": " + line(e.Detail)
		}
		tags = append(tags, st.Muted.Width(inner).Render("· "+ev))
	}

	// Paths: what the intent declared, and what the change touched.
	var paths []string
	if len(in.DeclaredPaths) > 0 {
		paths = append(paths, st.Muted.Render("declared ")+st.Text.Width(max(inner-9, 1)).Render(line(strings.Join(in.DeclaredPaths, ", "))))
	}
	undeclared := in.Undeclared()
	add, del := 0, 0
	for _, f := range d.Files {
		add += f.Added
		del += f.Deleted
	}
	fileMeta := st.Muted.Render(fmt.Sprintf("%d file(s)", len(d.Files)))
	switch {
	case len(d.Files) == 0:
		paths = append(paths, st.Muted.Render("no file list reported"))
	case d.LinesUnknown:
		fileMeta += st.Muted.Render(" · line counts not recorded")
	default:
		fileMeta += " " + st.Good.Render(fmt.Sprintf("+%d", add)) + " " + st.Stopped.Render(fmt.Sprintf("-%d", del))
	}
	pathW := max(inner-14, 8)
	for _, f := range d.Files {
		row := st.Text.Render(fit(line(f.Path), pathW))
		if !d.LinesUnknown {
			row += st.Good.Render(fmt.Sprintf("%6s", fmt.Sprintf("+%d", f.Added))) + st.Stopped.Render(fmt.Sprintf("%6s", fmt.Sprintf("-%d", f.Deleted)))
		}
		if slices.Contains(undeclared, f.Path) {
			row = clip(row+"  "+st.Stopped.Render("! undeclared"), inner)
		}
		paths = append(paths, row)
	}
	if d.Commit != "" {
		paths = append(paths, st.Muted.Render("commit "+d.Commit))
	}

	secs := []sec{
		section("Intent", st.Muted.Render(d.IntentID), intent),
		section("Policy", as.Render(actionLabel(d.Action)), strings.Join(pol, "\n")),
	}
	if a := d.AIAdvice; a != nil {
		secs = append(secs, section("AI advice", st.AI.Render("◆ advisory"), strings.Join(advice, "\n")))
	}
	secs = append(secs,
		section("Tags", "", strings.Join(tags, "\n")),
		section("Paths", fileMeta, strings.Join(paths, "\n")),
	)
	ov := []string{st.Muted.Render("none: no other open intent declared or touched these paths")}
	if len(overlaps) > 0 {
		ov = nil
		for _, o := range overlaps {
			other := o.B
			if other == d.IntentID {
				other = o.A
			}
			who := other
			if i := slices.IndexFunc(intents, func(x Intent) bool { return x.ID == other }); i >= 0 {
				who = fmt.Sprintf("%s (%s: %s)", other, line(intents[i].Agent), line(intents[i].Goal))
			}
			ov = append(ov,
				clip(st.Title.Render("⇄ ")+st.Text.Render(who), inner),
				clip(st.Muted.Render("  on ")+st.Text.Render(strings.Join(o.Paths, ", ")), inner),
				st.Muted.Width(inner).Render("  → suggested re-plan: "+line(o.Replan.Summary)))
		}
	}
	secs = append(secs, section("Overlaps", st.Muted.Render(fmt.Sprint(len(overlaps))), strings.Join(ov, "\n")))
	out := make([]paneSpec, len(secs))
	for i, x := range secs {
		out[i] = paneSpec{Title: x.title, Meta: x.meta, Body: x.body, W: w, H: lineCount(x.body) + 2}
	}
	return out
}

// dataClassStyle marks a sensitive data class; a dash stays plain.
func dataClassStyle(st Styles, v string) lipgloss.Style {
	if v == "-" {
		return st.Muted
	}
	return st.Attention
}
