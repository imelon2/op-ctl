// Package batchprefetch is the single integration seam between
// `op-ctl read batch` (cmd/op-ctl/read.go) and the TUI menu dispatch
// (internal/tui/app/app.go). Both entry points need the same
// "validate config → check TTL → maybe page Etherscan → return a
// ready-to-render *batchcache.Store" choreography, and isolating it
// here keeps either caller from drifting.
//
// Package boundary note: this package lives under internal/ so the
// TUI can import it (Go forbids `internal/tui/...` from importing
// `cmd/op-ctl/`). The CLI imports it the same way.
package batchprefetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/config"
	"op-ctl/internal/contracts"
	"op-ctl/internal/etherscan"
)

// HTTPClient is the *http.Client used for both eth_chainId and the
// Etherscan V2 calls. Exposed as a package-level var so tests can
// inject an httptest-backed transport without changing the public
// Prepare signature (mirrors the config.WarningWriter and
// config.SSHConfigGet seams used elsewhere in the project).
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// Prepare validates the API key, opens the per-chain cache, and —
// only if last_synced_at is older than cfg.Batch.CacheTTL — pages
// Etherscan from max(StartBlock, MaxBlock+1). On post-Open() errors
// the store is returned NON-nil so the caller can render a stale
// read; the caller owns Close().
func Prepare(ctx context.Context, cfg *config.Config, progress io.Writer) (*batchcache.Store, error) {
	if cfg == nil {
		return nil, fmt.Errorf("batchprefetch: nil config")
	}
	if progress == nil {
		progress = io.Discard
	}
	if cfg.URLs.EtherscanAPIKey == "" {
		return nil, fmt.Errorf("batchprefetch: etherscan_api_key required for `read batch`; set [urls].etherscan_api_key in your config")
	}
	if cfg.Batch.BatchInboxToAddress == "" {
		return nil, fmt.Errorf("batchprefetch: batch_inbox_to_address is empty; set [batch].batch_inbox_to_address in your config")
	}
	l2ChainID, err := contracts.LoadL2ChainID(cfg.Contracts.StateRoot)
	if err != nil {
		return nil, fmt.Errorf("batchprefetch: resolve L2 chainid: %w", err)
	}
	baseDir := filepath.Dir(cfg.Path())
	store, err := batchcache.Open(baseDir, l2ChainID)
	if err != nil {
		return nil, fmt.Errorf("batchprefetch: open cache: %w", err)
	}
	last, err := store.LastSyncedAt()
	if err != nil {
		// A malformed meta row is recoverable — treat as expired.
		fmt.Fprintf(progress, "batchprefetch: read last_synced_at: %v (treating as expired)\n", err)
	} else if !last.IsZero() && time.Since(last) < cfg.Batch.CacheTTL {
		fmt.Fprintf(progress, "batchprefetch: cache fresh (last_sync %s ago, ttl %s) — skipping Etherscan\n",
			time.Since(last).Round(time.Second), cfg.Batch.CacheTTL)
		return store, nil
	}
	l1ChainID, err := etherscan.ResolveChainID(ctx, HTTPClient, cfg.URLs.L1RPCURL)
	if err != nil {
		return store, fmt.Errorf("batchprefetch: resolve L1 chainid: %w", err)
	}
	startFrom := cfg.Batch.StartBlock
	if mb, mberr := store.MaxBlockNumber(); mberr == nil && mb >= startFrom {
		startFrom = mb + 1
	}
	fmt.Fprintf(progress, "batchprefetch: paginating Etherscan V2 chainid=%d address=%s from_block=%d\n",
		l1ChainID, cfg.Batch.BatchInboxToAddress, startFrom)
	if err := etherscan.FetchTxList(
		ctx, HTTPClient, cfg.URLs.EtherscanAPIKey, l1ChainID,
		cfg.Batch.BatchInboxToAddress, startFrom,
		store.UpsertPage, progress,
	); err != nil {
		return store, fmt.Errorf("batchprefetch: fetch txlist: %w", err)
	}
	// Always bump last_synced_at on a successful end-of-sync, even
	// when Etherscan returned zero new rows. Without this, a cache
	// caught up to chain head would never refresh its TTL and every
	// invocation would re-page.
	if err := store.MarkSynced(ctx); err != nil {
		return store, fmt.Errorf("batchprefetch: mark synced: %w", err)
	}
	return store, nil
}
