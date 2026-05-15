package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/app"
)

var (
	statusTxPoolBackend  string
	statusTxPoolInterval time.Duration
	statusTxPoolTimeout  time.Duration
	statusTxPoolPlain    bool
)

var statusTxPoolCmd = &cobra.Command{
	Use:   "txpool",
	Short: "Live txpool_status across every backend",
	Long: "Periodically calls txpool_status on each backend's " +
		"execution_rpc_url and displays the pending / queued mempool " +
		"counts. Useful for spotting backlog buildup or gossip-propagation " +
		"divergence between sequencer and ingress nodes. The TUI updates " +
		"in place; --plain streams one row per backend per tick for " +
		"piping/grep.\n\n" +
		"Backend ssh_jump (if set) is honored automatically via the shared " +
		"sshtunnel.Resolver — no separate config required.",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Wire an explicit signal context so Ctrl+C/SIGTERM unblock the
		// plain mode's select loop AND the alt-screen tea.Program. Same
		// rationale as stateBlockCmd; the project doesn't install one
		// at the cobra layer.
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}
		bs := cfg.BackendList()
		if len(bs) == 0 {
			return errors.New("no backends configured")
		}
		if statusTxPoolBackend != "" {
			b, err := pickBackend(statusTxPoolBackend, bs)
			if err != nil {
				return err
			}
			bs = []config.Backend{b}
		}

		intervalEff := resolveDuration(statusTxPoolInterval, cfg.State.TxPool.Interval, 1*time.Second)
		if intervalEff <= 0 {
			return fmt.Errorf("--interval must be > 0, got %v", intervalEff)
		}
		timeoutEff := resolveDuration(statusTxPoolTimeout, cfg.State.TxPool.Timeout, 5*time.Second)
		if timeoutEff <= 0 {
			return fmt.Errorf("--timeout must be > 0, got %v", timeoutEff)
		}

		resolver := buildResolver(cfg)
		defer closeResolver(resolver)

		if statusTxPoolPlain {
			return runStatusTxPoolPlain(ctx, cmd.OutOrStdout(), resolver, bs, intervalEff, timeoutEff)
		}
		return app.RunStatusTxPool(ctx, cfg, resolver, intervalEff, timeoutEff)
	},
}

// txpoolPlainResult is one backend's per-tick txpool_status outcome.
// Hoisted to package scope so the worker fan-out goroutines and the
// renderer share the same nominal type.
type txpoolPlainResult struct {
	idx     int
	status  *elnode.TxPoolStatus
	latency time.Duration
	err     error
}

// runStatusTxPoolPlain runs the polling loop for `--plain` mode. Uses
// the same select-based ticker shape as runStateBlockPlain so SIGINT
// unblocks mid-interval and the first sample renders within ~one RPC
// roundtrip instead of waiting out the full interval.
func runStatusTxPoolPlain(ctx context.Context, w io.Writer, resolver *sshtunnel.Resolver, bs []config.Backend, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		runStatusTxPoolTick(ctx, w, resolver, bs, timeout)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runStatusTxPoolTick fans out one txpool_status RPC per backend and
// renders one cycle of plain-mode output. Empty-URL guard mirrors
// fetchTxPool in status_txpool_screen.go so the plain path produces
// the same ERR row text without ever invoking the resolver.
func runStatusTxPoolTick(ctx context.Context, w io.Writer, resolver *sshtunnel.Resolver, bs []config.Backend, timeout time.Duration) {
	results := make([]txpoolPlainResult, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].idx = i
			if strings.TrimSpace(b.ExecutionRPCURL) == "" {
				results[i].err = errors.New("missing execution_rpc_url")
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			hc, err := resolver.HTTPClient(callCtx, b.SSHJump)
			if err != nil {
				results[i].err = err
				return
			}
			status, lat, err := elnode.TxPool(callCtx, hc, b.ExecutionRPCURL)
			results[i].status = status
			results[i].latency = lat
			results[i].err = err
		}()
	}
	wg.Wait()

	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(w, "[%s] status.txpool\n", ts)
	for i, r := range results {
		name := bs[i].Name
		if r.err != nil {
			fmt.Fprintf(w, "  %s\tlatency=%dms\tstatus=ERR %s\n",
				name, r.latency/time.Millisecond, sanitizeForPlain(r.err.Error()))
			continue
		}
		if r.status == nil {
			fmt.Fprintf(w, "  %s\tlatency=%dms\tstatus=ERR (nil status)\n",
				name, r.latency/time.Millisecond)
			continue
		}
		fmt.Fprintf(w, "  %s\tpending=%d\tqueued=%d\ttotal=%d\tlatency=%dms\tstatus=OK\n",
			name,
			r.status.Pending,
			r.status.Queued,
			r.status.Pending+r.status.Queued,
			r.latency/time.Millisecond,
		)
	}
	fmt.Fprintln(w)
}

func init() {
	statusTxPoolCmd.Flags().StringVar(
		&statusTxPoolBackend, "backend", "",
		"poll only this backend by name (default: all backends)",
	)
	statusTxPoolCmd.Flags().DurationVar(
		&statusTxPoolInterval, "interval", 0,
		"polling interval (default: 1s, or [state.txpool].interval from config)",
	)
	statusTxPoolCmd.Flags().DurationVar(
		&statusTxPoolTimeout, "timeout", 0,
		"per-RPC timeout (default: 5s, or [state.txpool].timeout from config)",
	)
	statusTxPoolCmd.Flags().BoolVar(
		&statusTxPoolPlain, "plain", false,
		"stream tab-separated rows to stdout instead of the alt-screen TUI (for piping/grep)",
	)
	stateCmd.AddCommand(statusTxPoolCmd)
}
