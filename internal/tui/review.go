package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/huh/v2"

	"github.com/wayglass/wayglass/internal/policy"
	"github.com/wayglass/wayglass/internal/textsafe"
)

// MinReasonLength is the shortest reason review accepts. A decision without
// a real reason defeats the point of the product.
const MinReasonLength = 3

// ValidateReason is the rule for the reason a person gives.
func ValidateReason(s string) error {
	if len([]rune(strings.TrimSpace(s))) < MinReasonLength {
		return errors.New("a reason is required: every decision carries its why")
	}
	return nil
}

type (
	formDoneMsg   struct{}
	formCancelMsg struct{}
	decidedMsg    struct {
		d      Decision
		v      Verdict
		reason string
		err    error
	}
	// aiMsg is the AI reviewer's answer to an ask from the review screen.
	aiMsg struct {
		d   Decision // the decision asked about
		ans AIAnswer
		err error
	}
)

// ReviewModel is `wayglass review`: the queue of changes policy routed to a
// person, with everything needed to decide, and a reason for every verdict.
type ReviewModel struct {
	ds      DataSource
	opt     Options
	st      Styles
	isDark  bool
	width   int
	height  int
	snap    Snapshot
	loaded  bool
	err     error
	backoff Backoff
	queue   []Decision
	cursor  int
	focusID string // keeps the selection on the same decision across polls
	vp      viewport.Model

	keys reviewKeys
	help help.Model
	all  bool // list every decision, not only those waiting for a person

	form    *huh.Form
	formFor string // the decision the form was opened for; a poll may move the cursor
	verdict Verdict
	reason  *string
	formErr string // why the Worker refused the last verdict, shown under the form
	busy    bool
	asking  bool // an ask to the AI reviewer is in flight
	notice  string
	noticeE bool

	pick picker
	gen  int // project switches; see WatchModel.gen
}

// aiCapable reports whether the source can ask the Worker's AI reviewer.
func (m ReviewModel) aiCapable() bool {
	_, ok := m.ds.(aiAsker)
	return ok
}

// NewReview builds the review model. If intentID is set, that intent's
// decision is selected first.
func NewReview(ds DataSource, opt Options, intentID string) ReviewModel {
	opt = opt.withDefaults()
	m := ReviewModel{
		ds: ds, opt: opt, st: NewStyles(true), isDark: true,
		width: 100, height: 30,
		backoff: NewBackoff(opt.Interval),
		vp:      viewport.New(),
		keys:    newReviewKeys(),
		help:    newHelp(NewStyles(true)),
		pick:    newPicker(NewStyles(true), true),
	}
	m.vp.KeyMap = viewport.KeyMap{} // keys are routed by the model
	m.focusID = intentID            // resolved to a decision on first load
	if opt.Pick {
		m.pick.open, m.pick.loading = true, true
	}
	m.layout()
	return m
}

// Init implements tea.Model.
func (m ReviewModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.fetch(), tea.RequestBackgroundColor}
	if m.pick.open {
		cmds = append(cmds, loadProjects(m.ds))
	}
	return tea.Batch(cmds...)
}

func (m ReviewModel) fetch() tea.Cmd {
	ds, now, gen := m.ds, m.opt.Now, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s, err := Load(ctx, ds, 0, nil, now())
		textsafe.LineAll(&s) // one line per value: the panes are fixed-width cells
		return snapshotMsg{snap: s, err: err, gen: gen}
	}
}

// Selected returns the decision under the cursor.
func (m ReviewModel) Selected() (Decision, bool) {
	if m.cursor < 0 || m.cursor >= len(m.queue) {
		return Decision{}, false
	}
	return m.queue[m.cursor], true
}

// Queue returns the decisions in the list: those waiting for a person, or
// every decision when the filter shows all.
func (m ReviewModel) Queue() []Decision { return m.queue }

// waiting counts the decisions waiting for a person, whatever the filter.
func (m ReviewModel) waiting() int { return len(Queue(m.snap.Decisions)) }

