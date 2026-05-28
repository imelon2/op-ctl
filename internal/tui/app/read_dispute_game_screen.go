package app

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"op-ctl/internal/l1"
	"op-ctl/internal/tui/theme"
)

// rowsPerPage is the gameAtIndex batch size: one HTTP POST fetches
// exactly this many rows from the L1 RPC. Tuned to balance:
//   - public-RPC rate limits (smaller batches are safer if the node
//     caps array sizes)
//   - paging UX (10 is a comfortable on-screen count that fits in any
//     terminal taller than 20 rows)
const rowsPerPage = 10

// --- messages ---

// readDGHeaderFetchedMsg carries the initial Version + gameCount
// result that gates the list rendering — until both arrive the page
// has nothing to fetch (count == 0 special-case shows an empty state).
type readDGHeaderFetchedMsg struct {
	version    string
	count      *big.Int
	versionErr error
	countErr   error
	latency    time.Duration
}

// readDGPageFetchedMsg carries one page's gameAtIndex batch result.
// gen tags the request so stale responses from a prior page are
// dropped when the operator pages quickly.
type readDGPageFetchedMsg struct {
	gen      uint64
	page     int
	listings []l1.GameListing
	errs     []error
	hardErr  error
	latency  time.Duration
}

// readGameSelectedMsg is emitted on enter; App.Update routes it to
// push the detail screen for the chosen row.
type readGameSelectedMsg struct {
	address string
}

// --- screen ---

// readDisputeGameScreen is the paginated game list. Layout:
//
//	read / dispute-game                  L1: https://... · Factory: 0x...
//	version: 1.4.0   gameCount: 260
//
//	  idx │ proxy                   │ created
//	  259 │ 0x123abc…def456         │ 2026-05-19T10:42Z (5m ago)
//	  258 │ 0x123abc…def456         │ ...
//	  ...
//
//	page 1 of 26 · ↑↓ select · ⏎ open · PgDn next · r refresh · q back
//
// Pagination is newest-first: page 0 covers indices [count-10 .. count-1]
// displayed top-down with the newest at the top. Lazy: only the visible
// page's batch is fetched.
type readDisputeGameScreen struct {
	l1RPCURL    string
	factoryAddr string
	timeout     time.Duration

	headerLoading bool
	version       string
	versionErr    error
	count         *big.Int
	countErr      error
	headerLatency time.Duration

	page        int
	pageLoading bool
	pageGen     uint64 // bumped each fetch; stale responses ignored
	pageRows    []l1.GameListing
	pageErrs    []error
	pageHardErr error
	pageLatency time.Duration

	cursor int

	width, height int
}

func newReadDisputeGameScreen(l1RPCURL, factoryAddr string, timeout time.Duration) readDisputeGameScreen {
	return readDisputeGameScreen{
		l1RPCURL:      l1RPCURL,
		factoryAddr:   factoryAddr,
		timeout:       timeout,
		headerLoading: true,
	}
}

func (s readDisputeGameScreen) Init() tea.Cmd {
	return fetchReadDGHeaderCmd(s.l1RPCURL, s.factoryAddr, s.timeout)
}

// fetchReadDGHeaderCmd issues Version + GameCount in a single batched
// RPC call — the two non-list reads happen together so the screen
// has one observable "header loaded" event.
func fetchReadDGHeaderCmd(l1RPCURL, factoryAddr string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		// Two independent calls — batch them so the header arrives in
		// one HTTP roundtrip instead of two.
		calls := []l1.EthCallReq{
			{To: factoryAddr, Data: l1.VersionSelectorData()},
			{To: factoryAddr, Data: l1.GameCountSelectorData()},
		}
		results, latency, err := l1.EthCallBatch(ctx, nil, l1RPCURL, calls)
		if err != nil {
			return readDGHeaderFetchedMsg{versionErr: err, countErr: err, latency: latency}
		}
		var ver string
		var verErr error
		if results[0].Err != nil {
			verErr = results[0].Err
		} else {
			ver, verErr = l1.DecodeVersionResult(results[0].Result)
		}
		var count *big.Int
		var countErr error
		if results[1].Err != nil {
			countErr = results[1].Err
		} else {
			count, countErr = l1.DecodeUint256Result(results[1].Result)
		}
		return readDGHeaderFetchedMsg{
			version: ver, versionErr: verErr,
			count: count, countErr: countErr,
			latency: latency,
		}
	}
}

