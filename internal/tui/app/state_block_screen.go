package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/opnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/theme"
)

// stateBlockScreen drives `op-ctl state block`: every `interval` it
// fans out an optimism_syncStatus RPC to each backend's
// consensus_rpc_url, then re-renders TWO stacked tables — Layer 2 (unsafe_l2,
// safe_l2, finalized_l2) and Layer 1 (current_l1, current_l1_finalized,
// head_l1, safe_l1, finalized_l1). Every cell shows `number(lag)` where
// lag is the column-local head minus this backend's number. The head row
// for a column renders `number(0)`.
//
// Concurrency contract: ticks come tagged with a monotonic generation
// counter; snapshot messages from older generations are dropped at
// Update time so a slow RPC from tick N cannot pollute tick N+1's
// display. See the Init() invariant comment block below for the exact
// rule.
type stateBlockScreen struct {
	backends    []config.Backend
	resolver    *sshtunnel.Resolver
	interval    time.Duration
	timeout     time.Duration
	l2BlockTime time.Duration        // L2 block time used by the Indicator section.
	snapshots   []stateBlockSnapshot // len == len(backends); zero-value pending=true
	gen         uint64               // last accepted tick generation
	width       int
	height      int
}

// stateBlockSnapshot is one backend's most-recent optimism_syncStatus
// poll result. `pending` is true until the first result arrives so
// View() can render "polling…" instead of nil-deref'ing on status.
type stateBlockSnapshot struct {
	pending    bool
	status     *opnode.SyncStatus
	latency    time.Duration
	err        error
	observedAt time.Time
}

type stateBlockTickMsg struct{ gen uint64 }

type stateBlockSnapshotMsg struct {
	gen        uint64
	backendIdx int
	status     *opnode.SyncStatus
	latency    time.Duration
	err        error
	observedAt time.Time
}

func newStateBlockScreen(backends []config.Backend, resolver *sshtunnel.Resolver, interval, timeout, l2BlockTime time.Duration) stateBlockScreen {
	if l2BlockTime <= 0 {
		l2BlockTime = opnode.L2BlockSeconds()
	}
	snaps := make([]stateBlockSnapshot, len(backends))
	for i := range snaps {
		snaps[i].pending = true
	}
	return stateBlockScreen{
		backends:    backends,
		resolver:    resolver,
		interval:    interval,
		timeout:     timeout,
		l2BlockTime: l2BlockTime,
		snapshots:   snaps,
	}
}

// stateBlockScreen invariant: s.gen starts at 0; the first tickMsg emitted by Init()
// must carry gen=1. Every tickMsg is accepted only if msg.gen == s.gen+1, after
// which s.gen is bumped. Snapshot messages from older generations are dropped at
// Update time so a slow RPC from tick N cannot pollute tick N+1's display.
func (s stateBlockScreen) Init() tea.Cmd {
	// Emit the gen=1 tick immediately so the first RPC fan-out fires
	// without waiting one `interval` for the first ticker.C signal.
	return func() tea.Msg { return stateBlockTickMsg{gen: 1} }
}

func (s stateBlockScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
		return s, nil

	case stateBlockTickMsg:
		if m.gen != s.gen+1 {
			return s, nil
		}
		s.gen = m.gen
		cmds := make([]tea.Cmd, 0, len(s.backends)+1)
		for i, b := range s.backends {
			cmds = append(cmds, fetchSyncStatus(s.gen, i, s.resolver, b, s.timeout))
		}
		cmds = append(cmds, stateBlockTick(s.interval, s.gen+1))
		return s, tea.Batch(cmds...)

	case stateBlockSnapshotMsg:
		if m.gen != s.gen {
			// Stragglers from a prior tick — drop so the display
			// reflects only the latest generation.
			return s, nil
		}
		if m.backendIdx < 0 || m.backendIdx >= len(s.snapshots) {
			return s, nil
		}
		s.snapshots[m.backendIdx] = stateBlockSnapshot{
			pending:    false,
			status:     m.status,
			latency:    m.latency,
			err:        m.err,
			observedAt: m.observedAt,
		}
		return s, nil

	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		}
	}
	return s, nil
}