// listed is what the list shows: a person's queue, or with the filter off,
// that queue first and then every other decision, newest first.
func listed(ds []Decision, all bool) []Decision {
	q := Queue(ds)
	if !all {
		return q
	}
	rest := slices.DeleteFunc(slices.Clone(ds), func(d Decision) bool { return d.AwaitsHuman() })
	slices.SortStableFunc(rest, func(a, b Decision) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return append(q, rest...)
}

// Update implements tea.Model.
func (m ReviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.form != nil {
			m.form = m.form.WithWidth(max(20, paneInner(m.formWidth())))
		}
		m.pick.setSize(msg.Width, msg.Height)
		m.layout()
		m.refreshDetail()
	case tea.BackgroundColorMsg:
		m.isDark = msg.IsDark()
		m.st = NewStyles(m.isDark)
		m.help = newHelp(m.st)
		m.pick.restyle(m.st, m.isDark)
		m.refreshDetail()
	case projectsMsg:
		return m, m.pick.apply(msg, m.ds.Project())
	case pickedMsg:
		return m.switchTo(msg.item)
	case snapshotMsg:
		if msg.gen != m.gen {
			return m, nil // fetched for a project we left
		}
		wait := m.backoff.Next(msg.err)
		if msg.err != nil {
			m.err = msg.err
			return m, poll(wait, m.gen)
		}
		m.err = nil
		m.applySnapshot(msg.snap)
		return m, poll(wait, m.gen)
	case pollMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if m.form != nil || m.busy {
			return m, poll(m.opt.Interval, m.gen) // never shift the list under a person who is typing
		}
		return m, m.fetch()
	case formDoneMsg:
		return m.submit()
	case formCancelMsg:
		m.closeForm()
		m.setNotice("cancelled; nothing recorded", false)
		return m, nil
	case aiMsg:
		m.asking = false
		if msg.err != nil {
			m.setNotice("AI reviewer: "+msg.err.Error(), true)
			return m, m.fetch()
		}
		m.setNotice(aiNotice(msg.d, msg.ans), false)
		return m, m.fetch()
	case decidedMsg:
		m.busy = false
		if msg.err != nil {
			if errors.Is(msg.err, ErrNotFound) || errors.Is(msg.err, ErrNotPending) {
				m.setNotice("not recorded: "+msg.err.Error(), true)
				return m, m.fetch()
			}
			// The Worker refused the verdict (a placeholder reason, the
			// author deciding their own change, ...). Reopen the form with
			// the reason kept, and say why under it.
			return m.reopenForm(msg.v, msg.reason, msg.err.Error())
		}
		m.setNotice(decidedNotice(msg.d, msg.v), false)
		return m, m.fetch()
	}

	if m.form != nil {
		return m.updateForm(msg)
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if m.pick.open {
			return m, m.pick.update(k)
		}
		return m.handleKey(k)
	}
	return m, m.pick.forward(msg)
}

// switchTo points the queue at another project; see WatchModel.switchTo.
func (m ReviewModel) switchTo(it ProjectItem) (tea.Model, tea.Cmd) {
	ds, ok := switchSource(m.ds, it)
	if !ok {
		return m, nil
	}
	m.ds = ds
	m.gen++
	m.snap, m.loaded, m.err = Snapshot{}, false, nil
	m.queue, m.cursor, m.focusID = nil, 0, ""
	m.notice, m.noticeE = "", false
	m.backoff = NewBackoff(m.opt.Interval)
	m.vp.SetContent("")
	return m, m.fetch()
}

func (m ReviewModel) handleKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.notice = "" // a key press acknowledges the last notice
	switch {
	case key.Matches(k, m.keys.Quit):
		if m.busy && k.String() != "ctrl+c" {
			// The verdict is on its way to the Worker; leaving now would
			// hide whether it was recorded.
			m.setNotice("recording your verdict; wait for it, or ctrl+c to leave anyway", false)
			return m, nil
		}
		return m, tea.Quit
	case key.Matches(k, m.keys.Up):
		m.move(-1)
	case key.Matches(k, m.keys.Down):
		m.move(1)
	case key.Matches(k, m.keys.PageDown):
		m.vp.PageDown()
	case key.Matches(k, m.keys.PageUp):
		m.vp.PageUp()
	case key.Matches(k, m.keys.Refresh):
		return m, m.fetch()
	case key.Matches(k, m.keys.Pick):
		return m, m.pick.show(m.ds)
	case key.Matches(k, m.keys.Filter):
		m.all = !m.all
		m.applySnapshot(m.snap)
	case key.Matches(k, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		m.layout()
	case key.Matches(k, m.keys.Approve):
		return m.openForm(Approve)
	case key.Matches(k, m.keys.Reject):
		return m.openForm(Reject)
	case key.Matches(k, m.keys.AskAI):
		return m.askAI()
	}
	return m, nil
}

