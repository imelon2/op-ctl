package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/etherscan"
	"op-ctl/internal/tui/keymap"
	"op-ctl/internal/tui/theme"
)

const batchRowsPerPage = 10

// --- messages ---

// readBatchPageFetchedMsg carries one page of cached rows back from
// the store fetch goroutine. gen tags this fetch so a slow result
// arriving after the operator has paged forward is dropped instead
// of overwriting fresher data.
type readBatchPageFetchedMsg struct {
	gen      uint64
	page     int
	rows     []etherscan.Tx
	total    int
	lastSync time.Time
	avgBlocks float64
	avgSecs   float64
	err      error
}

// readBatchTxSelectedMsg is emitted when the operator hits enter on a
// list row. App.Update routes it to push the per-tx detail screen,
// mirroring readGameSelectedMsg.
type readBatchTxSelectedMsg struct {
	txHash string
}

// --- screen ---

// readBatchScreen is the paginated batch tx list driven by the
// per-chain SQLite cache. Empty cache renders "(no batches yet; cache empty)".
type readBatchScreen struct {
	store *batchcache.Store

	page      int
	cursor    int
	rows      []etherscan.Tx
	total     int
	pageGen   uint64
	lastSync  time.Time
	avgBlocks float64
	avgSecs   float64
	loading   bool
	err       error
	tsMode    keymap.TimeMode

	width, height int
}

func newReadBatchScreen(store *batchcache.Store) readBatchScreen {
	// pageGen is seeded here (not Init) because Init has a value
	// receiver and mutations there don't survive to Update.
	return readBatchScreen{store: store, loading: true, pageGen: 1}
}

func (s readBatchScreen) Init() tea.Cmd {
	return fetchReadBatchPageCmd(s.store, 0, s.pageGen)
}

// fetchReadBatchPageCmd reads one page from the store (newest first)
// + the total count + the last_synced_at meta so the header line stays
// in sync. Generation-tagged so a slow result from page i lands after
// the operator has moved to page i+1 is dropped.
func fetchReadBatchPageCmd(store *batchcache.Store, page int, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		total, err := store.Count(ctx)
		if err != nil {
			return readBatchPageFetchedMsg{gen: gen, page: page, err: err}
		}
		offset := page * batchRowsPerPage
		rows, err := store.List(ctx, batchRowsPerPage, offset)
		if err != nil {
			return readBatchPageFetchedMsg{gen: gen, page: page, total: total, err: err}
		}
		last, _ := store.LastSyncedAt()
		avgBlocks, avgSecs, _ := store.AverageGaps(ctx)
		return readBatchPageFetchedMsg{
			gen:       gen,
			page:      page,
			rows:      rows,
			total:     total,
			lastSync:  last,
			avgBlocks: avgBlocks,
			avgSecs:   avgSecs,
		}
	}
}

func (s readBatchScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		return s, nil

	case readBatchPageFetchedMsg:
		// Drop stale generations to avoid clobbering a fresher page.
		if m.gen != s.pageGen {
			return s, nil
		}
		s.loading = false
		s.err = m.err
		s.rows = m.rows
		s.total = m.total
		s.lastSync = m.lastSync
		s.avgBlocks = m.avgBlocks
		s.avgSecs = m.avgSecs
		if s.cursor >= len(s.rows) {
			if len(s.rows) == 0 {
				s.cursor = 0
			} else {
				s.cursor = len(s.rows) - 1
			}
		}
		return s, nil

	case tea.KeyMsg:
		switch {
		case keymap.Back.Matches(m):
			return s, func() tea.Msg { return popMsg{} }
		case keymap.Down.Matches(m):
			if s.cursor < len(s.rows)-1 {
				s.cursor++
			}
			return s, nil
		case keymap.Up.Matches(m):
			if s.cursor > 0 {
				s.cursor--
			}
			return s, nil
		case keymap.PageNext.Matches(m), m.String() == "right", m.String() == "l":
			if (s.page+1)*batchRowsPerPage < s.total {
				s.page++
				s.cursor = 0
				s.pageGen++
				s.loading = true
				return s, fetchReadBatchPageCmd(s.store, s.page, s.pageGen)
			}
			return s, nil
		case keymap.PagePrev.Matches(m), m.String() == "left", m.String() == "h":
			if s.page > 0 {
				s.page--
				s.cursor = 0
				s.pageGen++
				s.loading = true
				return s, fetchReadBatchPageCmd(s.store, s.page, s.pageGen)
			}
			return s, nil
		case keymap.Refresh.Matches(m):
			s.pageGen++
			s.loading = true
			return s, fetchReadBatchPageCmd(s.store, s.page, s.pageGen)
		case keymap.TimeCycle.Matches(m):
			s.tsMode = s.tsMode.Next()
			return s, nil
		case keymap.Enter.Matches(m):
			if s.cursor < 0 || s.cursor >= len(s.rows) {
				return s, nil
			}
			hash := s.rows[s.cursor].Hash
			return s, func() tea.Msg { return readBatchTxSelectedMsg{txHash: hash} }
		}
	}
	return s, nil
}