// fetchReadDGPageCmd fetches rowsPerPage indices for the given page
// (newest-first). gen tags this fetch so a slow response from an
// older page can be discarded when it arrives after a fresh fetch.
func fetchReadDGPageCmd(l1RPCURL, factoryAddr string, timeout time.Duration, count *big.Int, page int, gen uint64) tea.Cmd {
	return func() tea.Msg {
		indices := pageIndices(count, page)
		if len(indices) == 0 {
			return readDGPageFetchedMsg{gen: gen, page: page}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		listings, errs, latency, err := l1.GameAtIndexBatch(ctx, nil, l1RPCURL, factoryAddr, indices)
		return readDGPageFetchedMsg{
			gen:      gen,
			page:     page,
			listings: listings,
			errs:     errs,
			hardErr:  err,
			latency:  latency,
		}
	}
}

// pageIndices returns up to rowsPerPage indices, newest-first, for
// the requested page number. page 0 covers [count-10 .. count-1]
// displayed top-down as count-1 ... count-10. Empty when count is
// nil/0 or page is out of range.
func pageIndices(count *big.Int, page int) []uint64 {
	if count == nil || count.Sign() <= 0 || page < 0 {
		return nil
	}
	total := count.Uint64()
	hi := int64(total) - int64(page)*int64(rowsPerPage)
	if hi <= 0 {
		return nil
	}
	lo := hi - rowsPerPage
	if lo < 0 {
		lo = 0
	}
	out := make([]uint64, 0, hi-lo)
	for i := hi - 1; i >= lo; i-- {
		out = append(out, uint64(i))
	}
	return out
}

func totalPages(count *big.Int) int {
	if count == nil || count.Sign() <= 0 {
		return 0
	}
	n := int(count.Uint64())
	return (n + rowsPerPage - 1) / rowsPerPage
}

func (s readDisputeGameScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height

	case readDGHeaderFetchedMsg:
		s.headerLoading = false
		s.version = m.version
		s.versionErr = m.versionErr
		s.count = m.count
		s.countErr = m.countErr
		s.headerLatency = m.latency
		if m.countErr == nil && m.count != nil && m.count.Sign() > 0 {
			s.pageLoading = true
			s.pageGen++
			return s, fetchReadDGPageCmd(s.l1RPCURL, s.factoryAddr, s.timeout, s.count, s.page, s.pageGen)
		}
		return s, nil

	case readDGPageFetchedMsg:
		if m.gen != s.pageGen || m.page != s.page {
			return s, nil // stale, ignore
		}
		s.pageLoading = false
		s.pageRows = m.listings
		s.pageErrs = m.errs
		s.pageHardErr = m.hardErr
		s.pageLatency = m.latency
		if s.cursor >= len(s.pageRows) {
			s.cursor = max0(len(s.pageRows) - 1)
		}
		return s, nil

	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

func (s readDisputeGameScreen) handleKey(m tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.String() {
	case "q", "esc", "ctrl+c":
		return s, func() tea.Msg { return popMsg{} }
	case "j", "down":
		if s.cursor < len(s.pageRows)-1 {
			s.cursor++
		}
		return s, nil
	case "k", "up":
		if s.cursor > 0 {
			s.cursor--
		}
		return s, nil
	case "g", "home":
		s.cursor = 0
		return s, nil
	case "G", "end":
		if len(s.pageRows) > 0 {
			s.cursor = len(s.pageRows) - 1
		}
		return s, nil
	case "pgdown", "right", "l":
		return s.advancePage(1)
	case "pgup", "left", "h":
		return s.advancePage(-1)
	case "r":
		if !s.pageLoading {
			s.pageLoading = true
			s.pageGen++
			return s, fetchReadDGPageCmd(s.l1RPCURL, s.factoryAddr, s.timeout, s.count, s.page, s.pageGen)
		}
	case "enter":
		if s.cursor < len(s.pageRows) {
			row := s.pageRows[s.cursor]
			if row.Proxy != "" {
				addr := row.Proxy
				return s, func() tea.Msg { return readGameSelectedMsg{address: addr} }
			}
		}
	}
	return s, nil
}

func (s readDisputeGameScreen) advancePage(delta int) (tea.Model, tea.Cmd) {
	if s.pageLoading || s.count == nil {
		return s, nil
	}
	totalP := totalPages(s.count)
	next := s.page + delta
	if next < 0 || next >= totalP {
		return s, nil
	}
	s.page = next
	s.cursor = 0
	s.pageLoading = true
	s.pageGen++
	return s, fetchReadDGPageCmd(s.l1RPCURL, s.factoryAddr, s.timeout, s.count, s.page, s.pageGen)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// --- view ---

// Table cell styles for the list view. These stay as named styles
// because tableStyleFunc returns them per (row, col); the colors come
// from the shared theme, with `Padding(0, 1)` for lipgloss/table's
// one-space breathing room inside each border-separated cell.
var (
	rdgListCellStyle      = lipgloss.NewStyle().Padding(0, 1)
	rdgListAddrStyle      = lipgloss.NewStyle().Padding(0, 1)
	rdgListColHeaderStyle = theme.ColHeader.Padding(0, 1)
	rdgListIdxColStyle    = theme.Label.Padding(0, 1)
	rdgListAgeStyle       = theme.Label.Padding(0, 1)
	rdgListSelectStyle    = theme.SelectedCell.Padding(0, 1)
	// gameType pills: 0 = Cannon (Permissionless), 1 = Permissioned.
	rdgListTypePermStyle = theme.WarnText.Bold(true).Padding(0, 1)
	rdgListTypeOpenStyle = theme.OKText.Bold(true).Padding(0, 1)
	rdgListTypeUnkStyle  = theme.Label.Padding(0, 1)
)

func (s readDisputeGameScreen) View() string {
	var b strings.Builder

	// Title + breadcrumb. The L1 RPC and factory address are on
	// separate lines because public RPC URLs are long and combining
	// them with the factory pushed the header off-screen on narrower
	// terminals.
	b.WriteString(theme.Title.Render("read / dispute-game"))
	b.WriteString("\n")
	b.WriteString(theme.Subtitle.Render("L1:      " + s.l1RPCURL))
	b.WriteString("\n")
	b.WriteString(theme.Subtitle.Render("Factory: " + s.factoryAddr))
	b.WriteString("\n")

	// Status line: version + gameCount as inline highlights — keeps
	// the operator's eye on "what chain am I looking at" without
	// needing a bordered card.
	b.WriteString(s.renderStatusLine())
	b.WriteString("\n\n")

	// Body: empty-state / loading / hard-error / table
	switch {
	case s.headerLoading:
		b.WriteString(theme.Subtitle.Render("  loading factory header ..."))
	case s.count != nil && s.count.Sign() == 0:
		b.WriteString(theme.Subtitle.Render("  (no games created yet)"))
	case s.pageLoading && len(s.pageRows) == 0:
		b.WriteString(theme.Subtitle.Render("  loading page ..."))
	case s.pageHardErr != nil:
		b.WriteString(theme.ErrText.Render(fmt.Sprintf("  ERR loading page: %v", s.pageHardErr)))
	default:
		b.WriteString(s.renderGameTable())
	}
	b.WriteString("\n\n")

	// Footer
	totalP := totalPages(s.count)
	pageInfo := fmt.Sprintf("page %d of %d", s.page+1, totalP)
	if totalP == 0 {
		pageInfo = "page —"
	}
	b.WriteString(theme.Subtitle.Render(pageInfo))
	b.WriteString("\n")
	b.WriteString(theme.Help.Render("↑↓ select · ⏎ open · PgDn/→ next · PgUp/← prev · g/G top/bottom · r refresh · q back"))

	return b.String()
}

// renderStatusLine produces the single inline `version: X · gameCount: Y`
// summary above the table. Either pill becomes an ERR cell when the
// underlying RPC failed, so the operator can see WHICH header call
// failed without scrolling.
func (s readDisputeGameScreen) renderStatusLine() string {
	if s.headerLoading {
		return theme.Subtitle.Render("⏳ fetching factory version + gameCount ...")
	}
	var parts []string
	if s.versionErr != nil {
		parts = append(parts, theme.ErrText.Render("version: ERR "+s.versionErr.Error()))
	} else {
		parts = append(parts, theme.Subtitle.Render("version: ")+theme.Header.Render(s.version))
	}
	if s.countErr != nil {
		parts = append(parts, theme.ErrText.Render("gameCount: ERR "+s.countErr.Error()))
	} else {
		cs := "0"
		if s.count != nil {
			cs = s.count.String()
		}
		parts = append(parts, theme.Subtitle.Render("gameCount: ")+theme.Header.Render(cs))
	}
	return strings.Join(parts, theme.Subtitle.Render("  ·  "))
}

// renderGameTable builds the lipgloss/table for the current page.
// Lipgloss-table owns column-width computation, header rendering,
// and row separator alignment — no more hand-tuned dash counts.
// Selection is communicated by per-cell background highlight via
// StyleFunc, so the cursor row stays visible even when the operator's
// eye is on the rightmost column.
func (s readDisputeGameScreen) renderGameTable() string {
	rows := make([][]string, len(s.pageRows))
	for i, row := range s.pageRows {
		if e := s.pageErrs[i]; e != nil {
			rows[i] = []string{
				fmt.Sprintf("%d", row.Index), "—", "—", fmt.Sprintf("ERR %v", e), "—",
			}
			continue
		}
		created, age := splitTimestamp(row.Timestamp)
		rows[i] = []string{
			fmt.Sprintf("%d", row.Index),
			gameTypeShort(row.GameType),
			row.Proxy,
			created,
			age,
		}
	}
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(theme.Mute).
		BorderRow(false).
		BorderColumn(true).
		BorderTop(true).
		BorderBottom(true).
		BorderLeft(false).
		BorderRight(false).
		Headers("#", "TYPE", "PROXY", "CREATED (UTC)", "AGE").
		Rows(rows...).
		StyleFunc(s.tableStyleFunc)
	return t.Render()
}

// tableStyleFunc selects the per-cell style for the lipgloss table.
// Header row gets the bold colored style; the cursor row gets a full
// background highlight; everything else uses column-specific muted /
// pill / value styles so visual weight tracks meaning (index is
// dim, game-type is colored, proxy is plain, timestamps are muted).
func (s readDisputeGameScreen) tableStyleFunc(row, col int) lipgloss.Style {
	if row == table.HeaderRow {
		return rdgListColHeaderStyle
	}
	if row == s.cursor && len(s.pageErrs) > row && s.pageErrs[row] == nil {
		return rdgListSelectStyle
	}
	// Color the type pill by gameType value when available.
	if col == 1 && row >= 0 && row < len(s.pageRows) {
		switch s.pageRows[row].GameType {
		case 0:
			return rdgListTypeOpenStyle
		case 1:
			return rdgListTypePermStyle
		default:
			return rdgListTypeUnkStyle
		}
	}
	switch col {
	case 0:
		return rdgListIdxColStyle
	case 2:
		return rdgListAddrStyle
	case 3, 4:
		return rdgListAgeStyle
	}
	return rdgListCellStyle
}

// splitTimestamp returns (UTC datetime string, short age string)
// formatted for compact rendering inside the table.
func splitTimestamp(unix uint64) (string, string) {
	t := time.Unix(int64(unix), 0).UTC()
	return t.Format("2006-01-02 15:04:05"), humanRelative(t)
}

// gameTypeShort returns the short label used in the TYPE column.
// 0 = Cannon (Permissionless), 1 = Permissioned. Unknown numeric types
// fall back to the raw integer so the operator can see which on-chain
// value drifted.
func gameTypeShort(gt uint32) string {
	switch gt {
	case 0:
		return "Cannon"
	case 1:
		return "Permissioned"
	default:
		return fmt.Sprintf("type=%d", gt)
	}
}

// humanRelative is a small humanizer for "5m" / "1h" / "3d" style
// suffixes used in the AGE column and the detail screen's time rows.
// The "ago" word is implied by context (the AGE column header and the
// parenthetical on detail-screen rows) so we keep the cell tight.
func humanRelative(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// RunReadDisputeGame keeps the same CLI-only entry signature the
// Phase-1 stub exposed; the unified App still invokes
// newReadDisputeGameScreen via push (no second tea.Program). Standalone
// invocation (`op-ctl read dispute-game`) lands here.
func RunReadDisputeGame(ctx context.Context, l1RPCURL, factoryAddr string, timeout time.Duration) error {
	screen := newReadDisputeGameScreen(l1RPCURL, factoryAddr, timeout)
	_, err := tea.NewProgram(screen, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}
