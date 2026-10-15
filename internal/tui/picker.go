package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

// ProjectSwitcher is a DataSource that can list the projects its key may
// read and point itself at another one. `wayglass watch` and `review` show
// a picker on `p` (or start in it with --pick) when the source offers it;
// the sample source does not.
type ProjectSwitcher interface {
	DataSource
	// Projects lists every project the key can read, in directory order.
	Projects(ctx context.Context) ([]ProjectItem, error)
	// Switch returns a source for the named project. The returned source
	// must also be a ProjectSwitcher, so the picker keeps working.
	Switch(p ProjectItem) DataSource
}

// ProjectItem is one row of the picker: the directory row a person needs
// to choose a project.
type ProjectItem struct {
	Name         string    `json:"name"`
	Jurisdiction string    `json:"jurisdiction"`
	Open         int       `json:"open"`
	PendingHuman int       `json:"pending_human"`
	Landed       int       `json:"landed"`
	LastActivity time.Time `json:"last_activity"`
	// Residency is "" when the project is served, else why it is not
	// (residency_pending, project_moved); such a project cannot be opened.
	Residency string `json:"residency,omitempty"`
}

// Title implements list.DefaultItem.
func (i ProjectItem) Title() string {
	if j := strings.ToUpper(i.Jurisdiction); j != "" {
		return i.Name + "  " + j
	}
	return i.Name
}

// Description implements list.DefaultItem: the numbers a person picks by.
func (i ProjectItem) Description() string {
	if i.Residency != "" {
		return "not served: " + i.Residency + " (an admin runs wayglass project migrate-residency)"
	}
	parts := []string{fmt.Sprintf("%d open", i.Open)}
	if i.PendingHuman > 0 {
		parts = append(parts, fmt.Sprintf("▲ %d for a person", i.PendingHuman))
	}
	parts = append(parts, fmt.Sprintf("%d landed", i.Landed))
	if !i.LastActivity.IsZero() {
		parts = append(parts, "active "+ago(time.Now(), i.LastActivity))
	}
	return strings.Join(parts, " · ")
}

// FilterValue implements list.Item: `/` fuzzy-matches the name.
func (i ProjectItem) FilterValue() string { return i.Name }

// Openable reports whether the project can be watched: its state is where
// its jurisdiction says.
func (i ProjectItem) Openable() bool { return i.Residency == "" }

// projectsMsg carries the directory listing into the model.
type projectsMsg struct {
	items []ProjectItem
	err   error
}

// pickedMsg is sent by the picker when a project was chosen.
type pickedMsg struct{ item ProjectItem }

// loadProjects lists the projects, or says that this source cannot.
func loadProjects(ds DataSource) tea.Cmd {
	sw, ok := ds.(ProjectSwitcher)
	if !ok {
		return func() tea.Msg { return projectsMsg{err: ErrNoDirectory} }
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		items, err := sw.Projects(ctx)
		return projectsMsg{items: items, err: err}
	}
}

// ErrNoDirectory is the picker's answer on a source without a directory
// (the built-in sample).
var ErrNoDirectory = errNoDirectory{}

type errNoDirectory struct{}

func (errNoDirectory) Error() string {
	return "this data source has no project directory; connect to a Worker to pick a project"
}

// pickerKeys are the picker's own keys; the list owns movement and `/`.
type pickerKeys struct {
	Open, Close key.Binding
}

func newPickerKeys() pickerKeys {
	return pickerKeys{
		Open:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open project")),
		Close: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
	}
}

// picker is the project list overlay shared by watch and review.
type picker struct {
	open    bool
	loading bool
	err     error
	items   []ProjectItem
	list    list.Model
	keys    pickerKeys
	width   int
	height  int
}

func newPicker(st Styles, isDark bool) picker {
	d := list.NewDefaultDelegate()
	d.Styles = list.NewDefaultItemStyles(isDark)
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(st.Selected.GetForeground()).BorderForeground(st.Selected.GetForeground())
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Foreground(st.Selected.GetForeground()).BorderForeground(st.Selected.GetForeground())
	l := list.New(nil, d, 0, 0)
	l.Title = "Projects"
	l.Styles = list.DefaultStyles(isDark)
	l.Styles.Title = st.Brand.Padding(0, 1)
	l.SetStatusBarItemName("project", "projects")
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	l.SetShowHelp(true)
	return picker{list: l, keys: newPickerKeys()}
}