// askAI asks the Worker's AI reviewer about the selected decision: a verdict
// on an agent_review change (it approves or hands the change to a person),
// or advice on a change this person decides. The AI never decides a change
// routed to a person; the Worker refuses that and the refusal is shown.
func (m ReviewModel) askAI() (tea.Model, tea.Cmd) {
	asker, ok := m.ds.(aiAsker)
	if !ok {
		m.setNotice("this source cannot ask the AI reviewer", true)
		return m, nil
	}
	d, ok := m.Selected()
	if !ok || m.asking || m.busy {
		return m, nil
	}
	if d.Status != StatusPendingAgent && !d.AwaitsHuman() {
		m.setNotice(fmt.Sprintf("%s is %s; the AI reviews agent_review changes and advises on those waiting for a person", shortID(d.IntentID), statusLabel(d)), true)
		return m, nil
	}
	m.asking = true
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		ans, err := asker.AskAI(ctx, d)
		textsafe.LineAll(&ans) // model-written text is as untrusted as an agent's
		return aiMsg{d: d, ans: ans, err: err}
	}
}

// aiNotice is the one-line result of an ask.
func aiNotice(d Decision, ans AIAnswer) string {
	who := aiName(ans.Model)
	id := shortID(d.IntentID)
	switch {
	case ans.Kind == "advice":
		return fmt.Sprintf("%s advised on %s; advice only, the decision is yours", who, id)
	case ans.Verdict == "approve":
		return fmt.Sprintf("%s approved %s: %s", who, id, ans.Reason)
	case ans.Verdict == "escalate":
		return fmt.Sprintf("%s escalated %s to a person: %s", who, id, ans.Reason)
	}
	return fmt.Sprintf("%s answered on %s", who, id)
}

func (m *ReviewModel) move(delta int) {
	if len(m.queue) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.queue)-1)
	m.focusID = m.queue[m.cursor].ID
	m.vp.GotoTop()
	m.refreshDetail()
}

func (m ReviewModel) openForm(v Verdict) (tea.Model, tea.Cmd) {
	return m.reopenForm(v, "", "")
}

// reopenForm opens the reason form for the selected decision, prefilled
// with reason and showing errMsg under it when the last try was refused.
func (m ReviewModel) reopenForm(v Verdict, reason, errMsg string) (tea.Model, tea.Cmd) {
	d, ok := m.Selected()
	if !ok || m.busy {
		return m, nil
	}
	if !d.AwaitsHuman() {
		m.setNotice(fmt.Sprintf("%s is %s; only changes waiting for a person are decided here", shortID(d.IntentID), statusLabel(d)), true)
		return m, nil
	}
	verb := "Approve"
	if v == Reject {
		verb = "Reject"
	}
	m.verdict = v
	m.formFor = d.ID
	m.reason = &reason
	m.formErr = errMsg
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Key("reason").
			Title(fmt.Sprintf("%s %s: why?", verb, shortID(d.IntentID))).
			Description("Kept in the audit log and the git note.").
			Prompt("> ").
			Placeholder("what you checked, and why").
			CharLimit(500).
			Validate(ValidateReason).
			Value(m.reason),
	)).
		WithShowHelp(false).
		WithWidth(max(20, paneInner(m.formWidth()))).
		WithTheme(huh.ThemeFunc(huh.ThemeBase))
	m.form.SubmitCmd = func() tea.Msg { return formDoneMsg{} }
	m.form.CancelCmd = func() tea.Msg { return formCancelMsg{} }
	m.notice = ""
	m.layout()
	return m, m.form.Init()
}

func (m ReviewModel) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "esc" {
		m.closeForm()
		m.setNotice("cancelled; nothing recorded", false)
		return m, nil
	}
	next, cmd := m.form.Update(msg)
	if f, ok := next.(*huh.Form); ok {
		m.form = f
	}
	return m, cmd
}

