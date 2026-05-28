// Package peers renders one opp2p_peers dump as a designed, scrollable
// bubbletea view: a bordered header with backend / counts, then three
// color-coded sections (Connected / Other / Banned) with aligned columns
// and namespace-resolved names highlighted.
//
// The model is a passive viewer — there is no live refresh. Re-run
// `op-ctl p2p consensus` to take a fresh snapshot.
package peers

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
	"op-ctl/internal/tui/theme"
)

// DoneMsg is emitted when the operator dismisses the peers view (q /
// esc / ctrl+c). Unified-app callers route this to "pop screen";
// standalone Run() converts it into tea.Quit.
type DoneMsg struct{}

// SelectedMsg is emitted when the operator presses enter on a peer
// row. Entry is nil for banned peers (the dump only carries their
// peerID); Name is the namespace-resolved backend name or "" if
// unknown. The unified-app program turns this into a push of a
// peer-detail screen; standalone callers can ignore it.
type SelectedMsg struct {
	PeerID string
	Name   string
	Entry  *opnode.PeerEntry
	Banned bool
}

type sectionKind int

const (
	secConnected sectionKind = iota
	secOther
	secBanned
)

// peerRow is the per-peer projection actually rendered. We pre-resolve
// the namespace name once at construction time so the hot View() path
// doesn't re-walk the index for every redraw. entry is nil for banned
// peers (the dump only carries their peerID, no full PeerEntry).
type peerRow struct {
	name          string // resolved namespace name; "" if unknown
	peerID        string
	connectedness int
	direction     int
	latencyMS     uint64
	section       sectionKind
	entry         *opnode.PeerEntry
}

// Model holds the prepared row buckets, the cursor + scroll offset,
// and the current terminal size. Sections are sorted once in New().
//
// rows is the flat selectable list (Connected → Other → Banned, each
// already sorted by name-then-peerID). cursor indexes into rows.
// offset controls which body line is the first line on screen and is
// recomputed on each View() to keep the cursor visible.
type Model struct {
	backend config.Backend
	dump    *opnode.PeerDump

	connected []peerRow
	other     []peerRow
	banned    []peerRow
	rows      []peerRow

	cursor int

	width  int
	height int
	offset int
}

// New buckets dump.Peers by Connectedness (Connected vs everything
// else), pulls names through idx, and sorts each bucket: matched names
// first (alphabetical), then bare peerIDs (alphabetical). Latency from
// op-node is in nanoseconds; we convert to ms once here. The flat
// rows slice is rebuilt from the sorted sections so cursor navigation
// hits Connected → Other → Banned in render order.
func New(b config.Backend, dump *opnode.PeerDump, idx *namespace.Index) Model {
	m := Model{backend: b, dump: dump}
	for id, e := range dump.Peers {
		entry := e
		r := peerRow{
			name:          idx.Lookup(id),
			peerID:        id,
			connectedness: e.Connectedness,
			direction:     e.Direction,
			latencyMS:     e.Latency / 1_000_000,
			entry:         &entry,
		}
		if e.Connectedness == 1 {
			r.section = secConnected
			m.connected = append(m.connected, r)
		} else {
			r.section = secOther
			m.other = append(m.other, r)
		}
	}
	for _, id := range dump.BannedPeers {
		m.banned = append(m.banned, peerRow{
			name:    idx.Lookup(id),
			peerID:  id,
			section: secBanned,
		})
	}
	sortRows(m.connected)
	sortRows(m.other)
	sortRows(m.banned)
	m.rows = make([]peerRow, 0, len(m.connected)+len(m.other)+len(m.banned))
	m.rows = append(m.rows, m.connected...)
	m.rows = append(m.rows, m.other...)
	m.rows = append(m.rows, m.banned...)
	return m
}

