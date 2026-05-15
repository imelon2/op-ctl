package app

import (
	"math/big"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
)

func txdBackend() config.Backend {
	return config.Backend{Name: "seq-1", ExecutionRPCURL: "http://127.0.0.1:8545"}
}

func feedTxD(s statusTxPoolDetailScreen, msgs ...tea.Msg) statusTxPoolDetailScreen {
	for _, m := range msgs {
		next, _ := s.Update(m)
		s = next.(statusTxPoolDetailScreen)
	}
	return s
}

// TestDetailScreen_View_InitialPolling exercises the first paint
// before any txpoolListFetchedMsg arrives. The screen must render the
// title, the URL subtitle, and a `polling…` placeholder.
func TestDetailScreen_View_InitialPolling(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	out := stripANSI(s.View())
	if !strings.Contains(out, "txpool detail · seq-1") {
		t.Errorf("title missing:\n%s", out)
	}
	if !strings.Contains(out, "polling…") {
		t.Errorf("pending state must show 'polling…':\n%s", out)
	}
	if !strings.Contains(out, "q back") {
		t.Errorf("footer should advertise 'q back':\n%s", out)
	}
}

// TestDetailScreen_View_FoldInOK feeds 3 cached txs into the screen
// and checks the rendered table contains each from/nonce/to. The
// cursor glyph must appear on the first row by default.
func TestDetailScreen_View_FoldInOK(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	s = feedTxD(s, txpoolListTickMsg{gen: 1})
	s = feedTxD(s, txpoolListFetchedMsg{
		gen: 1,
		txs: []elnode.TxPoolTx{
			{From: "0xaaaa", Nonce: 1, Pending: true, To: "0xrecvA", Value: big.NewInt(1), Gas: 21000},
			{From: "0xbbbb", Nonce: 7, Pending: true, To: "0xrecvB", Value: big.NewInt(250), Gas: 30000},
			{From: "0xaaaa", Nonce: 3, Pending: false, To: "0xrecvC", Value: big.NewInt(0), Gas: 25000},
		},
		observedAt: time.Now(),
	})
	out := stripANSI(s.View())
	for _, want := range []string{"0xaaaa", "0xbbbb", "0xrecvA", "0xrecvB", "0xrecvC", "21000", "30000"} {
		if !strings.Contains(out, want) {
			t.Errorf("View should contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "▸") {
		t.Errorf("cursor glyph should appear:\n%s", out)
	}
	if !strings.Contains(out, "3 txs") {
		t.Errorf("tx count should appear in status line:\n%s", out)
	}
}

// TestDetailScreen_EnterEmitsTxDetailMsg confirms enter on a populated
// row emits a txDetailMsg whose tx pointer points into the cached
// slice (Option A: zero per-click RPC).
func TestDetailScreen_EnterEmitsTxDetailMsg(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	s = feedTxD(s, txpoolListTickMsg{gen: 1})
	s = feedTxD(s, txpoolListFetchedMsg{
		gen: 1,
		txs: []elnode.TxPoolTx{
			{From: "0xaaaa", Nonce: 1, Pending: true, Value: big.NewInt(0)},
			{From: "0xbbbb", Nonce: 7, Pending: true, Value: big.NewInt(0)},
		},
		observedAt: time.Now(),
	})
	s = feedTxD(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a populated list should emit a cmd")
	}
	msg, ok := cmd().(txDetailMsg)
	if !ok {
		t.Fatalf("enter cmd: got %T, want txDetailMsg", cmd())
	}
	if msg.tx == nil {
		t.Fatal("emitted txDetailMsg should carry a non-nil *TxPoolTx pointer")
	}
	if msg.tx.From != "0xbbbb" || msg.tx.Nonce != 7 {
		t.Errorf("emitted tx: got from=%s nonce=%d, want 0xbbbb/7", msg.tx.From, msg.tx.Nonce)
	}
	// Pointer identity: msg.tx must alias the cache slot, not a copy.
	if msg.tx != &s.txs[1] {
		t.Errorf("emitted tx pointer should alias cache slot &s.txs[1]; got %p vs %p", msg.tx, &s.txs[1])
	}
}

// TestDetailScreen_EnterEmpty_NoOp confirms enter on an empty list is
// a no-op.
func TestDetailScreen_EnterEmpty_NoOp(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	s = feedTxD(s, txpoolListTickMsg{gen: 1})
	s = feedTxD(s, txpoolListFetchedMsg{gen: 1, txs: nil, observedAt: time.Now()})

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("enter on empty list should emit no cmd, got %T", cmd())
	}
}

// TestDetailScreen_StaleSnapshotDropped exercises the gen-counter
// guard: a fetched-msg tagged with an older generation must NOT
// mutate state and must reset inFlight so the loop recovers.
func TestDetailScreen_StaleSnapshotDropped(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	s = feedTxD(s, txpoolListTickMsg{gen: 1})
	s = feedTxD(s, txpoolListTickMsg{gen: 2}) // bumps gen to 2; tick=2 also sets inFlight=true
	if s.gen != 2 {
		t.Fatalf("setup: s.gen should be 2, got %d", s.gen)
	}
	stale := txpoolListFetchedMsg{
		gen: 1,
		txs: []elnode.TxPoolTx{
			{From: "0x9999", Nonce: 99, Pending: true, Value: big.NewInt(0)},
		},
		observedAt: time.Now(),
	}
	s = feedTxD(s, stale)
	if len(s.txs) != 0 {
		t.Errorf("stale snapshot should have been dropped; txs=%+v", s.txs)
	}
	if s.inFlight {
		t.Errorf("stale snapshot should have cleared inFlight to let the loop recover")
	}
}

// TestDetailScreen_RefreshBackpressure exercises the inFlight gate:
// a tick fired while a previous fetch hasn't returned must NOT issue
// a second fetch.
func TestDetailScreen_RefreshBackpressure(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Millisecond, 5*time.Second)
	next1, cmd1 := s.Update(txpoolListTickMsg{gen: 1})
	s = next1.(statusTxPoolDetailScreen)
	if !s.inFlight {
		t.Fatalf("inFlight should be true after first tick")
	}
	if cmd1 == nil {
		t.Fatal("first tick should emit a batch containing fetch + next-tick")
	}
	next2, cmd2 := s.Update(txpoolListTickMsg{gen: 2})
	s = next2.(statusTxPoolDetailScreen)
	if s.gen != 2 {
		t.Errorf("gen should be 2 after second tick, got %d", s.gen)
	}
	if cmd2 == nil {
		t.Fatalf("second tick should re-arm the cadence even when skipping the fetch")
	}
	msg := cmd2()
	if _, ok := msg.(txpoolListFetchedMsg); ok {
		t.Errorf("backpressured tick must NOT issue fetchTxPoolList; got txpoolListFetchedMsg")
	}
}

