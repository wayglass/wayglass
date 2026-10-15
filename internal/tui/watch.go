package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/intent"
	"github.com/wayglass/wayglass/internal/policy"
	"github.com/wayglass/wayglass/internal/textsafe"
)

// Options configure the TUI models. The zero value is usable.
type Options struct {
	// Now is the clock; nil means time.Now. Tests pass a fixed clock.
	Now func() time.Time
	// Location formats times; nil means time.Local.
	Location *time.Location
	// Interval is the poll interval; zero means DefaultPollInterval.
	Interval time.Duration
	// User is who approves or rejects in review.
	User string
	// Pick opens the project picker first (watch --pick, review --pick).
	Pick bool
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Interval <= 0 {
		o.Interval = DefaultPollInterval
	}
	if o.User == "" {
		o.User = "you"
	}
	return o
}

// revealStep paces how tags appear on a newly proposed change: area, data
// class, risk, jurisdiction, then the action and matched rule. It is a calm
// fade-in, not a flash, and only runs for changes that arrive while watching.
const revealStep = 300 * time.Millisecond

// revealDone is the stage at which everything about a change is shown.
const revealDone = 5

// staticEvents is how much activity one-shot text output shows.
const staticEvents = 12

type (
	snapshotMsg struct {
		snap Snapshot
		err  error
		gen  int // the source generation it was fetched for (switchTo)
	}
	pollMsg   struct{ gen int }
	revealMsg struct{}
)

// WatchModel is `wayglass watch`: the swarm at a glance, and one intent in
// detail on enter.
type WatchModel struct {
	ds      DataSource
	opt     Options
	st      Styles
	width   int
	height  int
	snap    Snapshot
	loaded  bool
	err     error
	backoff Backoff
	seen    map[string]time.Time // when each decision first appeared; zero = shown in full

	keys    watchKeys
	help    help.Model
	focusID string // the selected intent; follows it across polls
	detail  bool   // showing the selected intent in detail
	vp      viewport.Model

	pick   picker
	isDark bool
	// gen counts project switches; snapshots and polls from an earlier
	// source are dropped by it.
	gen int
}

// NewWatch builds the watch model.
func NewWatch(ds DataSource, opt Options) WatchModel {
	opt = opt.withDefaults()
	st := NewStyles(true)
	m := WatchModel{
		ds: ds, opt: opt, st: st,
		width: 100, height: 30,
		backoff: NewBackoff(opt.Interval),
		seen:    map[string]time.Time{},
		keys:    newWatchKeys(),
		help:    newHelp(st),
		vp:      viewport.New(),
		pick:    newPicker(st, true),
		isDark:  true,
	}
	m.vp.KeyMap = viewport.KeyMap{} // keys are routed by the model
	if opt.Pick {
		m.pick.open, m.pick.loading = true, true
	}
	return m
}

// Init implements tea.Model.
func (m WatchModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.fetch(), tea.RequestBackgroundColor}
	if m.pick.open {
		cmds = append(cmds, loadProjects(m.ds))
	}
	return tea.Batch(cmds...)
}

func (m WatchModel) fetch() tea.Cmd {
	ds, since, prev, now, gen := m.ds, m.snap.Cursor, m.snap.Events, m.opt.Now, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s, err := Load(ctx, ds, since, prev, now())
		textsafe.LineAll(&s) // one line per value: the panes are fixed-width cells
		return snapshotMsg{snap: s, err: err, gen: gen}
	}
}

// poll schedules the next fetch for source generation gen.
func poll(d time.Duration, gen int) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return pollMsg{gen: gen} })
}

func revealTick() tea.Cmd {
	return tea.Tick(revealStep, func(time.Time) tea.Msg { return revealMsg{} })
}

// Update implements tea.Model.
func (m WatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.pick.setSize(msg.Width, msg.Height)
		m.refreshDetail()
	case tea.BackgroundColorMsg:
		m.isDark = msg.IsDark()
		m.st = NewStyles(m.isDark)
		m.help = newHelp(m.st)
		m.pick.restyle(m.st, m.isDark)
		m.refreshDetail()
	case tea.KeyPressMsg:
		if m.pick.open {
			return m, m.pick.update(msg)
		}
		return m.handleKey(msg)
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
		cmds := []tea.Cmd{poll(wait, m.gen)}
		if m.revealing() {
			cmds = append(cmds, revealTick())
		}
		return m, tea.Batch(cmds...)
	case pollMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		return m, m.fetch()
	case revealMsg:
		m.refreshDetail()
		if m.revealing() {
			return m, revealTick()
		}
	default:
		return m, m.pick.forward(msg)
	}
	return m, nil
}

