package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
)

func txpBackends() []config.Backend {
	return []config.Backend{
		{Name: "seq-1", ExecutionRPCURL: "http://127.0.0.1:8545"},
		{Name: "ingress-1", ExecutionRPCURL: "http://127.0.0.1:8546"},
		{Name: "ingress-2", ExecutionRPCURL: "http://127.0.0.1:8547"},
	}
}

func feedTxP(s statusTxPoolScreen, msgs ...tea.Msg) statusTxPoolScreen {
	for _, m := range msgs {
		next, _ := s.Update(m)
		s = next.(statusTxPoolScreen)
	}
	return s
}

func TestStatusTxPoolScreen_View_InitialAllPolling(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	out := stripANSI(s.View())
	for _, name := range []string{"seq-1", "ingress-1", "ingress-2"} {
		if !strings.Contains(out, name) {
			t.Errorf("View should list backend %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "pending") || !strings.Contains(out, "queued") {
		t.Errorf("header row should contain 'pending' and 'queued':\n%s", out)
	}
	if got := strings.Count(out, "polling"); got < 3 {
		t.Errorf("expected >=3 polling rows, got %d:\n%s", got, out)
	}
}

func TestStatusTxPoolScreen_View_FoldInOK(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	s = feedTxP(s, txpoolTickMsg{gen: 1})
	s = feedTxP(s,
		txpoolSnapshotMsg{gen: 1, backendIdx: 0,
			status:     &elnode.TxPoolStatus{Pending: 123, Queued: 4},
			latency:    50 * time.Millisecond,
			observedAt: time.Now(),
		},
		txpoolSnapshotMsg{gen: 1, backendIdx: 1,
			status:     &elnode.TxPoolStatus{Pending: 120, Queued: 4},
			latency:    80 * time.Millisecond,
			observedAt: time.Now(),
		},
		txpoolSnapshotMsg{gen: 1, backendIdx: 2,
			status:     &elnode.TxPoolStatus{Pending: 124, Queued: 5},
			latency:    60 * time.Millisecond,
			observedAt: time.Now(),
		},
	)
	out := stripANSI(s.View())

	// Per-backend values must appear, with totals computed as
	// pending + queued.
	for _, want := range []string{
		"123", "4", "127", // seq-1
		"120", "124", // ingress-1 pending + total
		"124", "5", "129", // ingress-2
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View should contain value %q:\n%s", want, out)
		}
	}
	// Three OK rows → three ✓ glyphs.
	if got := strings.Count(out, "✓"); got < 3 {
		t.Errorf("expected >=3 ✓ glyphs (one per OK backend), got %d:\n%s", got, out)
	}
}

func TestStatusTxPoolScreen_View_FoldInERR(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	s = feedTxP(s, txpoolTickMsg{gen: 1})
	s = feedTxP(s,
		txpoolSnapshotMsg{gen: 1, backendIdx: 0,
			status:     &elnode.TxPoolStatus{Pending: 100, Queued: 0},
			latency:    50 * time.Millisecond,
			observedAt: time.Now(),
		},
		txpoolSnapshotMsg{gen: 1, backendIdx: 1,
			err:        errors.New("connection refused"),
			observedAt: time.Now(),
		},
		txpoolSnapshotMsg{gen: 1, backendIdx: 2,
			status:     &elnode.TxPoolStatus{Pending: 98, Queued: 1},
			latency:    60 * time.Millisecond,
			observedAt: time.Now(),
		},
	)
	out := stripANSI(s.View())
	if !strings.Contains(out, "ERR connection refused") {
		t.Errorf("failed backend should show 'ERR connection refused':\n%s", out)
	}
	if !strings.Contains(out, "100") {
		t.Errorf("seq-1 pending=100 should render:\n%s", out)
	}
	if !strings.Contains(out, "99") {
		t.Errorf("ingress-2 total=99 (98+1) should render:\n%s", out)
	}
}

// TestStatusTxPoolScreen_Update_StaleSnapshotDropped exercises the
// gen-counter guard: a snapshot tagged with an older generation must
// be ignored entirely.
func TestStatusTxPoolScreen_Update_StaleSnapshotDropped(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	s = feedTxP(s, txpoolTickMsg{gen: 1}, txpoolTickMsg{gen: 2})
	if s.gen != 2 {
		t.Fatalf("setup: s.gen should be 2, got %d", s.gen)
	}
	s = feedTxP(s, txpoolSnapshotMsg{gen: 1, backendIdx: 0,
		status: &elnode.TxPoolStatus{Pending: 999},
	})
	if !s.snapshots[0].pending {
		t.Errorf("stale snapshot should NOT have toggled pending; row 0 was overwritten with %+v", s.snapshots[0])
	}
	if s.snapshots[0].status != nil {
		t.Errorf("stale snapshot should have been dropped; got status=%+v", s.snapshots[0].status)
	}
}

func TestStatusTxPoolScreen_QEmitsPopMsg(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should emit a tea.Cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("q cmd: got %T, want popMsg", cmd())
	}
}

