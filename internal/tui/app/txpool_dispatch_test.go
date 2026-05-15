package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// TestHandleTxpoolDetailMsg_PushesDetailScreen verifies the App
// routes a txpoolDetailMsg into the per-backend detail-list screen.
func TestHandleTxpoolDetailMsg_PushesDetailScreen(t *testing.T) {
	root := &cobra.Command{Use: "op-ctl"}
	cfg := stubConfig(t, "sequencer")
	a := New(root, cfg, nil, t.TempDir(), 5*time.Second)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	b := cfg.BackendList()[0]
	next, _ := a.Update(txpoolDetailMsg{backend: b})
	a = next.(App)

	if got := len(a.stack); got != 2 {
		t.Fatalf("stack depth: got %d, want 2", got)
	}
	if _, ok := a.stack[1].(statusTxPoolDetailScreen); !ok {
		t.Errorf("top of stack: got %T, want statusTxPoolDetailScreen", a.stack[1])
	}
}

// TestHandleTxDetailMsg_PushesTxDetailScreen verifies the App pushes
// the tx-detail screen directly (no loading screen, no async fetch)
// when the operator presses enter on a list row. Option A: the tx
// pointer comes from the cache, so no RPC is dispatched.
func TestHandleTxDetailMsg_PushesTxDetailScreen(t *testing.T) {
	root := &cobra.Command{Use: "op-ctl"}
	cfg := stubConfig(t, "sequencer")
	a := New(root, cfg, nil, t.TempDir(), 5*time.Second)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	b := cfg.BackendList()[0]
	tx := sampleTxPoolTx()
	next, cmd := a.Update(txDetailMsg{backend: b, tx: tx})
	a = next.(App)

	if got := len(a.stack); got != 2 {
		t.Fatalf("stack depth after txDetailMsg: got %d, want 2 (root + tx-detail)", got)
	}
	if _, ok := a.stack[1].(statusTxPoolTxDetailScreen); !ok {
		t.Errorf("top of stack: got %T, want statusTxPoolTxDetailScreen", a.stack[1])
	}
	if cmd != nil {
		t.Errorf("txDetailMsg should NOT emit a cmd (no async fetch), got %T", cmd())
	}
}

// TestHandleTxDetailMsg_TxNotInPool covers the race-recovery path:
// pushing a tx-detail screen with a nil tx pointer (operator clicked
// a row that was JUST mined). The screen still renders rather than
// crashing.
func TestHandleTxDetailMsg_TxNotInPool(t *testing.T) {
	root := &cobra.Command{Use: "op-ctl"}
	cfg := stubConfig(t, "sequencer")
	a := New(root, cfg, nil, t.TempDir(), 5*time.Second)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	b := cfg.BackendList()[0]
	next, _ := a.Update(txDetailMsg{backend: b, tx: nil})
	a = next.(App)

	scr, ok := a.stack[1].(statusTxPoolTxDetailScreen)
	if !ok {
		t.Fatalf("top of stack: got %T, want statusTxPoolTxDetailScreen", a.stack[1])
	}
	if scr.tx != nil {
		t.Errorf("screen tx should be nil for race scenario, got %+v", scr.tx)
	}
}
