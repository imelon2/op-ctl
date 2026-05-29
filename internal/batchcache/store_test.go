package batchcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"op-ctl/internal/etherscan"
)

// openTestStore builds a Store rooted under t.TempDir() with a fake
// L2 chain id; the temp dir is automatically cleaned up by go test.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mkTx is a small fixture helper — generates a deterministic Tx for
// a given block number so tests can wire scenarios without repeating
// the same struct literal everywhere.
func mkTx(block uint64) etherscan.Tx {
	return etherscan.Tx{
		BlockNumber: block,
		TimeStamp:   int64(1_700_000_000 + block),
		Hash:        fmt.Sprintf("0x%064x", block),
		From:        "0xdf05E8C9C0Ef7b85d2536182fa1E911622622542",
		To:          "0x00B607c67e6662aC51C747961b657659BB47FD95",
		Value:       "0",
		GasUsed:     188_432,
		MethodID:    "0x6a",
		Input:       "0xdead",
		Status:      1,
	}
}

// TestOpen_CreatesDir asserts the per-chain sub-directory + db file
// are both materialized lazily on Open(), so an operator with a
// freshly-cloned repo doesn't need to mkdir anything by hand.
func TestOpen_CreatesDir(t *testing.T) {
	base := t.TempDir()
	s, err := Open(base, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	wantPath := filepath.Join(base, "0xa5e8", "batcher.db")
	if s.Path() != wantPath {
		t.Errorf("Path: got %q, want %q", s.Path(), wantPath)
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat db file: %v", err)
	}
	if info.IsDir() {
		t.Errorf("db file should not be a directory")
	}
}

// TestUpsertPage_Idempotent guards the cache from double-counting on
// a re-fetched page (operator runs `read batch` twice within TTL but
// before the meta updates, or Etherscan returns overlapping rows on a
// page boundary). INSERT OR IGNORE on tx_hash must absorb duplicates
// silently.
func TestUpsertPage_Idempotent(t *testing.T) {
	s := openTestStore(t)
	page := []etherscan.Tx{mkTx(100), mkTx(200), mkTx(300)}
	if err := s.UpsertPage(1, page); err != nil {
		t.Fatalf("first UpsertPage: %v", err)
	}
	if err := s.UpsertPage(2, page); err != nil {
		t.Fatalf("second UpsertPage: %v", err)
	}
	n, err := s.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count after duplicate insert: got %d, want 3", n)
	}
}

// TestUpsertPage_LastSyncedAdvances pins the monotone ratchet: each
// successful page commit moves both meta.last_synced_at (clock) and
// meta.last_synced_block (page-max) forward.
func TestUpsertPage_LastSyncedAdvances(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPage(1, []etherscan.Tx{mkTx(100), mkTx(200), mkTx(300)}); err != nil {
		t.Fatalf("page 1: %v", err)
	}
	firstSync, err := s.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt after page 1: %v", err)
	}
	if firstSync.IsZero() {
		t.Fatal("LastSyncedAt should be non-zero after a commit")
	}
	maxAfter1, _ := s.MaxBlockNumber()
	if maxAfter1 != 300 {
		t.Errorf("MaxBlockNumber after page 1: got %d, want 300", maxAfter1)
	}
	// Sleep just long enough that the second RFC3339-second-resolution
	// timestamp is strictly greater than the first.
	time.Sleep(1100 * time.Millisecond)
	if err := s.UpsertPage(2, []etherscan.Tx{mkTx(400), mkTx(500)}); err != nil {
		t.Fatalf("page 2: %v", err)
	}
	secondSync, err := s.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt after page 2: %v", err)
	}
	if !secondSync.After(firstSync) {
		t.Errorf("LastSyncedAt should advance: first=%v second=%v", firstSync, secondSync)
	}
	maxAfter2, _ := s.MaxBlockNumber()
	if maxAfter2 != 500 {
		t.Errorf("MaxBlockNumber after page 2: got %d, want 500", maxAfter2)
	}
}