// TestHandleCmdSelected_TxPoolUnderStatus verifies the menu-path
// dispatch lands on statusTxPoolScreen when `txpool` is chosen from
// inside the `status` parent menu.
func TestHandleCmdSelected_TxPoolUnderStatus(t *testing.T) {
	root := &cobra.Command{Use: "op-ctl"}
	statusRoot := &cobra.Command{Use: "status"}
	txpoolLeaf := &cobra.Command{
		Use:  "txpool",
		RunE: func(cmd *cobra.Command, args []string) error { return nil },
	}
	statusRoot.AddCommand(txpoolLeaf)
	root.AddCommand(statusRoot)

	cfg := stubConfig(t, "sequencer")
	a := New(root, cfg, nil, t.TempDir(), 5*time.Second)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	a.stack[0] = newCmdMenu(statusRoot, "status")

	next, _ := a.Update(cmdSelectedMsg{name: "txpool"})
	a = next.(App)

	if got := len(a.stack); got != 2 {
		t.Fatalf("stack depth after txpool: got %d, want 2", got)
	}
	if _, ok := a.stack[1].(statusTxPoolScreen); !ok {
		t.Errorf("top of stack: got %T, want statusTxPoolScreen", a.stack[1])
	}
}

// TestStatusTxPoolScreen_EnterEmitsDetailMsg verifies the drill-down
// entry point: navigate to a backend via j, press enter, and the
// emitted message must be a txpoolDetailMsg pointed at the cursor's
// backend.
func TestStatusTxPoolScreen_EnterEmitsDetailMsg(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	s = feedTxP(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	s = feedTxP(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a populated screen should emit a cmd")
	}
	msg, ok := cmd().(txpoolDetailMsg)
	if !ok {
		t.Fatalf("enter cmd: got %T, want txpoolDetailMsg", cmd())
	}
	if msg.backend.Name != "ingress-2" {
		t.Errorf("emitted backend: got %q, want ingress-2 (cursor=2 after 2× j)", msg.backend.Name)
	}
}

// TestStatusTxPoolScreen_EnterEmpty_NoOp confirms enter on a screen
// with no backends is a no-op (no panic, no message).
func TestStatusTxPoolScreen_EnterEmpty_NoOp(t *testing.T) {
	s := newStatusTxPoolScreen(nil, nil, time.Second, 5*time.Second)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("enter on empty backends should emit no cmd, got %T", cmd())
	}
}

// TestStatusTxPoolScreen_CursorClamps exercises the boundary clamps:
// up at cursor=0 stays at 0; repeated j past the end stays at len-1.
func TestStatusTxPoolScreen_CursorClamps(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	// up at the top: cursor stays 0.
	s = feedTxP(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.cursor != 0 {
		t.Errorf("cursor at top + k: got %d, want 0", s.cursor)
	}
	// j past the end (3 backends, indices 0..2): stays at 2.
	for i := 0; i < 5; i++ {
		s = feedTxP(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	}
	if s.cursor != 2 {
		t.Errorf("cursor after 5× j (len=3): got %d, want 2", s.cursor)
	}
}

// TestStatusTxPoolScreen_CursorVisible asserts the cursor glyph "▸ "
// appears in the rendered View on the active row.
func TestStatusTxPoolScreen_CursorVisible(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	out := stripANSI(s.View())
	if !strings.Contains(out, "▸") {
		t.Errorf("cursor glyph ▸ should appear in View output:\n%s", out)
	}
}

// TestStatusTxPoolScreen_FooterAppMode verifies the footer switches to
// the app-mode (back-navigation) copy when withAppMode is applied,
// and retains the standalone copy otherwise.
func TestStatusTxPoolScreen_FooterAppMode(t *testing.T) {
	s := newStatusTxPoolScreen(txpBackends(), nil, time.Second, 5*time.Second)
	standalone := stripANSI(s.View())
	if !strings.Contains(standalone, "q quits") {
		t.Errorf("standalone footer should contain 'q quits', got:\n%s", standalone)
	}

	app := stripANSI(s.withAppMode().View())
	if !strings.Contains(app, "q back") {
		t.Errorf("app-mode footer should contain 'q back', got:\n%s", app)
	}
	if !strings.Contains(app, "enter detail") {
		t.Errorf("app-mode footer should advertise 'enter detail', got:\n%s", app)
	}
}
