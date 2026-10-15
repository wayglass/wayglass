package tui

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
)

// watchKeys are the bindings of `wayglass watch`. They drive both the key
// handling and the help line, so the help never lies about a key.
type watchKeys struct {
	Up, Down, Open, Back, PageUp, PageDown, Refresh, Pick, Help, Quit key.Binding
	detail                                                            bool
}

func newWatchKeys() watchKeys {
	return watchKeys{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Open:     key.NewBinding(key.WithKeys("enter", "l", "right"), key.WithHelp("enter", "intent details")),
		Back:     key.NewBinding(key.WithKeys("esc", "h", "left", "backspace"), key.WithHelp("esc", "back")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("pgup", "scroll up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "space", "f"), key.WithHelp("pgdn", "scroll down")),
		Refresh:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Pick:     pickerHelp(),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "keys")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp implements help.KeyMap.
func (k watchKeys) ShortHelp() []key.Binding {
	if k.detail {
		return []key.Binding{k.Back, k.Up, k.Down, k.PageDown, k.Help, k.Quit}
	}
	return []key.Binding{k.Up, k.Down, k.Open, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k watchKeys) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Open, k.Back},
		{k.PageUp, k.PageDown, k.Refresh},
		{k.Pick, k.Help, k.Quit},
	}
}

// reviewKeys are the bindings of `wayglass review`.
type reviewKeys struct {
	Up, Down, Approve, Reject, AskAI, Filter, PageUp, PageDown, Refresh, Pick, Help, Quit key.Binding
	// Submit and Cancel only show while the reason form is open.
	Submit, Cancel key.Binding
	form           bool
	// ai shows the AskAI key: only sources that can reach the AI reviewer.
	ai bool
}

func newReviewKeys() reviewKeys {
	return reviewKeys{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Approve:  key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
		Reject:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "reject")),
		AskAI:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "ask AI")),
		Filter:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "show all")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "b", "K", "ctrl+u"), key.WithHelp("pgup", "scroll up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "space", "J", "ctrl+d"), key.WithHelp("pgdn", "scroll down")),
		Refresh:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Pick:     pickerHelp(),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "keys")),
		Quit:     key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q", "quit")),
		Submit:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "record verdict")),
		Cancel:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

// ShortHelp implements help.KeyMap.
func (k reviewKeys) ShortHelp() []key.Binding {
	if k.form {
		return []key.Binding{k.Submit, k.Cancel}
	}
	if k.ai {
		return []key.Binding{k.Up, k.Down, k.Approve, k.Reject, k.AskAI, k.Filter, k.PageDown, k.Help, k.Quit}
	}
	return []key.Binding{k.Up, k.Down, k.Approve, k.Reject, k.Filter, k.PageDown, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k reviewKeys) FullHelp() [][]key.Binding {
	if k.form {
		return [][]key.Binding{{k.Submit, k.Cancel}}
	}
	decide := []key.Binding{k.Approve, k.Reject}
	if k.ai {
		decide = append(decide, k.AskAI)
	}
	return [][]key.Binding{
		{k.Up, k.Down, k.Filter},
		decide,
		{k.PageUp, k.PageDown, k.Refresh},
		{k.Pick, k.Help, k.Quit},
	}
}

// newHelp builds a help view in the TUI palette.
func newHelp(st Styles) help.Model {
	h := help.New()
	h.Styles.ShortKey = st.Key
	h.Styles.ShortDesc = st.Muted
	h.Styles.ShortSeparator = st.Muted
	h.Styles.FullKey = st.Key
	h.Styles.FullDesc = st.Muted
	h.Styles.FullSeparator = st.Muted
	h.Styles.Ellipsis = st.Muted
	h.ShortSeparator = " · "
	return h
}
