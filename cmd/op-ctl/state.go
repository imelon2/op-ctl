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
	"op-ctl/internal/opnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/app"
)

var (
	stateBlockBackend  string
	stateBlockInterval time.Duration
	stateBlockTimeout  time.Duration
	stateBlockPlain    bool
)

var stateCmd = &cobra.Command{
	Use:   "status",
	Short: "Chain-state inspection commands",
	Long: "Periodic, multi-backend queries against execution-layer state. " +
		"Subcommands fan out to every [backends.*] in config.toml and surface " +
		"the result either as an alt-screen TUI (default) or a piping-friendly " +
		"stream (--plain).",
}

var stateBlockCmd = &cobra.Command{
	Use:   "block",
	Short: "Live optimism_syncStatus across every backend with per-column lag",
	Long: "Periodically calls optimism_syncStatus on each backend's " +
		"consensus_rpc_url and displays two stacked sections — Layer 2 " +
		"(unsafe_l2, safe_l2, finalized_l2) and Layer 1 (current_l1, " +
		"current_l1_finalized, head_l1, safe_l1, finalized_l1). Each cell " +
		"renders as `number(lag)` where lag is the column-local head minus " +
		"this backend's number. The TUI updates in place; --plain streams " +
		"one labeled row per backend per tick for piping/grep.\n\n" +
		"Backend ssh_jump (if set) is honored automatically via the shared " +
		"sshtunnel.Resolver — no separate config required.",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Wire an explicit signal context so Ctrl+C/SIGTERM unblock the
		// plain mode's select loop AND the alt-screen tea.Program.
		// Cobra is not configured to install one in this repo, so the
		// wiring has to live here.
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
		if stateBlockBackend != "" {
			b, err := pickBackend(stateBlockBackend, bs)
			if err != nil {
				return err
			}
			bs = []config.Backend{b}
		}

		intervalEff := resolveDuration(stateBlockInterval, cfg.State.Block.Interval, 1*time.Second)
		if intervalEff <= 0 {
			return fmt.Errorf("--interval must be > 0, got %v", intervalEff)
		}
		timeoutEff := resolveDuration(stateBlockTimeout, cfg.State.Block.Timeout, 5*time.Second)
		if timeoutEff <= 0 {
			return fmt.Errorf("--timeout must be > 0, got %v", timeoutEff)
		}

		resolver := buildResolver(cfg)
		defer closeResolver(resolver)

		l2BlockTime := cfg.Global.L2BlockTime
		if l2BlockTime <= 0 {
			l2BlockTime = opnode.L2BlockSeconds()
		}

		if stateBlockPlain {
			return runStateBlockPlain(ctx, cmd.OutOrStdout(), resolver, bs, intervalEff, timeoutEff, l2BlockTime)
		}
		return app.RunStateBlock(ctx, cfg, resolver, intervalEff, timeoutEff)
	},
}

// resolveDuration returns the first non-zero value among flag, config,
// fallback. Mirrors the resolution order documented in the spec:
// CLI > config > hard default.
func resolveDuration(flagVal, cfgVal, fallback time.Duration) time.Duration {
	if flagVal > 0 {
		return flagVal
	}
	if cfgVal > 0 {
		return cfgVal
	}
	return fallback
}