func (s readBatchScreen) View() string {
	var b strings.Builder

	// Header — breadcrumb + status line in the dispute-game style
	// (label in Subtitle, value highlighted in Header).
	b.WriteString(theme.Title.Render("read / batch"))
	b.WriteString("\n")
	parts := []string{theme.Subtitle.Render("batch count: ") + theme.Header.Render(fmt.Sprintf("%d", s.total))}
	if s.total >= 2 {
		parts = append(parts,
			theme.Subtitle.Render("avg interval: ")+theme.Header.Render(formatSecondsCompact(s.avgSecs)),
			theme.Subtitle.Render("avg blocks/batch: ")+theme.Header.Render(fmt.Sprintf("%.1f", s.avgBlocks)),
		)
	}
	if !s.lastSync.IsZero() {
		parts = append(parts,
			theme.Subtitle.Render("last_sync: ")+theme.Header.Render(time.Since(s.lastSync).Round(time.Second).String()+" ago"),
		)
	}
	b.WriteString(strings.Join(parts, theme.Subtitle.Render("  ·  ")))
	b.WriteString("\n\n")

	if s.err != nil {
		b.WriteString(theme.ErrLine("ERR: " + s.err.Error()))
		b.WriteString("\n")
		return b.String()
	}
	if s.loading && s.total == 0 {
		b.WriteString(theme.Pending.Render("loading…"))
		return b.String()
	}
	if s.total == 0 {
		b.WriteString(theme.Label.Render("(no batches yet; cache empty)"))
		b.WriteString("\n\n")
		b.WriteString(theme.Help.Render("q back"))
		return b.String()
	}

	headerStyle := theme.ColHeader.Padding(0, 1)
	selectStyle := theme.SelectedCell.Padding(0, 1)
	cellStyle := lipgloss.NewStyle().Padding(0, 1)
	now := time.Now()
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(theme.ColorMuted)).
		Headers("id", "block", s.tsMode.HeaderLabel(), "ago", "tx_hash").
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			if row == s.cursor {
				return selectStyle
			}
			return cellStyle
		})
	// id counts up with chain age: the OLDEST cached row is id=1,
	// the newest is id=total. So the topmost newest-first row on
	// page 0 reads s.total, and ids decrement down the page.
	topID := s.total - s.page*batchRowsPerPage
	for i, r := range s.rows {
		t.Row(
			fmt.Sprintf("%d", topID-i),
			fmt.Sprintf("%d", r.BlockNumber),
			s.tsMode.Format(r.TimeStamp),
			relativeAgo(r.TimeStamp, now),
			r.Hash,
		)
	}
	b.WriteString(t.Render())
	b.WriteString("\n")

	// Footer — pagination + the standard keymap hints.
	pageCount := (s.total + batchRowsPerPage - 1) / batchRowsPerPage
	b.WriteString(theme.Subtitle.Render(fmt.Sprintf("page %d of %d", s.page+1, pageCount)))
	b.WriteString("\n")
	b.WriteString(keymap.Footer(
		keymap.Navigate, keymap.Enter, keymap.PageNext, keymap.PagePrev,
		keymap.Refresh, keymap.TimeCycle, keymap.Back,
	))
	return b.String()
}

// relativeAgo formats `now - unix` as a compact "Xh Ym ago" string.
// Anything older than 30 days falls back to a date so the column stays
// narrow on historical rows.
func relativeAgo(unix int64, now time.Time) string {
	if unix == 0 {
		return ""
	}
	d := now.Sub(time.Unix(unix, 0))
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm ago", h, m)
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return time.Unix(unix, 0).Local().Format("2006-01-02")
	}
}

// formatSecondsCompact renders a duration-in-seconds as "Xs" / "Xm Ys"
// / "Xh Ym" so the header stays narrow for any realistic batch cadence.
func formatSecondsCompact(secs float64) string {
	d := time.Duration(secs * float64(time.Second))
	if d <= 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh %dm", h, m)
	}
}

// humanBytes formats a byte count as "130KB" / "1.2MB" / "42B".
func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

