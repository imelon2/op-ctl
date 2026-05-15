package peers

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
)

func sampleDump() *opnode.PeerDump {
	return &opnode.PeerDump{
		TotalConnected: 1,
		Peers: map[string]opnode.PeerEntry{
			"16Uiu2HAm-known-connected": {
				PeerID: "16Uiu2HAm-known-connected", Connectedness: 1, Direction: 1, Latency: 12_000_000,
			},
			"16Uiu2HAm-known-cached": {
				PeerID: "16Uiu2HAm-known-cached", Connectedness: 0, Direction: 2,
			},
			"16Uiu2HAm-stranger": {
				PeerID: "16Uiu2HAm-stranger", Connectedness: 3, Direction: 0,
			},
		},
		BannedPeers: []string{"16Uiu2HAm-known-banned", "16Uiu2HAm-bad-stranger"},
	}
}

func sampleIndex() *namespace.Index {
	return namespace.BuildIndex([]namespace.Entry{
		{Name: "fullnode", Consensus: namespace.Consensus{PeerID: "16Uiu2HAm-known-connected"}},
		{Name: "archive", Consensus: namespace.Consensus{PeerID: "16Uiu2HAm-known-cached"}},
		{Name: "blocked", Consensus: namespace.Consensus{PeerID: "16Uiu2HAm-known-banned"}},
	})
}

func sampleModel() Model {
	return New(
		config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004"},
		sampleDump(),
		sampleIndex(),
	)
}

func sized(m Model, w, h int) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(Model)
}

func TestNewBucketsAndSorts(t *testing.T) {
	m := sampleModel()
	if got, want := len(m.connected), 1; got != want {
		t.Fatalf("connected: got %d, want %d", got, want)
	}
	if m.connected[0].name != "fullnode" {
		t.Errorf("connected[0].name = %q, want %q", m.connected[0].name, "fullnode")
	}
	// Other should be archive (named) before stranger (unnamed)
	if got, want := len(m.other), 2; got != want {
		t.Fatalf("other: got %d, want %d", got, want)
	}
	if m.other[0].name != "archive" {
		t.Errorf("other[0].name = %q, want named first", m.other[0].name)
	}
	if m.other[1].name != "" {
		t.Errorf("other[1].name = %q, want unnamed last", m.other[1].name)
	}
	// Banned: blocked (named) before bad-stranger (unnamed)
	if got, want := len(m.banned), 2; got != want {
		t.Fatalf("banned: got %d, want %d", got, want)
	}
	if m.banned[0].name != "blocked" {
		t.Errorf("banned[0].name = %q, want named first", m.banned[0].name)
	}
}

func TestViewContainsHeaderBadgesAndNames(t *testing.T) {
	m := sized(sampleModel(), 100, 30)
	out := stripANSI(m.View())

	for _, want := range []string{
		"sequencer",
		"http://127.0.0.1:4004",
		"connected 1",
		"known 3",
		"banned 2",
		"Connected",
		"Other",
		"Banned",
		"fullnode",
		"archive",
		"blocked",
		"Inbound",
		"NotConnected",
		"CannotConnect",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q\n---\n%s", want, out)
		}
	}
}

func TestQuitKeys(t *testing.T) {
	for _, key := range []string{"q", "esc", "ctrl+c"} {
		m := sized(sampleModel(), 100, 30)
		next, cmd := m.Update(parseKey(key))
		if cmd == nil {
			t.Errorf("%s: expected tea.Quit cmd", key)
		}
		_ = next
	}
}

func TestCursorClampsAtZero(t *testing.T) {
	m := sized(sampleModel(), 100, 30)
	next, _ := m.Update(parseKey("k"))
	if got := next.(Model).cursor; got != 0 {
		t.Errorf("k from top: cursor = %d, want 0", got)
	}
}

func TestCursorDownAdvancesAndClamps(t *testing.T) {
	m := sized(sampleModel(), 100, 30)
	next, _ := m.Update(parseKey("j"))
	if got := next.(Model).cursor; got != 1 {
		t.Errorf("j: cursor = %d, want 1", got)
	}
	// Walk past the end; cursor should clamp to len(rows)-1.
	for i := 0; i < 50; i++ {
		next, _ = next.(Model).Update(parseKey("j"))
	}
	last := len(next.(Model).rows) - 1
	if got := next.(Model).cursor; got != last {
		t.Errorf("after many j: cursor = %d, want %d", got, last)
	}
}

func TestEnterEmitsSelectedMsgWithEntry(t *testing.T) {
	m := sized(sampleModel(), 100, 30)
	// Cursor starts at 0 (first connected row, fullnode).
	_, cmd := m.Update(parseKey("enter"))
	if cmd == nil {
		t.Fatal("enter should return a SelectedMsg cmd")
	}
	got, ok := cmd().(SelectedMsg)
	if !ok {
		t.Fatalf("enter cmd: got %T, want SelectedMsg", cmd())
	}
	if got.PeerID != "16Uiu2HAm-known-connected" {
		t.Errorf("PeerID = %q, want %q", got.PeerID, "16Uiu2HAm-known-connected")
	}
	if got.Name != "fullnode" {
		t.Errorf("Name = %q, want %q", got.Name, "fullnode")
	}
	if got.Entry == nil {
		t.Errorf("Entry should be non-nil for connected peer")
	}
	if got.Banned {
		t.Errorf("Banned should be false for connected peer")
	}
}

func TestEnterOnBannedPeerEmitsBannedTrue(t *testing.T) {
	m := sized(sampleModel(), 100, 30)
	// Walk down to the banned section. sample has 1 connected, 2 other,
	// 2 banned → banned starts at cursor=3.
	for i := 0; i < 3; i++ {
		next, _ := m.Update(parseKey("j"))
		m = next.(Model)
	}
	_, cmd := m.Update(parseKey("enter"))
	got := cmd().(SelectedMsg)
	if !got.Banned {
		t.Errorf("Banned = false for banned peer")
	}
	if got.Entry != nil {
		t.Errorf("Entry should be nil for banned peer")
	}
}

// TestSnapshotPrintsView is a developer aid: run with `go test -v -run
// SnapshotPrintsView` to eyeball the layout without launching a real
// TTY. It prints the ANSI-stripped View at 100x30 to t.Log and never
// fails.
func TestSnapshotPrintsView(t *testing.T) {
	m := sized(sampleModel(), 100, 30)
	t.Logf("\n%s", stripANSI(m.View()))
}

// parseKey constructs a simple tea.KeyMsg from a string. Handles a few
// special names; everything else is treated as a rune key.
func parseKey(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// stripANSI removes ANSI escape sequences so substring assertions
// don't have to worry about color codes injected by lipgloss.
func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	in := false
	for _, r := range s {
		if r == 0x1b {
			in = true
			continue
		}
		if in {
			if (r >= '@' && r <= '~') {
				in = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