func (m WatchModel) handleKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(k, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(k, m.keys.Back):
		if !m.detail {
			if k.String() == "esc" {
				return m, tea.Quit
			}
			return m, nil
		}
		m.detail = false
	case key.Matches(k, m.keys.Up):
		m.move(-1)
	case key.Matches(k, m.keys.Down):
		m.move(1)
	case key.Matches(k, m.keys.Open):
		if _, ok := m.Selected(); ok {
			m.detail = true
			m.vp.GotoTop()
			m.refreshDetail()
		}
	case key.Matches(k, m.keys.PageDown):
		m.vp.PageDown()
	case key.Matches(k, m.keys.PageUp):
		m.vp.PageUp()
	case key.Matches(k, m.keys.Refresh):
		return m, m.fetch()
	case key.Matches(k, m.keys.Pick):
		return m, m.pick.show(m.ds)
	case key.Matches(k, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		m.refreshDetail()
	}
	return m, nil
}

// switchTo points the view at another project: a fresh snapshot, selection
// and backoff, and a new generation so late replies for the old one are
// dropped. A source that cannot switch leaves everything as it is.
func (m WatchModel) switchTo(it ProjectItem) (tea.Model, tea.Cmd) {
	ds, ok := switchSource(m.ds, it)
	if !ok {
		return m, nil
	}
	m.ds = ds
	m.gen++
	m.snap, m.loaded, m.err = Snapshot{}, false, nil
	m.focusID, m.detail = "", false
	m.seen = map[string]time.Time{}
	m.backoff = NewBackoff(m.opt.Interval)
	m.vp.SetContent("")
	return m, m.fetch()
}

// ordered is the intent list in on-screen order.
func (m WatchModel) ordered() []Intent { return orderIntents(m.snap.Intents) }

// cursor is the selected intent's index in ordered().
func (m WatchModel) cursor() int {
	return max(0, slices.IndexFunc(m.ordered(), func(in Intent) bool { return in.ID == m.focusID }))
}

// Selected returns the selected intent.
func (m WatchModel) Selected() (Intent, bool) {
	in := m.ordered()
	if len(in) == 0 {
		return Intent{}, false
	}
	return in[m.cursor()], true
}

func (m *WatchModel) move(delta int) {
	in := m.ordered()
	if len(in) == 0 {
		return
	}
	i := min(max(m.cursor()+delta, 0), len(in)-1)
	m.focusID = in[i].ID
	m.vp.GotoTop()
	m.refreshDetail()
}

func (m *WatchModel) applySnapshot(s Snapshot) {
	now := m.opt.Now()
	for _, d := range s.Decisions {
		if _, ok := m.seen[d.ID]; ok {
			continue
		}
		if m.loaded {
			m.seen[d.ID] = now // arrived while watching: reveal its tags
		} else {
			m.seen[d.ID] = time.Time{} // already there at start: show in full
		}
	}
	m.snap, m.loaded = s, true
	in := m.ordered()
	if !slices.ContainsFunc(in, func(x Intent) bool { return x.ID == m.focusID }) && len(in) > 0 {
		m.focusID = in[0].ID
	}
	m.refreshDetail()
}

func (m WatchModel) stage(id string) int {
	t, ok := m.seen[id]
	if !ok || t.IsZero() {
		return revealDone
	}
	return min(revealDone, int(m.opt.Now().Sub(t)/revealStep))
}

func (m WatchModel) revealing() bool {
	return slices.ContainsFunc(m.snap.Decisions, func(d Decision) bool { return m.stage(d.ID) < revealDone })
}

// Snapshot returns the last loaded snapshot.
func (m WatchModel) Snapshot() Snapshot { return m.snap }

// helpLines renders the key help: one short line, or the full table.
func (m WatchModel) helpLines(w int) (short, full string) {
	k := m.keys
	k.detail = m.detail
	h := m.help
	h.SetWidth(w)
	if h.ShowAll {
		return "", clipLines(h.FullHelpView(k.FullHelp()), w)
	}
	return h.ShortHelpView(k.ShortHelp()), ""
}

// clipLines clips every line of s to w cells.
func clipLines(s string, w int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = clip(strings.TrimRight(l, " "), w)
	}
	return strings.Join(lines, "\n")
}

