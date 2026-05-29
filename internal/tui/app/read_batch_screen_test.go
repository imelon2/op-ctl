package app

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/etherscan"
)

// drain runs Init + every cmd's tea.Msg through Update until cmd is
// nil or only emits non-actionable messages. Lets a tea.Model-style
// test wait for an async fetch to land without spinning a real
// tea.Program. Bounded so a buggy loop fails the test rather than
// hanging.
func drain(t *testing.T, m tea.Model, max int) tea.Model {
	t.Helper()
	cmd := m.Init()
	for i := 0; cmd != nil && i < max; i++ {
		msg := cmd()
		m, cmd = m.Update(msg)
	}
	return m
}

// TestReadBatchScreen_EmptyState pins AC-11: an operator opening
// `read batch` against a freshly-provisioned chain (cache 0 rows)
// must see the graceful "(no batches yet; cache empty)" line, NOT a
// blank box or an error.
func TestReadBatchScreen_EmptyState(t *testing.T) {
	dir := t.TempDir()
	store, err := batchcache.Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	screen := drain(t, newReadBatchScreen(store), 5)
	view := screen.View()
	if !strings.Contains(view, "(no batches yet; cache empty)") {
		t.Errorf("empty-state view missing expected line:\n%s", view)
	}
}

// TestReadBatchScreen_RendersNewestFirst pins AC-9: list is ordered
// DESC by block_number at the read layer regardless of insertion order.
// Insertion is ascending (the etherscan client paginates sort=asc).
func TestReadBatchScreen_RendersNewestFirst(t *testing.T) {
	dir := t.TempDir()
	store, err := batchcache.Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	// Insert ascending; the screen must render descending.
	rows := []etherscan.Tx{
		{BlockNumber: 100, TimeStamp: 1, Hash: "0x100", From: "0x0", To: "0x0", Value: "0", GasUsed: 1, MethodID: "0x6a", Input: "0x", Status: 1},
		{BlockNumber: 200, TimeStamp: 2, Hash: "0x200", From: "0x0", To: "0x0", Value: "0", GasUsed: 1, MethodID: "0x6a", Input: "0x", Status: 1},
		{BlockNumber: 300, TimeStamp: 3, Hash: "0x300", From: "0x0", To: "0x0", Value: "0", GasUsed: 1, MethodID: "0x6a", Input: "0x", Status: 1},
	}
	if err := store.UpsertPage(1, rows); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	screen := drain(t, newReadBatchScreen(store), 5)
	view := screen.View()
	// Probe by tx_hash so header metrics (e.g. "avg blocks/batch: 100.0")
	// don't collide with the block-number column.
	idx100 := strings.Index(view, "0x100")
	idx200 := strings.Index(view, "0x200")
	idx300 := strings.Index(view, "0x300")
	if idx300 < 0 || idx200 < 0 || idx100 < 0 {
		t.Fatalf("view missing one of the tx hashes:\n%s", view)
	}
	if !(idx300 < idx200 && idx200 < idx100) {
		t.Errorf("expected DESC order (0x300 before 0x200 before 0x100), got positions 300=%d 200=%d 100=%d\nview:\n%s",
			idx300, idx200, idx100, view)
	}
}

// TestReadBatchScreen_EnterEmitsSelected guards the keymap contract:
// pressing Enter on the cursor row produces a readBatchTxSelectedMsg
// carrying the tx hash app.Update needs to push the detail screen.
func TestReadBatchScreen_EnterEmitsSelected(t *testing.T) {
	dir := t.TempDir()
	store, err := batchcache.Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertPage(1, []etherscan.Tx{
		{BlockNumber: 42, TimeStamp: 1, Hash: "0xabc", From: "0x0", To: "0x0", Value: "0", GasUsed: 1, MethodID: "0x6a", Input: "0x", Status: 1},
	}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	m := drain(t, newReadBatchScreen(store), 5)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on a row should emit a cmd, got nil")
	}
	msg := cmd()
	sel, ok := msg.(readBatchTxSelectedMsg)
	if !ok {
		t.Fatalf("expected readBatchTxSelectedMsg, got %T", msg)
	}
	if sel.txHash != "0xabc" {
		t.Errorf("txHash: got %q, want %q", sel.txHash, "0xabc")
	}
}

// TestReadBatchDetailScreen_ShowsFields guards AC-10: the detail
// screen renders the 12 labeled fields the spec calls out. We probe
// for a handful of representative labels (tx_hash, block_number,
// timestamp, gas_used, method_id, status) rather than asserting
// pixel-perfect layout.
func TestReadBatchDetailScreen_ShowsFields(t *testing.T) {
	dir := t.TempDir()
	store, err := batchcache.Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertPage(1, []etherscan.Tx{
		{BlockNumber: 99, TimeStamp: 1_700_000_000, Hash: "0xfeedbeef", From: "0xaaa", To: "0xbbb", Value: "0", GasUsed: 12345, MethodID: "0x6a", Input: "0xdeadbeef", Status: 1},
	}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	m := drain(t, newReadBatchDetailScreen(store, "0xfeedbeef"), 5)
	view := m.View()
	// Spec calls for 12 labeled fields: tx_hash, block, timestamp,
	// from, to, value, gas, gasUsed, gasPrice, methodId, status,
	// input. The screen uses snake_case ("gas_used", "gas_price",
	// "method_id") — probe for both halves of any underscore name
	// so the test catches accidental label renames.
	for _, want := range []string{"tx_hash", "block", "timestamp", "from", "to", "value", "gas", "gas_used", "gas_price", "method_id", "status", "input"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail view missing field %q:\n%s", want, view)
		}
	}
	for _, want := range []string{"0xfeedbeef", "99", "0xaaa", "0xbbb", "12345", "0x6a", "success"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail view missing value %q:\n%s", want, view)
		}
	}
}

// TestRunReadBatch_NonNil sanity-checks the CLI entry-point exists
// and accepts the documented signature. Running tea.NewProgram would
// require a real TTY, so we only smoke-test the function is callable
// (compile-time guarantee).
func TestRunReadBatch_NonNil(t *testing.T) {
	_ = RunReadBatch
	_ = context.Background
}