func sortRows(rs []peerRow) {
	sort.SliceStable(rs, func(a, b int) bool {
		an, bn := rs[a].name, rs[b].name
		if (an != "") != (bn != "") {
			return an != ""
		}
		if an != bn {
			return an < bn
		}
		return rs[a].peerID < rs[b].peerID
	})
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, func() tea.Msg { return DoneMsg{} }
		case "enter":
			if len(m.rows) == 0 {
				return m, nil
			}
			r := m.rows[m.cursor]
			sel := SelectedMsg{
				PeerID: r.peerID,
				Name:   r.name,
				Entry:  r.entry,
				Banned: r.section == secBanned,
			}
			return m, func() tea.Msg { return sel }
		case "down", "j":
			m.cursor++
		case "up", "k":
			m.cursor--
		case "pgdown", "ctrl+d", " ":
			m.cursor += halfPage(m.height)
		case "pgup", "ctrl+u", "b":
			m.cursor -= halfPage(m.height)
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(m.rows) - 1
		}
		m.cursor = clamp(m.cursor, 0, len(m.rows)-1)
	}
	return m, nil
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func halfPage(h int) int {
	if h < 4 {
		return 1
	}
	return h / 2
}

const (
	nameColW    = 14
	stateColW   = 14
	latencyColW = 8
	rowGutter   = 4 // "  " row indent + " " separators
)

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	head := m.renderHeader()
	body, cursorLine := m.renderBody()
	footer := theme.Footer(theme.KeyNav, theme.KeyOpenDetail, theme.KeyQuit)

	avail := m.height - lipgloss.Height(head) - 1
	if avail < 1 {
		avail = 1
	}

	bodyLines := strings.Split(body, "\n")

	// Auto-scroll to keep the cursor row in view.
	off := m.offset
	if cursorLine >= 0 {
		if cursorLine < off {
			off = cursorLine
		}
		if cursorLine >= off+avail {
			off = cursorLine - avail + 1
		}
	}
	maxOffset := len(bodyLines) - avail
	if maxOffset < 0 {
		maxOffset = 0
	}
	if off > maxOffset {
		off = maxOffset
	}
	if off < 0 {
		off = 0
	}

	end := off + avail
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[off:end]
	for len(visible) < avail {
		visible = append(visible, "")
	}

	return head + "\n" + strings.Join(visible, "\n") + "\n" + footer
}

func (m Model) renderHeader() string {
	title := theme.Title.Render(m.backend.Name) + "  " + theme.Subtitle.Render(m.backend.ConsensusRPCURL)
	badges := lipgloss.JoinHorizontal(lipgloss.Top,
		theme.OKBadge.Render(fmt.Sprintf(" connected %d ", m.dump.TotalConnected)),
		"  ",
		theme.WarnBadge.Render(fmt.Sprintf(" known %d ", len(m.dump.Peers))),
		"  ",
		theme.ErrBadge.Render(fmt.Sprintf(" banned %d ", len(m.dump.BannedPeers))),
	)
	innerW := m.width - 4
	if innerW < 20 {
		innerW = 20
	}
	return theme.HeaderBox.Width(innerW).Render(title + "\n" + badges)
}

// renderBody renders the three sections and returns (body, cursorLine)
// where cursorLine is the body-relative line number containing the
// currently selected peer row — needed by View() to clamp the scroll
// offset so the cursor stays visible.
func (m Model) renderBody() (string, int) {
	peerIDW := m.width - rowGutter - 2 /* cursor */ - nameColW - 1 - stateColW - 1 - latencyColW - 2
	if peerIDW < 18 {
		peerIDW = 18
	}

	var b strings.Builder
	cursorLine := -1
	rowIdx := 0
	lineNo := 0

	writeSection := func(style lipgloss.Style, icon, title string, rows []peerRow, kind sectionKind) {
		head := style.Render(icon+" "+title) +
			theme.Mute.Render(fmt.Sprintf("  (%d)", len(rows)))
		b.WriteString(head + "\n")
		lineNo++
		if len(rows) == 0 {
			b.WriteString(theme.Mute.Render("    (none)") + "\n")
			lineNo++
			return
		}
		for _, r := range rows {
			selected := rowIdx == m.cursor
			if selected {
				cursorLine = lineNo
			}
			b.WriteString(m.renderRow(r, kind, peerIDW, selected) + "\n")
			lineNo++
			rowIdx++
		}
	}

	writeSection(theme.OKText.Bold(true), "●", "Connected", m.connected, secConnected)
	b.WriteString("\n")
	lineNo++
	writeSection(theme.WarnText.Bold(true), "◐", "Other", m.other, secOther)
	b.WriteString("\n")
	lineNo++
	writeSection(theme.ErrText.Bold(true), "✕", "Banned", m.banned, secBanned)

	return strings.TrimRight(b.String(), "\n"), cursorLine
}