func (m ReviewModel) submit() (tea.Model, tea.Cmd) {
	// The verdict goes to the decision the form was opened for, not to
	// whatever the cursor is on now: a snapshot that landed while the
	// person typed may have moved it.
	i := slices.IndexFunc(m.queue, func(d Decision) bool { return d.ID == m.formFor })
	reason := ""
	if m.reason != nil {
		reason = strings.TrimSpace(*m.reason)
	}
	v := m.verdict
	m.closeForm()
	if i < 0 {
		m.setNotice("not recorded: that change is no longer in the queue; pick it again", true)
		return m, nil
	}
	d := m.queue[i]
	if !d.AwaitsHuman() {
		m.setNotice(fmt.Sprintf("not recorded: %s is now %s", shortID(d.IntentID), statusLabel(d)), true)
		return m, nil
	}
	if err := ValidateReason(reason); err != nil {
		m.setNotice(err.Error(), true)
		return m, nil
	}
	m.busy = true
	ds, by := m.ds, m.opt.User
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		got, err := ds.Decide(ctx, d.ID, v, by, reason)
		textsafe.LineAll(&got)
		return decidedMsg{d: got, v: v, reason: reason, err: err}
	}
}

func (m *ReviewModel) closeForm() {
	m.form, m.formFor, m.reason, m.formErr = nil, "", nil, ""
	m.layout()
}

func (m *ReviewModel) setNotice(s string, isErr bool) { m.notice, m.noticeE = s, isErr }

func decidedNotice(d Decision, v Verdict) string {
	switch {
	case v == Reject:
		return fmt.Sprintf("rejected %s; reason recorded", shortID(d.IntentID))
	case d.Status == StatusApproved:
		return fmt.Sprintf("approved %s; it lands, reason recorded", shortID(d.IntentID))
	default:
		return fmt.Sprintf("approval %d of %d recorded for %s", d.Approved(), d.ApprovalsNeeded, shortID(d.IntentID))
	}
}

func (m *ReviewModel) applySnapshot(s Snapshot) {
	first := !m.loaded
	m.snap, m.loaded = s, true
	m.queue = listed(s.Decisions, m.all)
	idx := slices.IndexFunc(m.queue, func(d Decision) bool {
		return d.ID == m.focusID || (first && d.IntentID == m.focusID)
	})
	switch {
	case idx >= 0:
		m.cursor = idx
	case len(m.queue) == 0:
		m.cursor = 0
	default:
		m.cursor = min(m.cursor, len(m.queue)-1)
	}
	if d, ok := m.Selected(); ok {
		m.focusID = d.ID
	}
	m.refreshDetail()
}

// helpLines renders the key help: one short line, or the full table.
func (m ReviewModel) helpLines(w int) (short, full string) {
	k := m.keys
	k.form = m.form != nil
	k.ai = m.aiCapable()
	if m.all {
		k.Filter.SetHelp("tab", "only for a person")
	}
	h := m.help
	h.SetWidth(w)
	if h.ShowAll {
		return "", clipLines(h.FullHelpView(k.FullHelp()), w)
	}
	return h.ShortHelpView(k.ShortHelp()), ""
}

func (m *ReviewModel) layout() {
	g := m.geom()
	m.vp.SetWidth(g.cardW)
	m.vp.SetHeight(g.cardH)
}

func (m *ReviewModel) refreshDetail() {
	m.layout()
	d, ok := m.Selected()
	if !ok {
		m.vp.SetContent("")
		return
	}
	ovs := overlapsOf(m.snap.Overlaps, d.IntentID)
	m.vp.SetContent(RenderCard(m.st, d, m.intent(d.IntentID), ovs, m.snap.Intents, m.vp.Width(), m.opt.Location))
}

func (m ReviewModel) intent(id string) Intent {
	for _, in := range m.snap.Intents {
		if in.ID == id {
			return in
		}
	}
	return Intent{ID: id}
}

// View implements tea.Model.
func (m ReviewModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "wayglass review · " + m.ds.Project()
	if m.pick.open {
		v.WindowTitle = pickerTitle("review")
	}
	return v
}

func (m ReviewModel) render() string {
	if m.pick.open {
		return m.pick.view(m.st, m.ds.Project(), m.width, m.height)
	}
	if !m.loaded {
		if m.err != nil {
			return m.st.Error.Render("wayglass review: " + m.err.Error())
		}
		return m.st.Muted.Render("wayglass review: loading " + m.ds.Project() + "…")
	}
	return m.frame()
}

