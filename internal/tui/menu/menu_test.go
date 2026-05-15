package menu

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func feed(m Model, keys ...string) Model {
	for _, k := range keys {
		next, _ := m.Update(key(k))
		m = next.(Model)
	}
	return m
}

func TestEnterPicksFirstItem(t *testing.T) {
	m := New([]Item{{Name: "list", Short: "show namespace"}, {Name: "namespace", Short: "snapshot"}})
	m = feed(m, "enter")
	if got := m.Chosen(); got != "list" {
		t.Fatalf("Chosen: got %q, want %q", got, "list")
	}
}

func TestDownEnterPicksSecondItem(t *testing.T) {
	m := New([]Item{{Name: "list"}, {Name: "namespace"}})
	m = feed(m, "down", "enter")
	if got := m.Chosen(); got != "namespace" {
		t.Fatalf("Chosen: got %q, want %q", got, "namespace")
	}
}

func TestJKNavigation(t *testing.T) {
	m := New([]Item{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	m = feed(m, "j", "j", "enter")
	if got := m.Chosen(); got != "c" {
		t.Fatalf("after jj enter: got %q, want %q", got, "c")
	}
	m = New([]Item{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	m = feed(m, "j", "j", "k", "enter")
	if got := m.Chosen(); got != "b" {
		t.Fatalf("after jjk enter: got %q, want %q", got, "b")
	}
}

func TestCursorClampedAtBounds(t *testing.T) {
	m := New([]Item{{Name: "a"}, {Name: "b"}})
	// Up from cursor=0 must stay at 0.
	m = feed(m, "k", "k", "k", "enter")
	if got := m.Chosen(); got != "a" {
		t.Fatalf("up-clamp: got %q, want %q", got, "a")
	}
	m = New([]Item{{Name: "a"}, {Name: "b"}})
	// Down past last item must stay at last.
	m = feed(m, "j", "j", "j", "j", "enter")
	if got := m.Chosen(); got != "b" {
		t.Fatalf("down-clamp: got %q, want %q", got, "b")
	}
}

func TestQuitWithoutChoice(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		m := New([]Item{{Name: "list"}})
		m = feed(m, k)
		if got := m.Chosen(); got != "" {
			t.Errorf("%s: Chosen=%q, want empty", k, got)
		}
	}
}

func TestEnterOnEmptyDoesNothing(t *testing.T) {
	m := New(nil)
	m = feed(m, "enter")
	if got := m.Chosen(); got != "" {
		t.Errorf("empty enter: Chosen=%q, want empty", got)
	}
}

func TestViewRendersNamesAndCursor(t *testing.T) {
	m := New([]Item{{Name: "list", Short: "show ns"}, {Name: "namespace", Short: "snapshot"}})
	m = feed(m, "down")
	out := m.View()
	if !strings.Contains(out, "list") || !strings.Contains(out, "namespace") {
		t.Fatalf("View missing item names:\n%s", out)
	}
	if !strings.Contains(out, "▸") {
		t.Errorf("View missing cursor marker:\n%s", out)
	}
}
