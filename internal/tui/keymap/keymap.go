// Package keymap centralises op-ctl's TUI key chords so every screen
// uses the same `r`/`t`/`u`/`q` etc. semantics and the same
// wei/time cycle state. See docs/tui-interactive-keys.md.
package keymap

import (
	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/tui/theme"
)

// Binding pairs one or more tea.KeyMsg.String() values with a footer
// label + description. Side-effects live in the consuming screen.
type Binding struct {
	Keys    []string
	Display string
	Desc    string
}

func (b Binding) Matches(m tea.KeyMsg) bool {
	s := m.String()
	for _, k := range b.Keys {
		if k == s {
			return true
		}
	}
	return false
}

func (b Binding) Hint() theme.Key {
	return theme.Key{Keys: b.Display, Desc: b.Desc}
}

// Navigate and Scroll have empty Keys: footer-only labels that don't
// re-match the j/k chord already handled by Up/Down.
var (
	Refresh   = Binding{Keys: []string{"r"}, Display: "r", Desc: "refresh"}
	Back      = Binding{Keys: []string{"q", "esc"}, Display: "q", Desc: "back"}
	Quit      = Binding{Keys: []string{"q", "esc", "ctrl+c"}, Display: "q/esc", Desc: "quit"}
	Up        = Binding{Keys: []string{"k", "up"}, Display: "↑/k", Desc: "up"}
	Down      = Binding{Keys: []string{"j", "down"}, Display: "↓/j", Desc: "down"}
	Enter     = Binding{Keys: []string{"enter"}, Display: "⏎", Desc: "open"}
	// PageNext / PagePrev intentionally avoid right/l, left/h, and
	// space — some screens (e.g. read_dispute_game_detail) bind
	// right/l/left/h to section navigation and space to toggle. A
	// screen that wants the extra aliases for paging can add them
	// inline next to PageNext.Matches.
	PageNext = Binding{Keys: []string{"pgdown", "ctrl+d", "n"}, Display: "PgDn", Desc: "next page"}
	PagePrev = Binding{Keys: []string{"pgup", "ctrl+u", "b", "p"}, Display: "PgUp", Desc: "prev page"}
	Top       = Binding{Keys: []string{"g", "home"}, Display: "g", Desc: "top"}
	Bottom    = Binding{Keys: []string{"G", "end"}, Display: "G", Desc: "bottom"}
	TimeCycle = Binding{Keys: []string{"t"}, Display: "t", Desc: "time (utc/local/unix)"}
	UnitCycle = Binding{Keys: []string{"u"}, Display: "u", Desc: "unit (wei/gwei/eth)"}

	// Navigate and Scroll are footer-only labels (empty Keys) so they
	// can sit next to Up/Down in the help line without the dispatcher
	// re-matching the same key.
	Navigate = Binding{Display: "↑↓/jk", Desc: "navigate"}
	Scroll   = Binding{Display: "↑↓/jk", Desc: "scroll"}
)

// Footer is a thin wrapper around theme.Footer that takes Bindings
// directly. It lets a screen write `keymap.Footer(keymap.Navigate,
// keymap.Refresh, keymap.Back)` without converting each one to a
// theme.Key in the call site.
func Footer(bindings ...Binding) string {
	hints := make([]theme.Key, 0, len(bindings))
	for _, b := range bindings {
		hints = append(hints, b.Hint())
	}
	return theme.Footer(hints...)
}
