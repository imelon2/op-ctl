package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/etherscan"
)

// TestRunReadBatchPlain_EmptyCacheGracefulOutput pins AC-11's plain
// branch: an empty cache must print the human-readable
// "(no batches yet; cache empty)" line and return nil (NOT an error).
// Scripted callers piping this into grep or jq should not have to
// distinguish between "fresh chain" and "failure".
func TestRunReadBatchPlain_EmptyCacheGracefulOutput(t *testing.T) {
	dir := t.TempDir()
	store, err := batchcache.Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	var buf bytes.Buffer
	if err := runReadBatchPlain(context.Background(), &buf, store); err != nil {
		t.Fatalf("runReadBatchPlain: %v", err)
	}
	if got, want := strings.TrimSpace(buf.String()), "(no batches yet; cache empty)"; got != want {
		t.Errorf("stdout: got %q, want %q", got, want)
	}
}

// TestRunReadBatchPlain_TabSeparatedRows pins AC-8: every cached row
// becomes one tab-separated line, ordered newest-first. The columns
// match the TUI list — block, timestamp, hash, methodId, input size,
// gas used — so an operator can pipe to `awk -F'\t'` without needing
// the TUI cheat sheet.
func TestRunReadBatchPlain_TabSeparatedRows(t *testing.T) {
	dir := t.TempDir()
	store, err := batchcache.Open(dir, "0xa5e8")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertPage(1, []etherscan.Tx{
		{BlockNumber: 100, TimeStamp: 1_700_000_000, Hash: "0x100", From: "0xa", To: "0xb",
			Value: "0", GasUsed: 21000, MethodID: "0x6a", Input: "0xdead", Status: 1},
		{BlockNumber: 200, TimeStamp: 1_700_000_100, Hash: "0x200", From: "0xa", To: "0xb",
			Value: "0", GasUsed: 31000, MethodID: "0x6a", Input: "0xbeefcafe", Status: 1},
	}); err != nil {
		t.Fatalf("UpsertPage: %v", err)
	}
	var buf bytes.Buffer
	if err := runReadBatchPlain(context.Background(), &buf, store); err != nil {
		t.Fatalf("runReadBatchPlain: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count: got %d, want 2 (rows=%v)", len(lines), lines)
	}
	// Newest first (block 200 before 100).
	if !strings.HasPrefix(lines[0], "200\t") {
		t.Errorf("first line should start with block 200: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "100\t") {
		t.Errorf("second line should start with block 100: %q", lines[1])
	}
	// Each line should have 6 tab-separated fields.
	for i, ln := range lines {
		parts := strings.Split(ln, "\t")
		if len(parts) != 6 {
			t.Errorf("line %d field count: got %d (%v), want 6", i, len(parts), parts)
		}
	}
}
