package app

import (
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/tui/menu"
)

// configPickerScreen is the first screen shown when op-ctl is invoked
// without --config and the discovery directory holds more than one
// *.toml file. It wraps menu.Model and translates the inner
// menu.SelectedMsg into a configSelectedMsg carrying the chosen
// absolute path; menu.CanceledMsg becomes popMsg so esc/ctrl+c at the
// picker quits the program cleanly (popMsg on a single-stack App quits
// via handlePop).
//
// Living inside the App's screen stack keeps the picker and the main
// menu inside one tea.Program — no alt-screen exit/enter flicker
// between the two.
type configPickerScreen struct {
	inner menu.Model
	paths []string // absolute paths, indexed by item name lookup
}

func newConfigPickerScreen(paths []string) configPickerScreen {
	items := make([]menu.Item, len(paths))
	for i, p := range paths {
		items[i] = menu.Item{Name: filepath.Base(p), Short: p}
	}
	return configPickerScreen{
		inner: menu.NewWithTitle("op-ctl / select config", items),
		paths: paths,
	}
}

func (s configPickerScreen) Init() tea.Cmd { return s.inner.Init() }

func (s configPickerScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.inner.Update(msg)
	s.inner = next.(menu.Model)
	paths := s.paths
	return s, translate(cmd, func(m tea.Msg) tea.Msg {
		switch v := m.(type) {
		case menu.SelectedMsg:
			for _, p := range paths {
				if filepath.Base(p) == v.Name {
					return configSelectedMsg{path: p}
				}
			}
			return popMsg{}
		case menu.CanceledMsg:
			return popMsg{}
		}
		return m
	})
}

func (s configPickerScreen) View() string { return s.inner.View() }