// TestDetailScreen_ManualRefresh_BumpsGenAndIssuesFetch confirms the
// `r` key pre-empts the in-flight fetch by bumping gen and issuing a
// fresh fetch; the stale result will be dropped by the gen guard.
func TestDetailScreen_ManualRefresh_BumpsGenAndIssuesFetch(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	s = feedTxD(s, txpoolListTickMsg{gen: 1})
	priorGen := s.gen
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r should emit a fetch cmd")
	}
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	s = next.(statusTxPoolDetailScreen)
	if s.gen <= priorGen {
		t.Errorf("manual r should bump gen; got %d, want > %d", s.gen, priorGen)
	}
	if !s.inFlight {
		t.Errorf("manual r should set inFlight=true")
	}
}

// TestDetailScreen_QEmitsPopMsg confirms q pops back to the summary
// screen rather than quitting the program.
func TestDetailScreen_QEmitsPopMsg(t *testing.T) {
	s := newStatusTxPoolDetailScreen(txdBackend(), nil, 10*time.Second, 5*time.Second)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should emit a tea.Cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("q cmd: got %T, want popMsg", cmd())
	}
}

// TestDetailScreen_EmptyURL_ImmediateERR confirms that the
// empty-URL guard short-circuits to an ERR snapshot without invoking
// the resolver.
func TestDetailScreen_EmptyURL_ImmediateERR(t *testing.T) {
	b := config.Backend{Name: "broken"} // ExecutionRPCURL empty
	cmd := fetchTxPoolList(1, nil, b, 5*time.Second)
	msg := cmd().(txpoolListFetchedMsg)
	if msg.err == nil {
		t.Fatal("empty-URL backend should produce an err")
	}
	if !strings.Contains(msg.err.Error(), "missing execution_rpc_url") {
		t.Errorf("err should mention 'missing execution_rpc_url': %v", msg.err)
	}
}
