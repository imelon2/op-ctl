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
	"op-ctl/internal/elnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/theme"
)

// statusTxPoolScreen drives `op-ctl status txpool`: every `interval`
// it fans out a `txpool_status` RPC to each backend's
// execution_rpc_url and re-renders one section per backend showing
// pending / queued / total counts. Mirrors stateBlockScreen's
// concurrency contract (monotonic generation counter; stragglers from
// older ticks are dropped at Update time).
//
// When mounted inside the unified App, `enter` on a backend row emits
// a txpoolDetailMsg so the App can push a drill-down screen. When the
// screen is run standalone via RunStatusTxPool the enter handler is
// inert (no listener consumes the msg) and the footer text reflects
// the simpler exit semantics.
type statusTxPoolScreen struct {
	backends  []config.Backend
	resolver  *sshtunnel.Resolver
	interval  time.Duration
	timeout   time.Duration
	snapshots []txpoolSnapshot // len == len(backends); zero-value pending=true
	gen       uint64
	cursor    int
	width     int
	height    int
	inApp     bool // App sets this via withAppMode; controls footer help text
}

// txpoolSnapshot is one backend's most-recent txpool_status poll
// result. `pending` here is the screen-state flag (no result yet),
// distinct from the wire-side TxPoolStatus.Pending field carried on
// `status`.
type txpoolSnapshot struct {
	pending    bool
	status     *elnode.TxPoolStatus
	latency    time.Duration
	err        error
	observedAt time.Time
}

type txpoolTickMsg struct{ gen uint64 }

type txpoolSnapshotMsg struct {
	gen        uint64
	backendIdx int
	status     *elnode.TxPoolStatus
	latency    time.Duration
	err        error
	observedAt time.Time
}

func newStatusTxPoolScreen(backends []config.Backend, resolver *sshtunnel.Resolver, interval, timeout time.Duration) statusTxPoolScreen {
	snaps := make([]txpoolSnapshot, len(backends))
	for i := range snaps {
		snaps[i].pending = true
	}
	return statusTxPoolScreen{
		backends:  backends,
		resolver:  resolver,
		interval:  interval,
		timeout:   timeout,
		snapshots: snaps,
	}
}

// withAppMode marks the screen as mounted inside the unified App so
// the footer advertises the back-navigation semantics. Without this,
// the screen runs in the standalone-program mode (`RunStatusTxPool`)
// and the footer keeps the simpler `q quits` copy.
func (s statusTxPoolScreen) withAppMode() statusTxPoolScreen {
	s.inApp = true
	return s
}

// statusTxPoolScreen invariant: s.gen starts at 0; the first tickMsg from
// Init() must carry gen=1. Subsequent ticks are accepted only if msg.gen ==
// s.gen+1. Snapshot messages from older generations are dropped at Update
// time so a slow RPC from tick N cannot pollute tick N+1's display.
func (s statusTxPoolScreen) Init() tea.Cmd {
	return func() tea.Msg { return txpoolTickMsg{gen: 1} }
}

func (s statusTxPoolScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
		return s, nil

	case txpoolTickMsg:
		if m.gen != s.gen+1 {
			return s, nil
		}
		s.gen = m.gen
		cmds := make([]tea.Cmd, 0, len(s.backends)+1)
		for i, b := range s.backends {
			cmds = append(cmds, fetchTxPool(s.gen, i, s.resolver, b, s.timeout))
		}
		cmds = append(cmds, txpoolTick(s.interval, s.gen+1))
		return s, tea.Batch(cmds...)

	case txpoolSnapshotMsg:
		if m.gen != s.gen {
			return s, nil
		}
		if m.backendIdx < 0 || m.backendIdx >= len(s.snapshots) {
			return s, nil
		}
		s.snapshots[m.backendIdx] = txpoolSnapshot{
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
		case "enter":
			if len(s.backends) == 0 {
				return s, nil
			}
			b := s.backends[s.cursor]
			return s, func() tea.Msg { return txpoolDetailMsg{backend: b} }
		case "down", "j":
			if len(s.backends) > 0 {
				s.cursor++
				if s.cursor >= len(s.backends) {
					s.cursor = len(s.backends) - 1
				}
			}
		case "up", "k":
			s.cursor--
			if s.cursor < 0 {
				s.cursor = 0
			}
		case "home", "g":
			s.cursor = 0
		case "end", "G":
			if len(s.backends) > 0 {
				s.cursor = len(s.backends) - 1
			}
		case "pgdown", "ctrl+d", " ":
			if len(s.backends) > 0 {
				s.cursor += halfPage(s.height)
				if s.cursor >= len(s.backends) {
					s.cursor = len(s.backends) - 1
				}
			}
		case "pgup", "ctrl+u", "b":
			s.cursor -= halfPage(s.height)
			if s.cursor < 0 {
				s.cursor = 0
			}
		}
	}
	return s, nil
}