// runStateBlockPlain runs the polling loop for `--plain` mode. It uses
// a select-based ticker (not a poll-then-sleep loop) so SIGINT through
// the signal-context unblocks mid-interval instead of waiting out a
// time.Sleep. The render-first-then-wait order means the operator sees
// an immediate first sample within ~one RPC roundtrip, not after the
// full interval has elapsed.
func runStateBlockPlain(ctx context.Context, w io.Writer, resolver *sshtunnel.Resolver, bs []config.Backend, interval, timeout, l2BlockTime time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	indicators := buildPlainIndicators(l2BlockTime)
	for {
		runStateBlockTick(ctx, w, resolver, bs, timeout, indicators)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// plainColumn is the plain-mode counterpart to the TUI's syncColumn —
// labels a column and extracts its uint64 from a SyncStatus. Kept in
// sync with state_block_screen.go:l2Columns / l1Columns so the plain
// and TUI paths show identical column sets in the same order.
type plainColumn struct {
	name string
	get  func(*opnode.SyncStatus) uint64
}

// plainIndicator is the plain-mode mirror of state_block_screen.go:
// indicator. Same semantics; duplicated here so the cmd package has
// no inbound dependency on the tui/app package.
type plainIndicator struct {
	left     plainColumn
	right    plainColumn
	perBlock time.Duration
}

// plainResult is one backend's per-tick optimism_syncStatus outcome.
// Hoisted to package scope so runStateBlockTick and writePlainSection
// can share the same nominal type (Go's type identity requires it).
type plainResult struct {
	idx     int
	status  *opnode.SyncStatus
	latency time.Duration
	err     error
}

var (
	plainL2Columns = []plainColumn{
		{"unsafe_l2", func(s *opnode.SyncStatus) uint64 { return s.UnsafeL2.Number }},
		{"safe_l2", func(s *opnode.SyncStatus) uint64 { return s.SafeL2.Number }},
		{"finalized_l2", func(s *opnode.SyncStatus) uint64 { return s.FinalizedL2.Number }},
	}
	plainL1Columns = []plainColumn{
		{"current_l1", func(s *opnode.SyncStatus) uint64 { return s.CurrentL1.Number }},
		{"current_l1_finalized", func(s *opnode.SyncStatus) uint64 { return s.CurrentL1Finalized.Number }},
		{"head_l1", func(s *opnode.SyncStatus) uint64 { return s.HeadL1.Number }},
		{"safe_l1", func(s *opnode.SyncStatus) uint64 { return s.SafeL1.Number }},
		{"finalized_l1", func(s *opnode.SyncStatus) uint64 { return s.FinalizedL1.Number }},
	}
)

// buildPlainIndicators returns the three pipeline-health gaps surfaced
// at the bottom of each tick. L2 indicators use the caller-supplied
// l2BlockTime so the displayed durations match the chain's actual
// block cadence; L1 stays on the EVM 12s constant. Mirrors the TUI's
// buildIndicators.
func buildPlainIndicators(l2BlockTime time.Duration) []plainIndicator {
	return []plainIndicator{
		{left: plainL2Columns[0], right: plainL2Columns[1], perBlock: l2BlockTime},
		{left: plainL2Columns[1], right: plainL2Columns[2], perBlock: l2BlockTime},
		{left: plainL1Columns[2], right: plainL1Columns[0], perBlock: opnode.L1BlockSeconds()},
	}
}

// runStateBlockTick fans out one optimism_syncStatus RPC per backend,
// collects results via WaitGroup, then renders two stacked sections
// (Layer 2 followed by Layer 1) with one row per backend. Each cell is
// `name=number(lag)` where lag = column_head - this_backend_number; the
// column head is the max across OK backends for that specific column.
//
// Keep in sync with state_block_screen.go:fetchSyncStatus — both paths
// must apply the empty-URL guard first, then resolver.HTTPClient, then
// opnode.Sync. Drift will surface as TUI/plain semantic divergence
// (e.g. one path hits the resolver with an empty URL).
func runStateBlockTick(ctx context.Context, w io.Writer, resolver *sshtunnel.Resolver, bs []config.Backend, timeout time.Duration, indicators []plainIndicator) {
	results := make([]plainResult, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].idx = i
			if strings.TrimSpace(b.ConsensusRPCURL) == "" {
				results[i].err = errors.New("missing consensus_rpc_url")
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			hc, err := resolver.HTTPClient(callCtx, b.SSHJump)
			if err != nil {
				results[i].err = err
				return
			}
			status, lat, err := opnode.Sync(callCtx, hc, b.ConsensusRPCURL)
			results[i].status = status
			results[i].latency = lat
			results[i].err = err
		}()
	}
	wg.Wait()

	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(w, "[%s] state.block\n", ts)
	writePlainSection(w, "Layer 2", plainL2Columns, bs, results)
	writePlainSection(w, "Layer 1", plainL1Columns, bs, results)
	writePlainIndicator(w, results, indicators)
	fmt.Fprintln(w)
}

// writePlainIndicator emits the Indicator section: a header line and
// one labeled line per indicator showing
// `<leftName>(<leftHead>) - <rightName>(<rightHead>) = <gap> ( <human> )`.
// Operand widths are computed across the surviving indicators so the
// `-` and `=` separators align across lines. Suppression matches the
// TUI — when every indicator is omitted, the whole section is skipped.
func writePlainIndicator(w io.Writer, results []plainResult, indicators []plainIndicator) {
	type computed struct {
		leftStr  string
		rightStr string
		gapStr   string
		timeStr  string
	}
	var rows []computed
	maxLeftW, maxRightW, maxGapW := 0, 0, 0
	for _, ind := range indicators {
		leftHead, haveLeft := plainHeadOf(results, ind.left.get)
		rightHead, haveRight := plainHeadOf(results, ind.right.get)
		if !haveLeft || !haveRight {
			continue
		}
		var gap uint64
		if leftHead >= rightHead {
			gap = leftHead - rightHead
		}
		r := computed{
			leftStr:  fmt.Sprintf("%s(%d)", ind.left.name, leftHead),
			rightStr: fmt.Sprintf("%s(%d)", ind.right.name, rightHead),
			gapStr:   fmt.Sprintf("%d", gap),
			timeStr:  opnode.HumanizeGap(gap, ind.perBlock),
		}
		if w := len(r.leftStr); w > maxLeftW {
			maxLeftW = w
		}
		if w := len(r.rightStr); w > maxRightW {
			maxRightW = w
		}
		if w := len(r.gapStr); w > maxGapW {
			maxGapW = w
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "  Indicator")
	for _, r := range rows {
		fmt.Fprintf(w, "    %-*s - %-*s = %*s ( %s )\n",
			maxLeftW, r.leftStr, maxRightW, r.rightStr, maxGapW, r.gapStr, r.timeStr)
	}
}

// plainHeadOf is the plain-mode mirror of state_block_screen.go:headOf.
// Returns the max value of get(status) across OK results and whether
// at least one OK result contributed.
func plainHeadOf(results []plainResult, get func(*opnode.SyncStatus) uint64) (uint64, bool) {
	var head uint64
	have := false
	for _, r := range results {
		if r.err != nil || r.status == nil {
			continue
		}
		n := get(r.status)
		if !have || n > head {
			head = n
			have = true
		}
	}
	return head, have
}

// writePlainSection renders one labeled section (Layer 2 or Layer 1).
// Per-column heads are computed from OK rows only. Failed rows print
// an `ERR <msg>` line that spans the section instead of per-column ERR
// cells; this matches the TUI's renderSection behavior.
func writePlainSection(w io.Writer, label string, cols []plainColumn, bs []config.Backend, results []plainResult) {
	heads := make([]uint64, len(cols))
	haveHead := make([]bool, len(cols))
	for _, r := range results {
		if r.err != nil || r.status == nil {
			continue
		}
		for ci, col := range cols {
			n := col.get(r.status)
			if !haveHead[ci] || n > heads[ci] {
				heads[ci] = n
				haveHead[ci] = true
			}
		}
	}

	fmt.Fprintf(w, "  %s\n", label)
	for i, r := range results {
		name := bs[i].Name
		if r.err != nil {
			fmt.Fprintf(w, "    %s\tlatency=%dms\tstatus=ERR %s\n",
				name, r.latency/time.Millisecond, sanitizeForPlain(r.err.Error()))
			continue
		}
		if r.status == nil {
			fmt.Fprintf(w, "    %s\tlatency=%dms\tstatus=ERR (nil status)\n",
				name, r.latency/time.Millisecond)
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "    %s", name)
		for ci, col := range cols {
			n := col.get(r.status)
			if haveHead[ci] {
				lag := heads[ci] - n
				fmt.Fprintf(&b, "\t%s=%d(%d)", col.name, n, lag)
			} else {
				fmt.Fprintf(&b, "\t%s=%d(?)", col.name, n)
			}
		}
		fmt.Fprintf(&b, "\tlatency=%dms\tstatus=OK", r.latency/time.Millisecond)
		fmt.Fprintln(w, b.String())
	}
}

// sanitizeForPlain replaces bytes that could hijack a terminal (ESC
// CSI, BEL, etc.) with '?', preserving printable ASCII and Unicode
// codepoints. Used only by plain-mode output where stdout may be a
// real terminal; the TUI path renders through lipgloss which already
// neutralizes escapes.
func sanitizeForPlain(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t', r == ' ':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func init() {
	stateBlockCmd.Flags().StringVar(
		&stateBlockBackend, "backend", "",
		"poll only this backend by name (default: all backends)",
	)
	stateBlockCmd.Flags().DurationVar(
		&stateBlockInterval, "interval", 0,
		"polling interval (default: 1s, or [state.block].interval from config)",
	)
	stateBlockCmd.Flags().DurationVar(
		&stateBlockTimeout, "timeout", 0,
		"per-RPC timeout (default: 5s, or [state.block].timeout from config)",
	)
	stateBlockCmd.Flags().BoolVar(
		&stateBlockPlain, "plain", false,
		"stream tab-separated rows to stdout instead of the alt-screen TUI (for piping/grep)",
	)
	stateCmd.AddCommand(stateBlockCmd)
}