func (m *WatchModel) refreshDetail() {
	g := m.geom()
	w := max(m.width, 40)
	if g.wide {
		w = g.rightW
	}
	inner := paneInner(w)
	m.vp.SetWidth(inner)
	m.vp.SetHeight(max(g.detailH-2, 1))
	in, ok := m.Selected()
	if !ok {
		m.vp.SetContent("")
		return
	}
	m.vp.SetContent(RenderIntentDetail(m.snap, in.ID, m.st, inner, m.opt.Location, m.stage))
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// View implements tea.Model.
func (m WatchModel) View() tea.View {
	var body string
	switch {
	case m.pick.open:
		body = m.pick.view(m.st, m.ds.Project(), m.width, m.height)
	case !m.loaded && m.err != nil:
		body = m.st.Error.Render("wayglass watch: " + m.err.Error())
	case !m.loaded:
		body = m.st.Muted.Render("wayglass watch: loading " + m.ds.Project() + "…")
	default:
		body = m.frame()
	}
	v := tea.NewView(body)
	v.AltScreen = true
	v.WindowTitle = "wayglass watch · " + m.ds.Project()
	if m.pick.open {
		v.WindowTitle = pickerTitle("watch")
	}
	return v
}

// WatchLayout carries what RenderWatch needs besides the data.
type WatchLayout struct {
	Styles   Styles
	Width    int
	Height   int // 0 means no limit (static, non-interactive output)
	Location *time.Location
	// Now is used for "last event … ago"; zero means the snapshot's fetch time.
	Now time.Time
	// Stage reports how far a decision's tags have been revealed; nil shows
	// everything.
	Stage    func(decisionID string) int
	Err      error
	Failures int
	// Selected marks the selected intent; empty shows no cursor (static output).
	Selected string
	// Keys is the one-line key help for the footer; FullHelp, when set,
	// replaces it with the full table above the footer.
	Keys     string
	FullHelp string
}

// RenderWatch draws the watch screen. It is shared by the live TUI and the
// one-shot text output, so both always agree.
func RenderWatch(s Snapshot, l WatchLayout) string {
	s = s.flattened()
	st, w := l.Styles, max(l.Width, 40)
	if l.Location == nil {
		l.Location = time.Local
	}
	if l.Now.IsZero() {
		l.Now = s.FetchedAt
	}
	stage := l.Stage
	if stage == nil {
		stage = func(string) int { return revealDone }
	}

	header := renderWatchHeader(s, l, w, "")
	counters := renderCounters(s.Counters, st, w, l.Now)
	footer := renderWatchFooter(s, l, w)

	intents := orderIntents(s.Intents)
	changes := orderChanges(s.Decisions)
	events := s.Events
	if l.Height <= 0 && len(events) > staticEvents {
		events = events[len(events)-staticEvents:]
	}
	cur := max(0, slices.IndexFunc(intents, func(in Intent) bool { return in.ID == l.Selected }))

	// Rows available for content, after the header, counters, footer, the
	// section titles and column headers, and the full help when shown.
	nIn, nCh, nOv, nEv := len(intents), max(len(changes), 1), max(len(s.Overlaps), 1), max(len(events), 1)
	if l.Height > 0 {
		const overhead = 1 + 2 + 1 + 4 + 2 // header, counters, footer; titles; column headers
		budget := max(l.Height-overhead-lineCount(l.FullHelp)-len(s.Warnings), 4)
		nOv = min(nOv, 2)
		nEv = 1
		rest := budget - nOv - nEv
		nCh = min(nCh, max(1, rest-min(nIn, 3)))
		nIn = min(nIn, max(1, rest-nCh))
		nEv = max(1, min(max(len(events), 1), budget-nOv-nCh-nIn))
	}
	start := 0
	if nIn < len(intents) {
		start = min(max(0, cur-nIn+1), len(intents)-nIn)
	}

	overlapping := map[string]bool{}
	for _, o := range s.Overlaps {
		overlapping[o.A], overlapping[o.B] = true, true
	}

	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	line(header)
	line(counters)
	for _, wmsg := range s.Warnings {
		line(clip(st.Error.Render("! "+wmsg), w))
	}

	line(sectionTitle(st, "INTENTS", len(intents), min(nIn, len(intents)), w))
	line(renderIntents(intents[start:start+min(nIn, len(intents))], len(intents), st, w, l.Selected, overlapping))
	line(sectionTitle(st, "CHANGES  tags as classified: area · data class · risk · jurisdiction", len(changes), min(nCh, len(changes)), w))
	line(renderChanges(changes[:min(nCh, len(changes))], len(changes), st, w, stage))
	line(sectionTitle(st, "OVERLAPS  suggested re-plans", len(s.Overlaps), min(nOv, len(s.Overlaps)), w))
	line(renderOverlaps(s.Overlaps[:min(nOv, len(s.Overlaps))], st, w))
	line(sectionTitle(st, "ACTIVITY", len(events), min(nEv, len(events)), w))
	line(renderEvents(events[max(0, len(events)-nEv):], st, w, l.Location))
	if l.FullHelp != "" {
		line(l.FullHelp)
	}
	b.WriteString(footer)
	out := b.String()
	if l.Height > 0 {
		out = lipgloss.NewStyle().MaxHeight(l.Height).Render(out)
	}
	return out
}

func renderWatchHeader(s Snapshot, l WatchLayout, w int, crumb string) string {
	st := l.Styles
	left := st.Title.Render("wayglass watch") + "  " + st.Text.Render(s.Project) + crumb
	var right []string
	if s.Sample {
		right = append(right, st.Sample.Render(" SAMPLE DATA ")+st.Muted.Render(" --demo-data, not live"))
	}
	if l.Err != nil {
		right = append(right, st.Error.Render(fmt.Sprintf("poll failed ×%d, retrying", l.Failures)))
	} else if len(s.Warnings) > 0 {
		right = append(right, st.Error.Render("partial: no decisions"))
	} else {
		right = append(right, st.Muted.Render("updated "+clock(s.FetchedAt, l.Location)))
	}
	r := strings.Join(right, "  ")
	gap := w - lipgloss.Width(left) - lipgloss.Width(r)
	if gap < 2 {
		// Narrow: keep the sample badge and any error, drop the clock.
		switch {
		case s.Sample:
			r = st.Sample.Render(" SAMPLE DATA ")
		case l.Err != nil:
			r = st.Error.Render(fmt.Sprintf("poll failed ×%d", l.Failures))
		default:
			r = ""
		}
		gap = max(2, w-lipgloss.Width(left)-lipgloss.Width(r))
	}
	return clip(left+strings.Repeat(" ", gap)+r, w)
}

// ago formats the time since t: "12s", "4m", "2h05m".
func ago(now, t time.Time) string {
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return minutes(int(d / time.Minute))
}

// usd prints a small estimate with enough digits to not read as zero.
func usd(v float64) string {
	switch {
	case v == 0:
		return "$0"
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func trimFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// renderCounters is the two-line strip: the swarm, then decisions by action.
func renderCounters(c Counters, st Styles, w int, now time.Time) string {
	type item struct {
		long, short, val string
		sval             string // the value when short; "" keeps val
	}
	last := "none yet"
	if c.LastEventAt != nil {
		last = ago(now, *c.LastEventAt) + " ago"
	}
	swarm := []item{
		{"agents", "agents", fmt.Sprint(c.Agents), ""},
		{"open intents", "open", fmt.Sprintf("%d of %d", c.OpenIntents, c.Intents), ""},
		{"overlaps", "overlaps", fmt.Sprint(c.Overlaps), ""},
		{"last event", "last", last, ""},
		{"time saved", "saved", "≈" + minutes(c.TimeSavedMinutes) + " est.", ""},
	}
	if c.AIApprovals+c.AIEscalations+c.AIAdvised > 0 {
		swarm = append(swarm, item{long: "ai reviewer", short: "ai",
			val:  fmt.Sprintf("%d approved · %d escalated · %d advised", c.AIApprovals, c.AIEscalations, c.AIAdvised),
			sval: fmt.Sprintf("%d✓ %d▲ %dadv", c.AIApprovals, c.AIEscalations, c.AIAdvised)})
	}
	if k := c.Cost; k != nil {
		swarm = append(swarm, item{"artifacts ops", "ops", fmt.Sprintf("%d ≈%s est.", k.Ops, usd(k.EstUSD)), ""})
		if k.Landed > 0 {
			swarm = append(swarm, item{"per landed change", "per land", fmt.Sprintf("%s ops ≈%s", trimFloat(k.PerLandedOps), usd(k.PerLandedUSD)), ""})
		}
		if ai := k.AI; ai != nil {
			// Measured by the Worker, never estimated here; today's figure
			// when the Worker keeps one.
			if ai.Today {
				swarm = append(swarm, item{"ai today", "ai $", fmt.Sprintf("%d call(s) ≈%s", ai.TodayCalls, usd(ai.TodayUSD)), ""})
			} else {
				swarm = append(swarm, item{"ai cost", "ai $", fmt.Sprintf("%d call(s) ≈%s", ai.Calls, usd(ai.EstUSD)), ""})
			}
		}
	}
	byAction := []item{{"decisions", "decisions", fmt.Sprint(c.Decisions), ""}}
	for _, a := range policy.Actions() {
		short := map[policy.Action]string{policy.AutoMerge: "auto", policy.AgentReview: "agent", policy.HumanApprove: "human",
			policy.MultiApprove: "multi", policy.Quarantine: "quar.", policy.Block: "block"}[a]
		byAction = append(byAction, item{actionLabel(a), short, fmt.Sprint(c.ByAction[a]), ""})
	}
	render := func(items []item, short bool, sep string) string {
		parts := make([]string, len(items))
		for i, it := range items {
			label, val := it.long, it.val
			if short {
				label = it.short
				if it.sval != "" {
					val = it.sval
				}
			}
			parts[i] = st.Muted.Render(label+" ") + st.Text.Render(val)
		}
		return strings.Join(parts, st.Muted.Render(sep))
	}
	fitLine := func(items []item) string {
		for _, short := range []bool{false, true} {
			for _, sep := range []string{" · ", "  "} {
				if s := render(items, short, sep); lipgloss.Width(s) <= w {
					return s
				}
			}
		}
		return clip(render(items, true, " "), w)
	}
	return fitLine(swarm) + "\n" + fitLine(byAction)
}

func renderWatchFooter(s Snapshot, l WatchLayout, w int) string {
	st := l.Styles
	var left string
	if n := s.Counters.AwaitingHuman; n > 0 {
		noun := "changes wait"
		if n == 1 {
			noun = "change waits"
		}
		left = st.Attention.Render(fmt.Sprintf("▲ %d %s for a person", n, noun)) + st.Muted.Render(" · wayglass review")
	} else if len(s.Warnings) > 0 {
		left = st.Error.Render("decisions unavailable from this Worker")
	} else {
		left = st.Muted.Render("nothing waits for a person")
	}
	right := l.Keys
	if right == "" {
		return clip(left, w)
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		// Narrow: the attention line wins; the help shrinks to its first keys.
		right = clip(right, max(0, w-lipgloss.Width(left)-2))
		gap = max(2, w-lipgloss.Width(left)-lipgloss.Width(right))
	}
	return clip(left+strings.Repeat(" ", gap)+right, w)
}

func sectionTitle(st Styles, title string, total, shown, w int) string {
	t := st.Section.Render(title)
	if shown < total {
		t += st.Muted.Render(fmt.Sprintf("  (%d of %d)", shown, total))
	}
	return clip(t, w)
}

// orderIntents puts active intents first so they survive a short terminal.
func orderIntents(in []Intent) []Intent {
	out := slices.Clone(in)
	rank := func(i Intent) int {
		if i.Active() {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(out, func(a, b Intent) int { return rank(a) - rank(b) })
	return out
}

// orderChanges puts changes waiting for a person first, then the newest.
func orderChanges(ds []Decision) []Decision {
	out := slices.Clone(ds)
	slices.Reverse(out)
	slices.SortStableFunc(out, func(a, b Decision) int {
		ra, rb := 1, 1
		if a.AwaitsHuman() {
			ra = 0
		}
		if b.AwaitsHuman() {
			rb = 0
		}
		return ra - rb
	})
	return out
}

// pathsSummary is "2 decl · 3 pushed" (or "2/3" when narrow), with pushes
// outside the declared paths called out: Zero Trust checks what an agent
// did against what it said it would do.
func pathsSummary(in Intent, narrow, roomy bool) (string, bool) {
	s := fmt.Sprintf("%d decl · %d pushed", len(in.DeclaredPaths), len(in.ActualPaths))
	if narrow {
		s = fmt.Sprintf("%d/%d", len(in.DeclaredPaths), len(in.ActualPaths))
	}
	n := len(in.Undeclared())
	switch {
	case n == 0:
		return s, false
	case roomy:
		return s + fmt.Sprintf(" · %d undecl.", n), true
	}
	return s + fmt.Sprintf(" !%d", n), true
}

func renderIntents(in []Intent, total int, st Styles, w int, selected string, overlapping map[string]bool) string {
	wide := w >= 100
	idW, agentW, pathsW, statusW := 7, 9, 7, 9
	pathsHdr := "DECL/PSH"
	roomy := w >= 130
	if wide {
		idW, agentW, pathsW = 11, 12, 23
		pathsHdr = "PATHS"
	}
	if roomy {
		pathsW = 31
	}
	pathsW = max(pathsW, len(pathsHdr))
	const markW = 2 // cursor
	goalW := max(8, w-markW-idW-agentW-pathsW-statusW-2*4)
	hdr := joinCols(fit("ID", idW), fit("AGENT", agentW), fit("GOAL", goalW), fit(pathsHdr, pathsW), fit("STATUS", statusW))
	rows := []string{st.Header.Render(clip("  "+hdr, w))}
	if total == 0 {
		rows = append(rows, st.Muted.Render("  no intents yet"))
	}
	for _, it := range in {
		id := it.ID
		if !wide {
			id = shortID(id)
		}
		mark, idStyle := "  ", st.Muted
		if selected != "" && it.ID == selected {
			mark, idStyle = "▸ ", st.Selected
		}
		paths, undeclared := pathsSummary(it, !wide, roomy)
		pathStyle := st.Muted
		if undeclared {
			pathStyle = st.Stopped
		}
		ov := ""
		if overlapping[it.ID] {
			ov = "⇄ "
		}
		pathsCell := st.Title.Render(ov) + pathStyle.Render(fit(paths, pathsW-lipgloss.Width(ov)))
		row := idStyle.Render(mark+fit(id, idW)) + "  " + joinCols(
			st.Text.Render(fit(it.Agent, agentW)),
			st.Text.Render(fit(it.Goal, goalW)),
			pathsCell,
			st.intentStatus(it.StatusText()).Render(fit(it.StatusText(), statusW)))
		rows = append(rows, clip(row, w))
	}
	return strings.Join(rows, "\n")
}

func renderChanges(ds []Decision, total int, st Styles, w int, stage func(string) int) string {
	wide := w >= 90 // room for the four tags in their own columns
	idW, actionW, statusW := 7, 13, 20
	var hdr []string
	var tagW []int
	compactStatus := !wide || w < 100 // narrower than the plain output, i.e. inside a pane
	if wide {
		if compactStatus {
			statusW = 15 // "▲ needs you 1/2": leaves the rule room
		}
		tagW = []int{10, 8, 6, 3}
		hdr = []string{fit("INTENT", idW), fit("AREA", tagW[0]), fit("DATA", tagW[1]), fit("RISK", tagW[2]), fit("JUR", tagW[3])}
	} else {
		// Narrow: the four tags share one column, but jurisdiction always shows.
		actionW, statusW = 12, 15
		tagW = []int{20}
		hdr = []string{fit("INTENT", idW), fit("AREA·DATA·RISK·JUR", tagW[0])}
	}
	used := idW + actionW + statusW
	for _, t := range tagW {
		used += t
	}
	ncols := len(hdr) + 3
	ruleW := max(6, w-used-2*(ncols-1))
	hdr = append(hdr, fit("ACTION", actionW), fit("RULE", ruleW), fit("STATUS", statusW))
	rows := []string{st.Header.Render(clip(joinCols(hdr...), w))}
	if total == 0 {
		rows = append(rows, st.Muted.Render("no changes proposed yet"))
	}
	for _, d := range ds {
		sg := stage(d.ID)
		parts := tagParts(d.Tags)
		for i := range parts {
			if sg <= i {
				parts[i] = "…"
			}
		}
		cells := []string{st.Muted.Render(fit(shortID(d.IntentID), idW))}
		if wide {
			for i, p := range parts {
				cells = append(cells, st.Text.Render(fit(p, tagW[i])))
			}
		} else {
			// Truncate the area list, never the jurisdiction.
			tail := "·" + parts[1] + "·" + parts[2] + "·" + parts[3]
			cells = append(cells, st.Text.Render(fit(clip(parts[0], tagW[0]-lipgloss.Width(tail))+tail, tagW[0])))
		}
		if sg < revealDone {
			cells = append(cells, st.Muted.Render(fit("classifying…", actionW)), fit("", ruleW), fit("", statusW))
		} else {
			action, status := actionLabel(d.Action), statusLabel(d)
			if compactStatus {
				status = strings.Replace(status, "needs a person", "needs you", 1)
			}
			if !wide {
				if d.Action == policy.MultiApprove {
					action = "multi"
				}
			}
			cells = append(cells,
				st.ActionStyle(d.Action).Render(fit(action, actionW)),
				st.Muted.Render(fit(d.MatchedRule, ruleW)),
				st.status(d).Render(fit(status, statusW)))
		}
		rows = append(rows, clip(joinCols(cells...), w))
	}
	return strings.Join(rows, "\n")
}

// replanText is the re-plan in words, with short ids for the screen.
func replanText(r intent.Replan) string {
	a, b := shortID(r.First), shortID(r.Then)
	switch r.Action {
	case intent.ReplanLandFirst:
		return fmt.Sprintf("land %s first, %s lands after it", a, b)
	case intent.ReplanStack:
		return fmt.Sprintf("stack %s on %s", b, a)
	case intent.ReplanSplit:
		return fmt.Sprintf("split %s: %s and %s declare narrower paths", strings.Join(r.Paths, ", "), a, b)
	}
	return r.Summary
}

func renderOverlaps(os []Overlap, st Styles, w int) string {
	if len(os) == 0 {
		return st.Muted.Render("none: every open intent has its own paths")
	}
	rows := make([]string, 0, len(os))
	for _, o := range os {
		row := st.Title.Render("⇄ ") + st.Text.Render(shortID(o.A)+" ↔ "+shortID(o.B)) + "  " +
			st.Muted.Render(strings.Join(o.Paths, ", ")) + "  " +
			st.Muted.Render("→ ") + st.Text.Render(replanText(o.Replan))
		rows = append(rows, clip(row, w))
	}
	return strings.Join(rows, "\n")
}

func renderEvents(es []Event, st Styles, w int, loc *time.Location) string {
	if len(es) == 0 {
		return st.Muted.Render("no activity yet")
	}
	rows := make([]string, 0, len(es))
	kindW, actorW := 17, 14
	if w < 100 {
		kindW, actorW = 17, 9
	}
	for _, e := range es {
		id := ""
		if e.IntentID != "" {
			id = shortID(e.IntentID) + " "
		}
		row := joinCols(
			st.Muted.Render(clock(e.At, loc)),
			st.Text.Render(fit(e.Kind, kindW)),
			st.Muted.Render(fit(e.Actor, actorW)),
			st.Muted.Render(id)+st.Text.Render(e.Summary),
		)
		rows = append(rows, clip(row, w))
	}
	return strings.Join(rows, "\n")
}

// latestDecision is the intent's newest decision, if any.
func latestDecision(ds []Decision, intentID string) (Decision, bool) {
	var out Decision
	found := false
	for _, d := range ds {
		if d.IntentID == intentID && (!found || !d.CreatedAt.Before(out.CreatedAt)) {
			out, found = d, true
		}
	}
	return out, found
}

// overlapsOf lists the overlaps involving one intent.
func overlapsOf(os []Overlap, id string) []Overlap {
	var out []Overlap
	for _, o := range os {
		if o.A == id || o.B == id {
			out = append(out, o)
		}
	}
	return out
}

// RenderIntentDetail draws one intent: goal and why, declared against
// pushed paths, its change's tags and decision, and its overlaps with the
// suggested re-plan.
func RenderIntentDetail(s Snapshot, id string, st Styles, w int, loc *time.Location, stage func(string) int) string {
	if loc == nil {
		loc = time.Local
	}
	if stage == nil {
		stage = func(string) int { return revealDone }
	}
	i := slices.IndexFunc(s.Intents, func(in Intent) bool { return in.ID == id })
	if i < 0 {
		return st.Muted.Render("intent " + id + " is no longer in the project")
	}
	in := s.Intents[i]
	ovs := overlapsOf(s.Overlaps, id)
	contested := []string{}
	for _, o := range ovs {
		contested = append(contested, o.Paths...)
	}

	var b strings.Builder
	line := func(s string) { b.WriteString(clip(s, w)); b.WriteByte('\n') }
	wrap := func(s string, style lipgloss.Style) {
		b.WriteString(style.Width(w).Render(s))
		b.WriteByte('\n')
	}
	wrap(in.Goal, st.Section)
	meta := []string{in.Agent, in.StatusText()}
	if in.Fork != "" {
		meta = append(meta, "fork "+in.Fork)
	}
	if !in.CreatedAt.IsZero() {
		meta = append(meta, "declared "+clock(in.CreatedAt, loc))
	}
	if in.Jurisdiction != "" {
		meta = append(meta, "jurisdiction "+strings.ToUpper(in.Jurisdiction))
	}
	line(st.Muted.Render(strings.Join(meta, " · ")))
	wrap("Why: "+cmpOr(in.Reason, "(no reason recorded)"), st.Text)
	if in.Status == IntentAbandoned && in.AbandonedReason != "" {
		wrap("Abandoned: "+in.AbandonedReason, st.Muted)
	}
	if in.StatusText() == "stale" {
		wrap("Stale: no push for a while. Its author can push, or run 'wayglass intent abandon "+in.ID+" --reason ...'.", st.Muted)
	}
	line("")

	// Paths: declared patterns, then pushed files no literal declaration names.
	line(st.Section.Render("PATHS") + st.Muted.Render("  declared against pushed"))
	pathW := max(12, min(48, w-34))
	line(st.Header.Render(joinCols(fit("PATH", pathW), fit("DECLARED", 8), fit("PUSHED", 9), "NOTE")))
	isContested := func(p string) bool {
		return slices.ContainsFunc(contested, func(c string) bool { return intent.PatternsIntersect(c, p) })
	}
	note := func(p string) string {
		if isContested(p) {
			return st.Title.Render("⇄ overlap")
		}
		return ""
	}
	if len(in.DeclaredPaths) == 0 && len(in.ActualPaths) == 0 {
		line(st.Muted.Render("no paths declared or pushed"))
	}
	for _, d := range in.DeclaredPaths {
		pushed := "-"
		n := 0
		for _, a := range in.ActualPaths {
			if intent.PatternsIntersect(d, a) {
				n++
			}
		}
		switch {
		case strings.ContainsAny(d, "*?") && n > 0:
			pushed = fmt.Sprintf("%d file(s)", n)
		case n > 0:
			pushed = "yes"
		}
		line(joinCols(st.Text.Render(fit(d, pathW)), st.Muted.Render(fit("yes", 8)), st.Text.Render(fit(pushed, 9)), note(d)))
	}
	undeclared := in.Undeclared()
	for _, a := range in.ActualPaths {
		if slices.Contains(in.DeclaredPaths, a) {
			continue
		}
		decl, ds, n := "via glob", st.Muted, note(a)
		if slices.Contains(undeclared, a) {
			decl, ds = "no", st.Stopped
			n = strings.TrimSpace(st.Stopped.Render("! undeclared") + " " + n)
		}
		line(joinCols(st.Text.Render(fit(a, pathW)), ds.Render(fit(decl, 8)), st.Text.Render(fit("yes", 9)), n))
	}
	line("")

	// The change and its decision.
	line(st.Section.Render("CHANGE") + st.Muted.Render("  tags as classified"))
	if d, ok := latestDecision(s.Decisions, id); ok {
		sg := stage(d.ID)
		t := tagParts(d.Tags)
		labels := [4]string{"area", "data class", "risk", "jurisdiction"}
		var tags []string
		for i, v := range t {
			if sg <= i {
				v = "…"
			}
			tags = append(tags, st.Muted.Render(labels[i]+" ")+st.Text.Render(v))
		}
		line(strings.Join(tags, st.Muted.Render(" · ")))
		if sg < revealDone {
			line(st.Muted.Render("classifying…"))
		} else {
			line(st.status(d).Render(actionLabel(d.Action)) + st.Muted.Render(" by rule ") + st.Text.Render(d.MatchedRule) +
				st.Muted.Render(" · ") + st.status(d).Render(statusLabel(d)))
			wrap("Rule's reason: "+cmpOr(d.Reason, "(none)"), st.Muted)
			if e := d.Escalation; e != nil {
				wrap("Escalated by "+aiName(cmpOr(e.Model, AIModel(e.By)))+": "+e.Reason, st.AI)
				for _, c := range e.Concerns {
					wrap("  • "+c, st.Text)
				}
			}
			for _, a := range d.Approvals {
				by := a.By
				if a.AI() {
					by = aiName(AIModel(a.By))
				}
				wrap(fmt.Sprintf("%s by %s at %s: %s", a.Verdict, by, clock(a.At, loc), a.Reason), st.Text)
			}
			if a := d.AIAdvice; a != nil {
				wrap("AI advice ("+aiName(a.Model)+", advisory): "+a.Summary, st.AI)
				for _, grp := range []struct {
					name  string
					items []string
				}{{"risk", a.Risks}, {"concern", a.Concerns}, {"question", a.Questions}} {
					for _, it := range grp.items {
						wrap("  • "+grp.name+": "+it, st.Text)
					}
				}
			}
			add, del := 0, 0
			for _, f := range d.Files {
				add += f.Added
				del += f.Deleted
			}
			if d.LinesUnknown {
				line(st.Muted.Render(fmt.Sprintf("%d file(s), line counts not recorded", len(d.Files))))
			} else {
				line(st.Muted.Render(fmt.Sprintf("%d file(s) +%d -%d", len(d.Files), add, del)))
			}
		}
	} else {
		line(st.Muted.Render("not proposed yet: tags appear when the change is classified"))
	}
	line("")

	line(st.Section.Render("OVERLAPS"))
	if len(ovs) == 0 {
		line(st.Muted.Render("none"))
	}
	for _, o := range ovs {
		other := o.B
		if o.B == id {
			other = o.A
		}
		desc := other
		if j := slices.IndexFunc(s.Intents, func(x Intent) bool { return x.ID == other }); j >= 0 {
			desc = fmt.Sprintf("%s (%s: %s)", other, s.Intents[j].Agent, s.Intents[j].Goal)
		}
		line(st.Title.Render("⇄ ") + st.Text.Render(desc))
		line("  " + st.Muted.Render("on ") + st.Text.Render(strings.Join(o.Paths, ", ")))
		b.WriteString(st.Text.Width(w).PaddingLeft(2).Render("suggested re-plan: " + o.Replan.Summary))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
