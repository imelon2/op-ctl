// Package errscreen renders a message in alt-screen until the user
// presses any key.
//
// Used as a transient overlay between menu loops so an RPC failure
// doesn't dump to stderr (where alt-screen would obscure it) — the
// operator sees the error, dismisses it, and lands back at the menu.
package errscreen

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/tui/theme"
)

// DoneMsg is emitted when the operator dismisses the error overlay.
// The unified-app program routes this to "pop screen"; standalone
// Run() converts it into tea.Quit.
type DoneMsg struct{}

// Model holds the error message + the most recently seen window size.
// Construct via New so the message is set; the zero value renders an
// empty box.
type Model struct {
	msg    string
	width  int
	height int
}

// New builds a Model around an error message string.
func New(msg string) Model { return Model{msg: msg} }

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		_ = msg
		return m, func() tea.Msg { return DoneMsg{} }
	}
	return m, nil
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return m.msg
	}
	innerW := m.width - 6
	if innerW < 20 {
		innerW = 20
	}
	body := wrap(m.msg, innerW)
	content := theme.ErrTitle.Render("Error") + "\n\n" + body + "\n\n" + theme.Help.Render("press any key to continue")
	return theme.ErrorBox.Width(innerW).Render(content)
}

// wrap is a minimal width-based word wrap. lipgloss has no built-in
// wrap and pulling reflow for one screen would be overkill.
func wrap(s string, w int) string {
	if w <= 0 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		col := 0
		for _, word := range strings.Fields(line) {
			if col == 0 {
				out.WriteString(word)
				col = len(word)
				continue
			}
			if col+1+len(word) > w {
				out.WriteByte('\n')
				out.WriteString(word)
				col = len(word)
				continue
			}
			out.WriteByte(' ')
			out.WriteString(word)
			col += 1 + len(word)
		}
	}
	return out.String()
}

// runner wraps Model so the standalone Run() can quit on DoneMsg.
type runner struct{ inner Model }

func (r runner) Init() tea.Cmd { return r.inner.Init() }

func (r runner) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(DoneMsg); ok {
		return r, tea.Quit
	}
	next, cmd := r.inner.Update(msg)
	r.inner = next.(Model)
	return r, cmd
}

func (r runner) View() string { return r.inner.View() }

// Run shows msg in alt-screen and returns when the user presses any
// key. Kept for one-off CLI flows; the unified app program embeds
// Model directly and handles DoneMsg itself.
func Run(msg string) error {
	_, err := tea.NewProgram(runner{inner: New(msg)}, tea.WithAltScreen()).Run()
	return err
}