// fetchSyncStatus returns a tea.Cmd that performs a single
// optimism_syncStatus RPC for backend `b` under generation `gen`.
// An empty ConsensusRPCURL short-circuits to an ERR snapshot before
// touching the resolver — keeps the row informative without raising
// a confusing transport-level "unsupported protocol scheme" message.
//
// Keep in sync with cmd/op-ctl/state.go:runStateBlockTick — both
// paths must apply the empty-URL guard first, then resolver.HTTPClient,
// then opnode.Sync. The TUI shape (tea.Cmd → tea.Msg) and the plain
// shape (sync WaitGroup) differ structurally but the per-backend RPC
// sequence is identical.
func fetchSyncStatus(gen uint64, idx int, resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(b.ConsensusRPCURL) == "" {
			return stateBlockSnapshotMsg{
				gen:        gen,
				backendIdx: idx,
				err:        errors.New("missing consensus_rpc_url"),
				observedAt: time.Now(),
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hc, err := resolver.HTTPClient(ctx, b.SSHJump)
		if err != nil {
			return stateBlockSnapshotMsg{gen: gen, backendIdx: idx, err: err, observedAt: time.Now()}
		}
		status, lat, err := opnode.Sync(ctx, hc, b.ConsensusRPCURL)
		return stateBlockSnapshotMsg{
			gen:        gen,
			backendIdx: idx,
			status:     status,
			latency:    lat,
			err:        err,
			observedAt: time.Now(),
		}
	}
}

// stateBlockTick schedules the next stateBlockTickMsg carrying
// `nextGen`. The Update branch enforces msg.gen == s.gen+1 before
// accepting and bumping s.gen.
func stateBlockTick(interval time.Duration, nextGen uint64) tea.Cmd {
	return tea.Tick(interval, func(time.Time) tea.Msg { return stateBlockTickMsg{gen: nextGen} })
}

// ---------- columns ----------

// syncColumn is one cell-extractor: how to label the column and how to
// pull the uint64 for a given SyncStatus. The L2 and L1 sections each
// have their own ordered slice of these so the view code is a single
// loop instead of duplicated per-field logic.
type syncColumn struct {
	name string
	get  func(*opnode.SyncStatus) uint64
}

// l2Columns are the Layer-2 columns in display order. The other L2
// fields op-node returns (pending_safe_l2, cross_unsafe_l2,
// local_safe_l2) are intentionally NOT displayed.
var l2Columns = []syncColumn{
	{"unsafe_l2", func(s *opnode.SyncStatus) uint64 { return s.UnsafeL2.Number }},
	{"safe_l2", func(s *opnode.SyncStatus) uint64 { return s.SafeL2.Number }},
	{"finalized_l2", func(s *opnode.SyncStatus) uint64 { return s.FinalizedL2.Number }},
}

// l1Columns are the Layer-1 columns in display order.
var l1Columns = []syncColumn{
	{"current_l1", func(s *opnode.SyncStatus) uint64 { return s.CurrentL1.Number }},
	{"current_l1_finalized", func(s *opnode.SyncStatus) uint64 { return s.CurrentL1Finalized.Number }},
	{"head_l1", func(s *opnode.SyncStatus) uint64 { return s.HeadL1.Number }},
	{"safe_l1", func(s *opnode.SyncStatus) uint64 { return s.SafeL1.Number }},
	{"finalized_l1", func(s *opnode.SyncStatus) uint64 { return s.FinalizedL1.Number }},
}

// indicator is one line in the Indicator section: a labeled
// subtraction between two columns translated into a human-readable
// time. Each side references a full syncColumn so the column name and
// extractor stay coupled and the display can show `name(head)` for
// each operand.
type indicator struct {
	left     syncColumn
	right    syncColumn
	perBlock time.Duration
}

// buildIndicators returns the three pipeline-health gaps surfaced
// below the per-layer sections, in display order: L2 publish lag,
// L2 finality buffer, L1 catch-up. L2 indicators use the caller-
// supplied l2BlockTime so the displayed durations match the chain's
// actual block cadence; L1 stays on the EVM 12s constant.
func buildIndicators(l2BlockTime time.Duration) []indicator {
	return []indicator{
		{left: l2Columns[0], right: l2Columns[1], perBlock: l2BlockTime},
		{left: l2Columns[1], right: l2Columns[2], perBlock: l2BlockTime},
		{left: l1Columns[2], right: l1Columns[0], perBlock: opnode.L1BlockSeconds()},
	}
}

const sbNameColW = 18

// View renders the two stacked sections. Each section computes its
// per-column heads from the current snapshots and prints
// `number(lag)` per cell. Rows that failed RPC or are still pending
// span the whole section row with a single ERR / polling message
// rather than 3-or-5 individual ERR cells.
func (s stateBlockScreen) View() string {
	var b strings.Builder

	b.WriteString(theme.Title.Render("state · block · sync status") + "  ")
	b.WriteString(theme.Subtitle.Render(fmt.Sprintf(
		"interval=%s  timeout=%s  backends=%d",
		s.interval, s.timeout, len(s.backends),
	)))
	b.WriteString("\n\n")

	b.WriteString(theme.Section.Render("Layer 2") + "\n")
	s.renderSection(&b, l2Columns)
	b.WriteString("\n")
	b.WriteString(theme.Section.Render("Layer 1") + "\n")
	s.renderSection(&b, l1Columns)

	if lines := s.renderIndicators(); lines != "" {
		b.WriteString("\n")
		b.WriteString(theme.Section.Render("Indicator") + "\n")
		b.WriteString(lines)
	}

	b.WriteString("\n")
	b.WriteString(theme.Help.Render("q quits · live updates every " + s.interval.String()))
	return b.String()
}

// renderIndicators returns the body lines of the Indicator section,
// or "" when no indicator can be computed (all backends errored or
// every indicator's operand columns are missing heads). The caller
// is responsible for emitting the section title only when this
// returns a non-empty string.
//
// Each rendered line is `<leftName>(<leftHead>) - <rightName>(<rightHead>) = <gap> ( <human> )`.
// Operand widths are computed across the surviving indicators so the
// `-` and `=` separators align across lines.
func (s stateBlockScreen) renderIndicators() string {
	type computed struct {
		leftStr  string
		rightStr string
		gapStr   string
		timeStr  string
	}
	var rows []computed
	maxLeftW, maxRightW, maxGapW := 0, 0, 0
	for _, ind := range buildIndicators(s.l2BlockTime) {
		leftHead, haveLeft := headOf(s.snapshots, ind.left.get)
		rightHead, haveRight := headOf(s.snapshots, ind.right.get)
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
		return ""
	}
	var b strings.Builder
	for _, r := range rows {
		// Left-pad the operand columns so " - " and " = " sit at the
		// same byte (and visible) offset on every row.
		labelText := fmt.Sprintf("%-*s - %-*s = ", maxLeftW, r.leftStr, maxRightW, r.rightStr)
		// Right-align the numeric gap so digits line up under each
		// other regardless of magnitude (e.g., 4 vs 1055).
		valueText := fmt.Sprintf("%*s ( %s )", maxGapW, r.gapStr, r.timeStr)
		b.WriteString("  ")
		b.WriteString(theme.Label.Render(labelText))
		b.WriteString(theme.Value.Render(valueText))
		b.WriteString("\n")
	}
	return b.String()
}

// headOf returns the max value of get(status) across OK snapshots and
// whether at least one OK snapshot contributed.
func headOf(snapshots []stateBlockSnapshot, get func(*opnode.SyncStatus) uint64) (uint64, bool) {
	var head uint64
	have := false
	for _, snap := range snapshots {
		if snap.pending || snap.err != nil || snap.status == nil {
			continue
		}
		n := get(snap.status)
		if !have || n > head {
			head = n
			have = true
		}
	}
	return head, have
}

// renderSection writes one stacked table (header row + per-backend
// rows) for the given column set. Column widths are computed from the
// max of header-width and any rendered cell-width so the L1 section's
// long "current_l1_finalized" header aligns cleanly with its narrow
// numbers, and equally so under shorter L2 headers.
func (s stateBlockScreen) renderSection(b *strings.Builder, cols []syncColumn) {
	// Compute column heads (max over OK rows per column).
	heads := make([]uint64, len(cols))
	haveHead := make([]bool, len(cols))
	for _, snap := range s.snapshots {
		if snap.pending || snap.err != nil || snap.status == nil {
			continue
		}
		for ci, col := range cols {
			n := col.get(snap.status)
			if !haveHead[ci] || n > heads[ci] {
				heads[ci] = n
				haveHead[ci] = true
			}
		}
	}

	// Pre-format every cell to know the eventual column widths.
	type cell struct {
		text   string
		styled string
	}
	rows := make([][]cell, len(s.snapshots))
	colWidths := make([]int, len(cols))
	for ci, col := range cols {
		colWidths[ci] = lipgloss.Width(col.name)
	}
	for ri, snap := range s.snapshots {
		row := make([]cell, len(cols))
		for ci, col := range cols {
			var text, styled string
			switch {
			case snap.pending, snap.err != nil, snap.status == nil:
				// pending/err handled per-row below (full-width
				// message), but we still need placeholder strings
				// so the colWidths calc below stays consistent.
				text = ""
				styled = ""
			default:
				n := col.get(snap.status)
				if haveHead[ci] {
					lag := heads[ci] - n
					text = fmt.Sprintf("%d(%d)", n, lag)
					if lag == 0 {
						styled = theme.Header.Render(text)
					} else {
						styled = theme.Value.Render(text)
					}
				} else {
					text = fmt.Sprintf("%d(?)", n)
					styled = theme.Value.Render(text)
				}
			}
			row[ci] = cell{text: text, styled: styled}
			if w := lipgloss.Width(text); w > colWidths[ci] {
				colWidths[ci] = w
			}
		}
		rows[ri] = row
	}

	// Header row.
	b.WriteString("  ")
	b.WriteString(padTrunc(theme.Label.Render("backend"), sbNameColW))
	for ci, col := range cols {
		b.WriteString("  ")
		b.WriteString(padTrunc(theme.Label.Render(col.name), colWidths[ci]))
	}
	b.WriteString("\n")

	// Per-backend rows.
	for ri, snap := range s.snapshots {
		name := theme.Name.Render(s.backends[ri].Name)
		b.WriteString("  ")
		b.WriteString(padTrunc(name, sbNameColW))
		switch {
		case snap.pending:
			b.WriteString("  ")
			b.WriteString(theme.Pending.Render("polling…"))
		case snap.err != nil:
			b.WriteString("  ")
			b.WriteString(theme.ErrText.Render("ERR " + truncate(snap.err.Error(), 80)))
		case snap.status == nil:
			// Defensive: shouldn't happen given the snapshot
			// invariant (status set iff err == nil), but renders a
			// recognizable bug-report-friendly marker if it ever does.
			b.WriteString("  ")
			b.WriteString(theme.ErrText.Render("ERR (nil status)"))
		default:
			for ci := range cols {
				b.WriteString("  ")
				b.WriteString(padTrunc(rows[ri][ci].styled, colWidths[ci]))
			}
			b.WriteString("  ")
			b.WriteString(theme.Label.Render(fmt.Sprintf("%dms", snap.latency/time.Millisecond)))
			b.WriteString("  ")
			b.WriteString(theme.OKText.Render("✓"))
		}
		b.WriteString("\n")
	}
}

// truncate shortens s to width visible columns, ending with "…" when
// it must clip. The companion padTrunc (namespace_screen.go) handles
// the fixed-width padding case.
func truncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	// Simple rune-based truncate; good enough for monospace cells
	// where most characters are width-1.
	runes := []rune(s)
	for i := len(runes); i > 0; i-- {
		candidate := string(runes[:i]) + "…"
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	return "…"
}