func (m ReviewModel) listRow(i, w int, twoLine bool) string {
	st := m.st
	d := m.queue[i]
	in := m.intent(d.IntentID)
	marker, style := "  ", st.Text
	if i == m.cursor {
		marker, style = "▸ ", st.Selected
	}
	what := actionLabel(d.Action)
	switch d.Action {
	case policy.MultiApprove:
		what = fmt.Sprintf("%s %d/%d", what, d.Approved(), d.ApprovalsNeeded)
	case policy.HumanApprove:
		what = "approve"
	}
	whatStyle := st.Attention
	if !d.AwaitsHuman() {
		what, whatStyle = statusLabel(d), st.status(d)
	}
	switch {
	case d.Escalation != nil && d.AwaitsHuman():
		what = "AI escalated"
	case d.AIAdvice != nil && d.AwaitsHuman():
		what += " ·advised"
	}
	if twoLine {
		l1 := style.Render(marker+shortID(d.IntentID)) + "  " + whatStyle.Render(what)
		l2 := "  " + st.Text.Render(clip(in.Goal, w-2))
		return clip(l1, w) + "\n" + clip(l2, w)
	}
	row := style.Render(marker+shortID(d.IntentID)) + "  " + whatStyle.Render(fit(what, 17)) + " " + st.Text.Render(in.Goal)
	return clip(row, w)
}

// RenderDetail renders one decision as Markdown through Glamour: the intent
// goal and reason, the tags with their evidence, the matched rule and its
// reason, approvals so far, and the diff summary. Glamour turns URLs in the
// text into terminal hyperlinks (OSC 8) whose target the writer chose, so
// the output keeps its colour and loses every other escape.
func RenderDetail(d Decision, in Intent, overlaps []Overlap, intents []Intent, width int, isDark bool, loc *time.Location) string {
	md := DetailMarkdown(d, in, overlaps, intents, loc)
	style := "light"
	if isDark {
		style = "dark"
	}
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(max(width-4, 20)))
	if err != nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return textsafe.Styled(strings.Trim(out, "\n"))
}