func (m Model) renderRow(r peerRow, kind sectionKind, peerIDW int, selected bool) string {
	var name string
	if r.name != "" {
		name = theme.Name.Render(padTrunc(r.name, nameColW))
	} else {
		name = theme.Mute.Render(padTrunc("·", nameColW))
	}
	pid := theme.Value.Render(padTrunc(shrinkPeerID(r.peerID, peerIDW), peerIDW))

	var stateCell, latencyCell string
	switch kind {
	case secConnected:
		stateCell = theme.Label.Render(padTrunc(directionLong(r.direction), stateColW))
		latencyCell = theme.Label.Render(padLeft(fmt.Sprintf("%dms", r.latencyMS), latencyColW))
	case secOther:
		stateCell = theme.Label.Render(padTrunc(connectednessLabel(r.connectedness), stateColW))
		latencyCell = strings.Repeat(" ", latencyColW)
	case secBanned:
		stateCell = strings.Repeat(" ", stateColW)
		latencyCell = strings.Repeat(" ", latencyColW)
	}

	cursor := "  "
	if selected {
		cursor = theme.Cursor.Render("▸ ")
	}
	row := cursor + name + " " + pid + " " + stateCell + " " + latencyCell
	if selected {
		row = theme.SelectedRow.Render(row)
	}
	return "  " + row
}

func padTrunc(s string, w int) string {
	rs := []rune(s)
	if len(rs) > w {
		if w >= 1 {
			return string(rs[:w-1]) + "…"
		}
		return ""
	}
	return s + strings.Repeat(" ", w-len(rs))
}

func padLeft(s string, w int) string {
	rs := []rune(s)
	if len(rs) > w {
		return string(rs[len(rs)-w:])
	}
	return strings.Repeat(" ", w-len(rs)) + s
}

// shrinkPeerID compresses long peer IDs with a middle ellipsis so the
// distinguishing prefix and suffix both stay visible. With width <= 4
// it falls back to a hard truncation.
func shrinkPeerID(id string, w int) string {
	if len(id) <= w {
		return id
	}
	if w < 5 {
		return id[:w]
	}
	head := (w - 1) / 2
	tail := w - 1 - head
	return id[:head] + "…" + id[len(id)-tail:]
}

func connectednessLabel(c int) string {
	switch c {
	case 0:
		return "NotConnected"
	case 1:
		return "Connected"
	case 2:
		return "CanConnect"
	case 3:
		return "CannotConnect"
	case 4:
		return "Limited"
	default:
		return fmt.Sprintf("Unknown(%d)", c)
	}
}

func directionLong(d int) string {
	switch d {
	case 1:
		return "Inbound"
	case 2:
		return "Outbound"
	default:
		return "—"
	}
}

// runner wraps Model so the standalone Run() can quit on DoneMsg.
// SelectedMsg is dropped (no detail screen for the standalone case)
// so pressing enter in `--plain` flows just no-ops.
type runner struct{ inner Model }

func (r runner) Init() tea.Cmd { return r.inner.Init() }

func (r runner) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case DoneMsg:
		return r, tea.Quit
	case SelectedMsg:
		return r, nil
	}
	next, cmd := r.inner.Update(msg)
	r.inner = next.(Model)
	return r, cmd
}

func (r runner) View() string { return r.inner.View() }

// Run renders the dump in alt-screen mode and blocks until the user
// quits. AltScreen keeps the designed view from polluting scrollback.
// Kept for one-off CLI flows; the unified app program embeds Model
// directly and handles DoneMsg itself.
func Run(b config.Backend, dump *opnode.PeerDump, idx *namespace.Index) error {
	_, err := tea.NewProgram(runner{inner: New(b, dump, idx)}, tea.WithAltScreen()).Run()
	return err
}
