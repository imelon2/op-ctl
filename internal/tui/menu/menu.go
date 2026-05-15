// Package menu is a small bubbletea selector that lets the operator pick
// a registered top-level command when op-ctl is launched with no args.
//
// It is intentionally minimal — no list-component dependency, no
// search/filter, no scrolling — because op-ctl has only a handful of
// commands. If the catalog grows past one screen, swap the View() for
// bubbles/list.
package menu

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Item is one menu row: the command name (returned to the caller as the
// selection result) and a short one-line description.
type Item struct {
	Name  string
	Short string
}

// SelectedMsg is emitted when the operator presses enter on an item.
// CanceledMsg is emitted on q / esc / ctrl+c. Both are routed via
// tea.Cmd so callers (a parent app or the standalone runner) decide
// whether to quit, navigate, or do anything else — Update itself never
// calls tea.Quit.
type SelectedMsg struct{ Name string }
type CanceledMsg struct{}

// Title is rendered above the items. Leave Title empty to hide the
// breadcrumb line; the static "op-ctl" / "select a command" header is
// shown only at the root level.
type Options struct {
	Title string
}

// Model holds the cursor + chosen-item state. Chosen() returns "" when
// the user quit without picking anything.
type Model struct {
	items  []Item
	cursor int
	chosen string

	title string
}

// New builds a Model from an ordered slice of items. Order is preserved
// from the caller — typically the cobra subcommand registration order.
func New(items []Item) Model {
	return Model{items: items}
}

// NewWithTitle builds a Model with a custom breadcrumb-style title in
// place of the default "op-ctl / select a command" header — used by
// embedded contexts like the backend picker that want their own
// labeling.
func NewWithTitle(title string, items []Item) Model {
	return Model{items: items, title: title}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "ctrl+c", "q", "esc":
		return m, func() tea.Msg { return CanceledMsg{} }
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		if len(m.items) > 0 {
			m.cursor = len(m.items) - 1
		}
	case "enter":
		if len(m.items) > 0 {
			m.chosen = m.items[m.cursor].Name
			name := m.chosen
			return m, func() tea.Msg { return SelectedMsg{Name: name} }
		}
	}
	return m, nil
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	cursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selectedStyle = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
)

func (m Model) View() string {
	var b strings.Builder
	if m.title != "" {
		b.WriteString(titleStyle.Render(m.title) + "\n\n")
	} else {
		b.WriteString(titleStyle.Render("op-ctl") + "\n")
		b.WriteString(dimStyle.Render("select a command") + "\n\n")
	}

	nameWidth := 0
	for _, it := range m.items {
		if n := lipgloss.Width(it.Name); n > nameWidth {
			nameWidth = n
		}
	}

	for i, it := range m.items {
		name := it.Name + strings.Repeat(" ", nameWidth-lipgloss.Width(it.Name))
		short := dimStyle.Render(it.Short)
		if i == m.cursor {
			b.WriteString(cursorStyle.Render("▸ "))
			b.WriteString(selectedStyle.Render(name))
		} else {
			b.WriteString("  " + name)
		}
		b.WriteString("  " + short + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ or j/k move · enter run · q/esc quit"))
	b.WriteString("\n")
	return b.String()
}

// Chosen returns the Name of the picked Item, or "" if the user quit.
func (m Model) Chosen() string { return m.chosen }

// runner wraps Model so the standalone Run() can quit on Selected /
// Canceled messages. The embedded-app case uses Model directly and
// handles those messages itself.
type runner struct{ inner Model }

func (r runner) Init() tea.Cmd { return r.inner.Init() }

func (r runner) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case SelectedMsg, CanceledMsg:
		return r, tea.Quit
	}
	next, cmd := r.inner.Update(msg)
	r.inner = next.(Model)
	return r, cmd
}

func (r runner) View() string { return r.inner.View() }

// Run renders the menu in alt-screen mode and returns the chosen item
// name (or "" on quit). Kept for one-off CLI flows that don't go
// through the unified app program.
func Run(items []Item) (string, error) {
	final, err := tea.NewProgram(runner{inner: New(items)}, tea.WithAltScreen()).Run()
	if err != nil {
		return "", fmt.Errorf("menu: %w", err)
	}
	return final.(runner).inner.Chosen(), nil
}