// TestUpsertPage_LastSyncedRatchetWithOverlap covers Critic finding
// #5 explicitly: when page 2 overlaps page 1 (re-fetch boundary,
// idempotent retry) the duplicate rows hit INSERT OR IGNORE and
// disappear from the row count delta — but meta.last_synced_block
// must still ratchet to the OVERLAPPING-PAGE'S MAX, not the row-count
// max, because the spec defines last_synced_block as "max(block in
// THIS page)" regardless of how many of those rows were duplicates.
func TestUpsertPage_LastSyncedRatchetWithOverlap(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPage(1, []etherscan.Tx{mkTx(100), mkTx(200), mkTx(300)}); err != nil {
		t.Fatalf("page 1: %v", err)
	}
	// Page 2 overlaps page 1 (blocks 200, 300) and adds 400 — common
	// when Etherscan's page boundary lands mid-batch and the cursor
	// is re-resolved to ensure no rows are missed.
	if err := s.UpsertPage(2, []etherscan.Tx{mkTx(200), mkTx(300), mkTx(400)}); err != nil {
		t.Fatalf("page 2: %v", err)
	}
	// Row count: 4 unique blocks (100, 200, 300, 400).
	cnt, _ := s.Count(context.Background())
	if cnt != 4 {
		t.Errorf("Count after overlap: got %d, want 4", cnt)
	}
	// MaxBlockNumber: 400 (cumulative).
	mb, _ := s.MaxBlockNumber()
	if mb != 400 {
		t.Errorf("MaxBlockNumber after overlap: got %d, want 400", mb)
	}
}

// TestList_NewestFirst pins the ORDER BY block_number DESC contract
// the TUI list depends on. Insertion was ascending; reads must come
// back descending.
func TestList_NewestFirst(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPage(1, []etherscan.Tx{mkTx(100), mkTx(200), mkTx(300), mkTx(400), mkTx(500)}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	rows, err := s.List(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("List len: got %d, want 5", len(rows))
	}
	for i, want := range []uint64{500, 400, 300, 200, 100} {
		if rows[i].BlockNumber != want {
			t.Errorf("rows[%d].BlockNumber: got %d, want %d", i, rows[i].BlockNumber, want)
		}
	}
}

// TestMaxBlockNumber_Resume mirrors the production resume flow:
// after a partial sync the prefetcher reads MaxBlockNumber+1 as its
// next Etherscan startBlock. An empty store returns 0 so the caller
// falls back to cfg.Batch.StartBlock for the very first sync.
func TestMaxBlockNumber_Resume(t *testing.T) {
	s := openTestStore(t)
	got, err := s.MaxBlockNumber()
	if err != nil {
		t.Fatalf("empty MaxBlockNumber: %v", err)
	}
	if got != 0 {
		t.Errorf("empty store MaxBlockNumber: got %d, want 0", got)
	}
	if err := s.UpsertPage(1, []etherscan.Tx{mkTx(10), mkTx(20), mkTx(30)}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	got, err = s.MaxBlockNumber()
	if err != nil {
		t.Fatalf("populated MaxBlockNumber: %v", err)
	}
	if got != 30 {
		t.Errorf("MaxBlockNumber: got %d, want 30", got)
	}
}

// TestGet_RoundTrip verifies the detail-screen lookup path: insert a
// row, then fetch by tx_hash. Tests that the BLOB input column
// round-trips through the scan helper.
func TestGet_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	row := mkTx(42)
	row.Input = "0x1234abcd"
	if err := s.UpsertPage(1, []etherscan.Tx{row}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	got, err := s.Get(context.Background(), row.Hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for existing tx")
	}
	if got.Input != row.Input {
		t.Errorf("Input round-trip: got %q, want %q", got.Input, row.Input)
	}
	if got.BlockNumber != row.BlockNumber {
		t.Errorf("BlockNumber: got %d, want %d", got.BlockNumber, row.BlockNumber)
	}
	// Missing tx returns (nil, nil).
	missing, err := s.Get(context.Background(), "0xdeadbeef")
	if err != nil {
		t.Fatalf("Get(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("missing tx should return nil, got %+v", missing)
	}
}
