package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/batchprefetch"
	"op-ctl/internal/tui/app"
)

var (
	readBatchTimeout time.Duration
	readBatchPlain   bool
)

// readBatchCmd is the third leaf of `op-ctl read`: pages Etherscan V2
// txlist for the batch inbox address configured under [batch], caches
// the results in a per-L2-chain SQLite store, and renders the
// accumulated batches either as a paginated TUI (default) or one tab-
// separated line per row (--plain, for piping/grep).
//
// Sync semantics are owned by internal/batchprefetch.Prepare:
//   - if the cache's last_synced_at is within [batch].cache_ttl, the
//     existing rows are served without any HTTP call.
//   - otherwise Etherscan is paged from max(cfg.Batch.StartBlock,
//     store.MaxBlockNumber()+1) with the store committing per page.
//
// The 200ms inter-request throttle inside the etherscan client keeps
// us under the public-tier 5 calls/sec quota; daily-quota rejection
// surfaces as a clear error from FetchTxList that the CLI prints +
// returns.
var readBatchCmd = &cobra.Command{
	Use:   "batch",
	Short: "List L1 batcher transactions cached from Etherscan V2",
	Long: "Pages Etherscan V2 txlist for [batch].batch_inbox_to_address on the " +
		"L1 chain identified by eth_chainId on [urls].l1_rpc_url. Results are " +
		"persisted to config/{l2-chainid}/batcher.db (pure-Go SQLite) and " +
		"reused for [batch].cache_ttl before the next fetch. Use --plain to " +
		"stream tab-separated rows to stdout for scripting.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}

		// Progress logs from the prefetcher (which page is being
		// fetched, last_synced gating decisions) go to stderr so they
		// don't pollute --plain stdout output.
		store, err := batchprefetch.Prepare(ctx, cfg, os.Stderr)
		if err != nil {
			// On prefetch failure we still close any partially-opened
			// store the prefetcher handed back (it may have committed
			// pages 1..N-1 before the failure).
			if store != nil {
				_ = store.Close()
			}
			return err
		}
		defer store.Close()

		timeoutEff := readBatchTimeout
		if timeoutEff <= 0 {
			timeoutEff = 60 * time.Second
		}

		if readBatchPlain {
			return runReadBatchPlain(ctx, cmd.OutOrStdout(), store)
		}
		return app.RunReadBatch(ctx, store, timeoutEff)
	},
}

// runReadBatchPlain dumps every cached transaction as tab-separated
// fields, ordered newest-first. The empty-cache branch prints a
// single human-readable line rather than erroring out — `read batch`
// against a fresh chain or an inbox that hasn't received batches yet
// must not look like a failure to scripted callers.
//
// Field order matches the TUI list:
//
//	block_number  timestamp(unix)  tx_hash  method_id  input_size_bytes  gas_used
//
// Choosing tabs (not commas) makes `awk -F'\t'` ergonomic and avoids
// CSV-style quoting around hex strings.
func runReadBatchPlain(ctx context.Context, out io.Writer, store *batchcache.Store) error {
	total, err := store.Count(ctx)
	if err != nil {
		return fmt.Errorf("read batch: count: %w", err)
	}
	if total == 0 {
		fmt.Fprintln(out, "(no batches yet; cache empty)")
		return nil
	}
	// Page through in 1000-row chunks rather than one giant SELECT so
	// a multi-million-row cache doesn't allocate the entire result
	// set in memory at once.
	const chunk = 1000
	for offset := 0; offset < total; offset += chunk {
		rows, err := store.List(ctx, chunk, offset)
		if err != nil {
			return fmt.Errorf("read batch: list offset=%d: %w", offset, err)
		}
		for _, r := range rows {
			fmt.Fprintf(out, "%d\t%d\t%s\t%s\t%d\t%d\n",
				r.BlockNumber, r.TimeStamp, r.Hash, r.MethodID, len(r.Input), r.GasUsed,
			)
		}
	}
	return nil
}

func init() {
	readBatchCmd.Flags().DurationVar(
		&readBatchTimeout, "timeout", 0,
		"per-screen TUI timeout (default: 60s; ignored with --plain)",
	)
	readBatchCmd.Flags().BoolVar(
		&readBatchPlain, "plain", false,
		"print tab-separated rows to stdout instead of the alt-screen TUI",
	)
	readCmd.AddCommand(readBatchCmd)
}