// fetchTxPool returns a tea.Cmd that performs a single txpool_status
// RPC for backend `b` under generation `gen`. An empty ExecutionRPCURL
// short-circuits to an ERR snapshot before touching the resolver.
//
// Keep in sync with cmd/op-ctl/txpool.go:runStatusTxPoolTick — both
// paths must apply the empty-URL guard first, then resolver.HTTPClient,
// then elnode.TxPool.
func fetchTxPool(gen uint64, idx int, resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(b.ExecutionRPCURL) == "" {
			return txpoolSnapshotMsg{
				gen:        gen,
				backendIdx: idx,
				err:        errors.New("missing execution_rpc_url"),
				observedAt: time.Now(),
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hc, err := resolver.HTTPClient(ctx, b.SSHJump)
		if err != nil {
			return txpoolSnapshotMsg{gen: gen, backendIdx: idx, err: err, observedAt: time.Now()}
		}
		status, lat, err := elnode.TxPool(ctx, hc, b.ExecutionRPCURL)
		return txpoolSnapshotMsg{
			gen:        gen,
			backendIdx: idx,
			status:     status,
			latency:    lat,
			err:        err,
			observedAt: time.Now(),
		}
	}
}

func txpoolTick(interval time.Duration, nextGen uint64) tea.Cmd {
	return tea.Tick(interval, func(time.Time) tea.Msg { return txpoolTickMsg{gen: nextGen} })
}

const txpNameColW = 18

// View renders one table: header row plus one row per backend showing
// pending / queued / total counts and the per-RPC latency. Failed
// rows print a single full-width ERR message instead of per-column ERR
// cells; pending rows show "polling…".
func (s statusTxPoolScreen) View() string {
	var b strings.Builder

	b.WriteString(theme.Title.Render("status · txpool") + "  ")
	b.WriteString(theme.Subtitle.Render(fmt.Sprintf(
		"interval=%s  timeout=%s  backends=%d",
		s.interval, s.timeout, len(s.backends),
	)))
	b.WriteString("\n\n")

	// Pre-format values so column widths absorb the max number width
	// in this tick.
	type cell struct {
		pending string
		queued  string
		total   string
	}
	cells := make([]cell, len(s.snapshots))
	pendingW, queuedW, totalW := lipgloss.Width("pending"), lipgloss.Width("queued"), lipgloss.Width("total")
	for i, snap := range s.snapshots {
		if snap.pending || snap.err != nil || snap.status == nil {
			continue
		}
		c := cell{
			pending: fmt.Sprintf("%d", snap.status.Pending),
			queued:  fmt.Sprintf("%d", snap.status.Queued),
			total:   fmt.Sprintf("%d", snap.status.Pending+snap.status.Queued),
		}
		if w := lipgloss.Width(c.pending); w > pendingW {
			pendingW = w
		}
		if w := lipgloss.Width(c.queued); w > queuedW {
			queuedW = w
		}
		if w := lipgloss.Width(c.total); w > totalW {
			totalW = w
		}
		cells[i] = c
	}

	// Header. Two extra leading spaces account for the per-row cursor
	// gutter ("▸ " on the active row, "  " on inactive rows).
	b.WriteString("    ")
	b.WriteString(padTrunc(theme.Label.Render("backend"), txpNameColW))
	b.WriteString("  ")
	b.WriteString(padTrunc(theme.Label.Render("pending"), pendingW))
	b.WriteString("  ")
	b.WriteString(padTrunc(theme.Label.Render("queued"), queuedW))
	b.WriteString("  ")
	b.WriteString(padTrunc(theme.Label.Render("total"), totalW))
	b.WriteString("\n")

	// Rows. The cursor gutter is rendered before the row content; the
	// active row also gets a background tint so it's clear at a glance
	// which backend `enter` will drill into.
	for i, snap := range s.snapshots {
		var row strings.Builder
		name := theme.Name.Render(s.backends[i].Name)
		row.WriteString(padTrunc(name, txpNameColW))
		switch {
		case snap.pending:
			row.WriteString("  ")
			row.WriteString(theme.Pending.Render("polling…"))
		case snap.err != nil:
			row.WriteString("  ")
			row.WriteString(theme.ErrText.Render("ERR " + truncate(snap.err.Error(), 80)))
		case snap.status == nil:
			row.WriteString("  ")
			row.WriteString(theme.ErrText.Render("ERR (nil status)"))
		default:
			row.WriteString("  ")
			row.WriteString(padTrunc(theme.Value.Render(cells[i].pending), pendingW))
			row.WriteString("  ")
			row.WriteString(padTrunc(theme.Value.Render(cells[i].queued), queuedW))
			row.WriteString("  ")
			row.WriteString(padTrunc(theme.Value.Render(cells[i].total), totalW))
			row.WriteString("  ")
			row.WriteString(theme.Label.Render(fmt.Sprintf("%dms", snap.latency/time.Millisecond)))
			row.WriteString("  ")
			row.WriteString(theme.OKText.Render("✓"))
		}

		cursor := "  "
		rowText := row.String()
		if i == s.cursor {
			cursor = theme.Cursor.Render("▸ ")
			rowText = theme.SelectedRow.Render(rowText)
		}
		b.WriteString("  ")
		b.WriteString(cursor)
		b.WriteString(rowText)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if s.inApp {
		b.WriteString(theme.Footer(theme.KeyNav, theme.KeyOpenDetail, theme.KeyBack) +
			theme.Help.Render(" · live updates every "+s.interval.String()))
	} else {
		b.WriteString(theme.Help.Render("q quits · live updates every " + s.interval.String()))
	}
	return b.String()
}
