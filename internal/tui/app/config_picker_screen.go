package app

import (
	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/tui/menu"
)

// configPickerScreen is the first screen shown when op-ctl is invoked
// without --config and config.yaml lists more than one chain. It wraps
// menu.Model and translates the inner menu.SelectedMsg into a
// configSelectedMsg carrying the chosen chain's TOML path;
// menu.CanceledMsg becomes popMsg so esc/ctrl+c at the picker quits
// the program cleanly (popMsg on a single-stack App quits via
// handlePop).
//
// Living inside the App's screen stack keeps the picker and the main
// menu inside one tea.Program — no alt-screen exit/enter flicker
// between the two.
type configPickerScreen struct {
	inner   menu.Model
	entries []config.ChainEntry
}

func newConfigPickerScreen(entries []config.ChainEntry) configPickerScreen {
	items := make([]menu.Item, len(entries))
	for i, e := range entries {
		items[i] = menu.Item{Name: e.Name, Short: e.ConfigPath}
	}
	return configPickerScreen{
		inner:   menu.NewWithTitle("op-ctl / select chain", items),
		entries: entries,
	}
}

func (s configPickerScreen) Init() tea.Cmd { return s.inner.Init() }

func (s configPickerScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.inner.Update(msg)
	s.inner = next.(menu.Model)
	entries := s.entries
	return s, translate(cmd, func(m tea.Msg) tea.Msg {
		switch v := m.(type) {
		case menu.SelectedMsg:
			for _, e := range entries {
				if e.Name == v.Name {
					return configSelectedMsg{path: e.ConfigPath}
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