// restyle follows a background colour change.
func (p *picker) restyle(st Styles, isDark bool) {
	items := p.list.Items()
	idx := p.list.Index()
	fresh := newPicker(st, isDark)
	fresh.open, fresh.loading, fresh.err, fresh.items = p.open, p.loading, p.err, p.items
	fresh.list.SetItems(items)
	fresh.list.Select(idx)
	fresh.setSize(p.width, p.height)
	*p = fresh
}

// show opens the picker and starts loading the directory.
func (p *picker) show(ds DataSource) tea.Cmd {
	p.open, p.loading, p.err = true, true, nil
	p.list.ResetFilter()
	return loadProjects(ds)
}

func (p *picker) hide() { p.open = false }

func (p *picker) setSize(w, h int) {
	p.width, p.height = w, h
	// The list sits in a pane: border, padding, and one status line of ours.
	p.list.SetSize(max(paneInner(w), 10), max(h-4, 3))
}

// apply stores a listing; the current project is selected first.
func (p *picker) apply(msg projectsMsg, current string) tea.Cmd {
	p.loading = false
	p.err = msg.err
	if msg.err != nil {
		return nil
	}
	p.items = msg.items
	items := make([]list.Item, 0, len(msg.items))
	sel := 0
	for i, it := range msg.items {
		items = append(items, it)
		if it.Name == current {
			sel = i
		}
	}
	cmd := p.list.SetItems(items)
	p.list.Select(sel)
	return cmd
}

// update routes a key to the picker. It returns a pickedMsg command when a
// project was chosen, and closes on esc unless a filter is being typed.
func (p *picker) update(k tea.KeyPressMsg) tea.Cmd {
	if p.list.SettingFilter() {
		var cmd tea.Cmd
		p.list, cmd = p.list.Update(k)
		return cmd
	}
	switch {
	case key.Matches(k, p.keys.Close):
		if p.list.IsFiltered() {
			p.list.ResetFilter()
			return nil
		}
		p.hide()
		return nil
	case key.Matches(k, p.keys.Open):
		it, ok := p.list.SelectedItem().(ProjectItem)
		if !ok {
			return nil
		}
		if !it.Openable() {
			return p.list.NewStatusMessage(it.Name + " is not served yet: " + it.Residency)
		}
		p.hide()
		return func() tea.Msg { return pickedMsg{item: it} }
	}
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(k)
	return cmd
}

// forward hands the list a message that is not a key (its filter matches
// arrive as one) while the picker is open.
func (p *picker) forward(msg tea.Msg) tea.Cmd {
	if !p.open {
		return nil
	}
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return cmd
}

// view renders the picker pane to w by h cells.
func (p picker) view(st Styles, current string, w, h int) string {
	var body string
	switch {
	case p.loading && len(p.items) == 0:
		body = st.Muted.Render("listing projects…")
	case p.err != nil:
		body = st.Error.Render(p.err.Error())
	case len(p.items) == 0:
		body = st.Muted.Render("No projects on this Worker yet; 'wayglass init' creates one.")
	default:
		body = p.list.View()
	}
	hint := st.Muted.Render("enter opens · / filters · esc closes")
	return st.pane(paneSpec{Title: "projects", Meta: st.Muted.Render("now: " + current), Hint: hint, Body: body, W: w, H: h, Focused: true})
}

// pickerHelp is the binding both views show for the picker.
func pickerHelp() key.Binding {
	return key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "projects"))
}

// switchSource re-points a model at the chosen project. It returns the new
// source, or the old one with ok=false when the source cannot switch.
func switchSource(ds DataSource, it ProjectItem) (DataSource, bool) {
	sw, ok := ds.(ProjectSwitcher)
	if !ok {
		return ds, false
	}
	return sw.Switch(it), true
}

// pickerTitle is the window title while the picker is open.
func pickerTitle(view string) string { return "wayglass " + view + " · projects" }

var _ list.DefaultItem = ProjectItem{}
