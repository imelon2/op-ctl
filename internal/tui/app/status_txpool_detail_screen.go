package app

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/sshtunnel"
)

// statusTxPoolDetailScreen is the per-backend drill-down: a sortable
// list of every pending+queued tx for one backend, fed by one
// `txpool_content` call on entry and on each refresh tick. The
// returned slice is cached in `txs`; `enter` on a row pushes the
// tx-detail screen with a pointer into this cache — no per-click RPC.
//
// Gen-counter race protection and inFlight backpressure mirror the
// summary screen's contract: ticks at gen+1 are accepted, stale
// snapshots are dropped, and a tick fired while a previous fetch is
// outstanding skips the new fetch but re-arms the cadence.
type statusTxPoolDetailScreen struct {
	backend  config.Backend
	resolver *sshtunnel.Resolver
	refresh  time.Duration
	timeout  time.Duration

	txs        []elnode.TxPoolTx
	err        error
	observedAt time.Time
	pending    bool // true until the first fetch resolves
	gen        uint64
	inFlight   bool
	cursor     int
	offset     int

	width  int
	height int
}

func newStatusTxPoolDetailScreen(backend config.Backend, resolver *sshtunnel.Resolver, refresh, timeout time.Duration) statusTxPoolDetailScreen {
	return statusTxPoolDetailScreen{
		backend:  backend,
		resolver: resolver,
		refresh:  refresh,
		timeout:  timeout,
		pending:  true,
	}
}

// Init kicks off the first list fetch. When refresh > 0 the
// auto-refresh tick is scheduled via Update's branch on the first
// txpoolListTickMsg.
func (s statusTxPoolDetailScreen) Init() tea.Cmd {
	return func() tea.Msg { return txpoolListTickMsg{gen: 1} }
}