// DetailMarkdown is the Markdown source of a decision's detail pane.
// overlaps are those involving the decision's intent; intents name the
// other side of each.
func DetailMarkdown(d Decision, in Intent, overlaps []Overlap, intents []Intent, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	// Goals, reasons, names and paths are agent-written: escaped so they
	// render as text and cannot fake a heading, a rule or an approval.
	md, code, cell := textsafe.Markdown, textsafe.Code, textsafe.CodeCell
	goal := cmpOr(in.Goal, "(intent not loaded)")
	p("# %s\n\n", md(goal))
	p("Intent %s by **%s**", code(d.IntentID), md(cmpOr(in.Agent, "unknown agent")))
	if in.Fork != "" {
		p(" in fork %s", code(in.Fork))
	}
	p("\n\n**Why:** %s\n\n", md(cmpOr(in.Reason, "(no reason recorded)")))

	p("## Policy\n\n")
	p("Rule **%s** matched: **%s**\n\n", md(d.MatchedRule), md(string(d.Action)))
	p("> %s\n\n", md(d.Reason))
	switch d.Action {
	case policy.MultiApprove:
		p("Approvals: **%d of %d**\n\n", d.Approved(), d.ApprovalsNeeded)
	case policy.Quarantine:
		p("Quarantined: it stays in its fork until a person releases or rejects it.\n\n")
	}
	if e := d.Escalation; e != nil {
		p("**Escalated by %s:** %s\n\n", md(aiName(cmpOr(e.Model, AIModel(e.By)))), md(e.Reason))
		for _, c := range e.Concerns {
			p("- %s\n", md(c))
		}
		if len(e.Concerns) > 0 {
			p("\n")
		}
	}
	for _, a := range d.Approvals {
		by := md(a.By)
		if a.AI() {
			by = md(aiName(AIModel(a.By)))
		}
		p("- %s **%s** at %s: %s\n", md(string(a.Verdict)), by, a.At.In(loc).Format("15:04"), md(a.Reason))
	}
	if len(d.Approvals) > 0 {
		p("\n")
	}
	if a := d.AIAdvice; a != nil {
		p("## AI advice\n\n")
		p("*Advice from %s; the decision and its reason are yours.*\n\n%s\n\n", md(aiName(a.Model)), md(a.Summary))
		for _, grp := range []struct {
			name  string
			items []string
		}{{"Risks", a.Risks}, {"Concerns", a.Concerns}, {"Questions", a.Questions}} {
			if len(grp.items) == 0 {
				continue
			}
			p("**%s**\n\n", grp.name)
			for _, it := range grp.items {
				p("- %s\n", md(it))
			}
			p("\n")
		}
	}

	t := tagParts(d.Tags)
	p("## Tags\n\n")
	p("| area | data class | risk | jurisdiction |\n|---|---|---|---|\n| %s | %s | %s | %s |\n\n", md(t[0]), md(t[1]), md(t[2]), md(t[3]))
	for _, e := range d.Tags.Evidence {
		line := fmt.Sprintf("- %s **%s**", md(e.Tag), md(e.Value))
		if e.Path != "" {
			line += fmt.Sprintf(" from %s", code(e.Path))
			if e.Line > 0 {
				line += fmt.Sprintf(" (added line %d)", e.Line)
			}
		}
		if e.Pattern != "" {
			line += fmt.Sprintf(" matched %s", code(e.Pattern))
		}
		if e.Detail != "" {
			line += ": " + md(e.Detail)
		}
		p("%s\n", line)
	}
	if len(d.Tags.Evidence) > 0 {
		p("\n")
	}

	p("## Overlaps\n\n")
	if len(overlaps) == 0 {
		p("None: no other open intent declared or touched these paths.\n\n")
	}
	for _, o := range overlaps {
		other := o.B
		if other == d.IntentID {
			other = o.A
		}
		who := ""
		if i := slices.IndexFunc(intents, func(x Intent) bool { return x.ID == other }); i >= 0 {
			who = fmt.Sprintf(" (%s: %s)", md(intents[i].Agent), md(intents[i].Goal))
		}
		paths := make([]string, len(o.Paths))
		for i, p := range o.Paths {
			paths[i] = code(p)
		}
		p("- %s%s on %s. Suggested re-plan: %s.\n", code(other), who, strings.Join(paths, ", "), md(o.Replan.Summary))
	}
	if len(overlaps) > 0 {
		p("\n")
	}

	p("## Diff summary\n\n")
	if len(d.Files) == 0 {
		p("No file list reported.\n")
		return b.String()
	}
	if d.LinesUnknown {
		p("| file |\n|---|\n")
		for _, f := range d.Files {
			p("| `%s` |\n", f.Path)
		}
		p("\n%d file(s), line counts not recorded for this decision", len(d.Files))
		if d.Commit != "" {
			p(", commit `%s`", d.Commit)
		}
		p("\n")
		return b.String()
	}
	p("| file | + | - |\n|---|---:|---:|\n")
	add, del := 0, 0
	for _, f := range d.Files {
		p("| %s | %d | %d |\n", cell(f.Path), f.Added, f.Deleted)
		add += f.Added
		del += f.Deleted
	}
	p("\n%d file(s), +%d -%d", len(d.Files), add, del)
	if d.Commit != "" {
		p(", commit %s", code(d.Commit))
	}
	p("\n")
	return b.String()
}

func cmpOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// RenderQueue is the plain-text queue for non-interactive output.
func RenderQueue(s Snapshot, st Styles, w int) string {
	s = s.flattened()
	q := Queue(s.Decisions)
	byID := map[string]Intent{}
	for _, in := range s.Intents {
		byID[in.ID] = in
	}
	var b strings.Builder
	head := st.Title.Render("wayglass review") + "  " + st.Text.Render(s.Project)
	if s.Sample {
		head += "  " + st.Sample.Render(" SAMPLE DATA ")
	}
	b.WriteString(head + "\n")
	for _, w := range s.Warnings {
		b.WriteString(st.Error.Render("! "+w) + "\n")
	}
	if len(q) == 0 {
		b.WriteString(st.Muted.Render("Nothing is waiting for a person.") + "\n")
		return b.String()
	}
	for _, d := range q {
		in := byID[d.IntentID]
		t := tagParts(d.Tags)
		fmt.Fprintf(&b, "%s  %s  %s\n", st.Text.Render(d.IntentID), st.Attention.Render(statusLabel(d)), st.Text.Render(clip(in.Goal, max(10, w-40))))
		fmt.Fprintf(&b, "  %s\n", st.Muted.Render(clip(fmt.Sprintf("tags %s · %s · %s · %s  rule %s → %s: %s", t[0], t[1], t[2], t[3], d.MatchedRule, d.Action, d.Reason), w-2)))
	}
	b.WriteString(st.Muted.Render("Decide with: wayglass approve|reject <intent-id> --reason \"...\"") + "\n")
	return b.String()
}
