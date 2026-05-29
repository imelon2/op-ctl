package app

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/batchprefetch"
	"op-ctl/internal/config"
)

// batchPrefetchedMsg is the result of running batchprefetch.Prepare
// off the bubbletea event loop. The store handle is carried even on
// err (it may still be non-nil — etherscan.FetchTxList can fail
// mid-stream while page 1..N-1 are already committed) so the loading
// screen handler can decide whether to render a stale-read screen or
// surface the error.
type batchPrefetchedMsg struct {
	store *batchcache.Store
	err   error
}

// runBatchPrefetchCmd kicks off the prefetch in a goroutine and
// resolves with a typed batchPrefetchedMsg. timeout (carried on App)
// bounds the overall prefetch — Etherscan pagination + 200ms throttles
// can take a while on the initial sync, so the timeout should be
// generous (60s+).
func runBatchPrefetchCmd(cfg *config.Config, timeout time.Duration) tea.Cmd {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		// Prefetch progress logs are dropped from the TUI path — the
		// alt-screen would clobber them anyway. The CLI --plain path
		// is where stderr progress goes.
		store, err := batchprefetch.Prepare(ctx, cfg, nil)
		return batchPrefetchedMsg{store: store, err: err}
	}
}

// RunReadBatch opens the alt-screen list backed by the pre-opened
// store. The caller owns Close(). timeout is reserved for a future
// in-screen refresh path.
func RunReadBatch(ctx context.Context, store *batchcache.Store, timeout time.Duration) error {
	_ = timeout
	screen := newReadBatchScreen(store)
	_, err := tea.NewProgram(screen, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}