func (s statusTxPoolDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
		return s, nil

	case txpoolListTickMsg:
		if m.gen != s.gen+1 {
			return s, nil
		}
		if s.inFlight {
			// Backpressure: previous fetch hasn't returned. Skip the
			// new fetch but still re-arm the next tick so the cadence
			// is preserved.
			s.gen = m.gen
			if s.refresh > 0 {
				return s, statusTxPoolDetailTick(s.refresh, s.gen+1)
			}
			return s, nil
		}
		s.gen = m.gen
		s.inFlight = true
		cmds := []tea.Cmd{fetchTxPoolList(s.gen, s.resolver, s.backend, s.timeout)}
		if s.refresh > 0 {
			cmds = append(cmds, statusTxPoolDetailTick(s.refresh, s.gen+1))
		}
		return s, tea.Batch(cmds...)

	case txpoolListFetchedMsg:
		if m.gen != s.gen {
			// Stale: drop. inFlight stays cleared so the cadence can
			// recover from a missed fetch without manual intervention.
			s.inFlight = false
			return s, nil
		}
		s.inFlight = false
		s.pending = false
		s.txs = m.txs
		s.err = m.err
		s.observedAt = m.observedAt
		if s.cursor >= len(s.txs) {
			s.cursor = len(s.txs) - 1
			if s.cursor < 0 {
				s.cursor = 0
			}
		}
		return s, nil

	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		case "r":
			// Manual refresh pre-empts the in-flight fetch — the stale
			// result will be dropped by the gen guard.
			s.inFlight = true
			s.gen++
			return s, fetchTxPoolList(s.gen, s.resolver, s.backend, s.timeout)
		case "enter":
			if len(s.txs) == 0 {
				return s, nil
			}
			tx := &s.txs[s.cursor]
			return s, func() tea.Msg {
				return txDetailMsg{backend: s.backend, tx: tx}
			}
		case "down", "j":
			if len(s.txs) > 0 {
				s.cursor++
				if s.cursor >= len(s.txs) {
					s.cursor = len(s.txs) - 1
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
			if len(s.txs) > 0 {
				s.cursor = len(s.txs) - 1
			}
		case "pgdown", "ctrl+d", " ":
			if len(s.txs) > 0 {
				s.cursor += halfPage(s.height)
				if s.cursor >= len(s.txs) {
					s.cursor = len(s.txs) - 1
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

// fetchTxPoolList performs one txpool_content call for the backend
// under generation `gen`. An empty ExecutionRPCURL short-circuits to
// an ERR snapshot before touching the resolver.
func fetchTxPoolList(gen uint64, resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		now := time.Now()
		if strings.TrimSpace(b.ExecutionRPCURL) == "" {
			return txpoolListFetchedMsg{
				gen: gen, err: errors.New("missing execution_rpc_url"),
				observedAt: now,
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hc, err := resolver.HTTPClient(ctx, b.SSHJump)
		if err != nil {
			return txpoolListFetchedMsg{gen: gen, err: err, observedAt: now}
		}
		txs, _, err := elnode.TxPoolContent(ctx, hc, b.ExecutionRPCURL)
		return txpoolListFetchedMsg{
			gen:        gen,
			txs:        txs,
			err:        err,
			observedAt: time.Now(),
		}
	}
}

// statusTxPoolDetailTick schedules the next list-refresh tick at
// generation `nextGen`. Update accepts it only when nextGen == s.gen+1.
func statusTxPoolDetailTick(interval time.Duration, nextGen uint64) tea.Cmd {
	return tea.Tick(interval, func(time.Time) tea.Msg { return txpoolListTickMsg{gen: nextGen} })
}

// ---------- styles ----------

var (
	txdTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	txdSubtitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	txdLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	txdValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	txdMuteStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	txdHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	txdErrStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	txdCursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	txdSelectedBg    = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	txdPendingFlag   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	txdQueuedFlag    = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
)

// Minimum column widths. Actual widths are computed per render so that
// long values (e.g. full wei integers) don't push later columns out of
// alignment — padTrunc only pads, it doesn't truncate.
const (
	txdFromW  = 12
	txdNonceW = 6
	txdToW    = 12
	txdValueW = 14
	txdGasW   = 8
)

// View renders the detail screen.
func (s statusTxPoolDetailScreen) View() string {
	var b strings.Builder

	b.WriteString(txdTitleStyle.Render("txpool detail · "+s.backend.Name) + "  ")
	b.WriteString(txdSubtitleStyle.Render(s.backend.ExecutionRPCURL))
	b.WriteString("\n")

	// Status line: last refreshed + cadence + entry count.
	var refreshDesc string
	if s.refresh > 0 {
		refreshDesc = "refresh " + s.refresh.String()
	} else {
		refreshDesc = "manual refresh only"
	}
	var ageDesc string
	switch {
	case s.observedAt.IsZero():
		ageDesc = "polling…"
	default:
		ageDesc = "last refreshed " + humanAge(time.Since(s.observedAt)) + " ago"
	}
	b.WriteString("  ")
	b.WriteString(txdLabelStyle.Render(fmt.Sprintf(
		"%s · %s · %d txs", ageDesc, refreshDesc, len(s.txs),
	)))
	b.WriteString("\n")

	if s.err != nil {
		b.WriteString("  ")
		b.WriteString(txdErrStyle.Render("ERR " + truncate(s.err.Error(), 80)))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if s.pending {
		b.WriteString("  ")
		b.WriteString(txdMuteStyle.Render("polling…"))
		b.WriteString("\n")
		b.WriteString("\n")
		b.WriteString(s.footer())
		return b.String()
	}
	if len(s.txs) == 0 {
		b.WriteString("  ")
		b.WriteString(txdMuteStyle.Render("(empty pool)"))
		b.WriteString("\n")
		b.WriteString("\n")
		b.WriteString(s.footer())
		return b.String()
	}

	// Pre-render each cell once and grow column widths to fit, so a
	// long value (e.g. full wei integer) doesn't shift later columns.
	type rowCells struct{ from, nonce, to, value, gas string }
	cells := make([]rowCells, len(s.txs))
	fromW, nonceW, toW, valueW, gasW := txdFromW, txdNonceW, txdToW, txdValueW, txdGasW
	for i, tx := range s.txs {
		cells[i] = rowCells{
			from:  shrinkID(tx.From, txdFromW),
			nonce: fmt.Sprintf("%d", tx.Nonce),
			to:    shrinkID(tx.To, txdToW),
			value: formatValue(tx.Value),
			gas:   fmt.Sprintf("%d", tx.Gas),
		}
		if w := lipgloss.Width(cells[i].from); w > fromW {
			fromW = w
		}
		if w := lipgloss.Width(cells[i].nonce); w > nonceW {
			nonceW = w
		}
		if w := lipgloss.Width(cells[i].to); w > toW {
			toW = w
		}
		if w := lipgloss.Width(cells[i].value); w > valueW {
			valueW = w
		}
		if w := lipgloss.Width(cells[i].gas); w > gasW {
			gasW = w
		}
	}

	// Header.
	b.WriteString("    ")
	b.WriteString(padTrunc(txdLabelStyle.Render("from"), fromW))
	b.WriteString("  ")
	b.WriteString(padTrunc(txdLabelStyle.Render("nonce"), nonceW))
	b.WriteString("  ")
	b.WriteString(padTrunc(txdLabelStyle.Render("to"), toW))
	b.WriteString("  ")
	b.WriteString(padTrunc(txdLabelStyle.Render("value"), valueW))
	b.WriteString("  ")
	b.WriteString(padTrunc(txdLabelStyle.Render("gas"), gasW))
	b.WriteString("  ")
	b.WriteString(txdLabelStyle.Render("p/q"))
	b.WriteString("\n")

	for i, tx := range s.txs {
		var row strings.Builder
		row.WriteString(padTrunc(txdValueStyle.Render(cells[i].from), fromW))
		row.WriteString("  ")
		row.WriteString(padTrunc(txdValueStyle.Render(cells[i].nonce), nonceW))
		row.WriteString("  ")
		row.WriteString(padTrunc(txdValueStyle.Render(cells[i].to), toW))
		row.WriteString("  ")
		row.WriteString(padTrunc(txdValueStyle.Render(cells[i].value), valueW))
		row.WriteString("  ")
		row.WriteString(padTrunc(txdValueStyle.Render(cells[i].gas), gasW))
		row.WriteString("  ")
		if tx.Pending {
			row.WriteString(txdPendingFlag.Render("P"))
		} else {
			row.WriteString(txdQueuedFlag.Render("Q"))
		}

		cursor := "  "
		rowText := row.String()
		if i == s.cursor {
			cursor = txdCursorStyle.Render("▸ ")
			rowText = txdSelectedBg.Render(rowText)
		}
		b.WriteString("  ")
		b.WriteString(cursor)
		b.WriteString(rowText)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(s.footer())
	return b.String()
}

func (s statusTxPoolDetailScreen) footer() string {
	return txdHelpStyle.Render("↑/↓ j/k navigate · enter detail · r refresh · q back")
}

// humanAge renders a duration as a compact age string. Granularity:
// ms for <1s, then s / m / h boundaries.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d/time.Millisecond)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}

// formatValue renders a wei value compactly for the list view. Nil
// or zero values render as "(empty)" / "0"; otherwise the full wei
// integer is returned with a "wei" suffix.
func formatValue(v *big.Int) string {
	if v == nil {
		return "(empty)"
	}
	if v.Sign() == 0 {
		return "0"
	}
	return v.String() + " wei"
}
