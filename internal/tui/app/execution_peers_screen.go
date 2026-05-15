package app

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/namespace"
)

// executionPeersScreen renders the result of admin_peers + net_peerCount
// for one execution-layer backend. Two top stats (counts) sit above a
// scrollable, cursor-selectable peer list; pressing enter on a row
// pushes a detail screen with the full AdminPeer.
//
// admin_peers can fail when the operator hasn't enabled the `admin`
// JSON-RPC namespace on op-geth/reth. We detect that case (RPC code
// -32601) and replace the list with a tailored hint instead of a raw
// error string — the count from net_peerCount stays usable since it
// lives in the always-on `net` namespace.
type executionPeersScreen struct {
	backend  config.Backend
	count    uint64
	countErr error
	peers    []executionPeerRow
	peersErr error

	cursor int
	width  int
	height int
	offset int
}

type executionPeerRow struct {
	name string
	peer elnode.AdminPeer
}

func newExecutionPeersScreen(
	b config.Backend,
	count uint64, countErr error,
	peers []elnode.AdminPeer, peersErr error,
	idx *namespace.Index,
) executionPeersScreen {
	rows := make([]executionPeerRow, 0, len(peers))
	for _, p := range peers {
		rows = append(rows, executionPeerRow{name: idx.Lookup(p.ID), peer: p})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		an, bn := rows[i].name, rows[j].name
		if (an != "") != (bn != "") {
			return an != ""
		}
		if an != bn {
			return an < bn
		}
		return rows[i].peer.ID < rows[j].peer.ID
	})
	return executionPeersScreen{
		backend:  b,
		count:    count,
		countErr: countErr,
		peers:    rows,
		peersErr: peersErr,
	}
}

func (s executionPeersScreen) Init() tea.Cmd { return nil }

func (s executionPeersScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		case "enter":
			if len(s.peers) == 0 {
				return s, nil
			}
			r := s.peers[s.cursor]
			return s, func() tea.Msg {
				return executionPeerDetailMsg{
					backend: s.backend,
					name:    r.name,
					peer:    r.peer,
				}
			}
		case "down", "j":
			if len(s.peers) > 0 {
				s.cursor++
				if s.cursor >= len(s.peers) {
					s.cursor = len(s.peers) - 1
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
			if len(s.peers) > 0 {
				s.cursor = len(s.peers) - 1
			}
		case "pgdown", "ctrl+d", " ":
			s.cursor += halfPage(s.height)
			if s.cursor >= len(s.peers) {
				s.cursor = len(s.peers) - 1
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

// ---------- styles ----------

var (
	exTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	exURLStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	exLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	exValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	exNameStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	exDimNameStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	exMuteStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	exHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	exDirInStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	exDirOutStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	exCursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	exSelectedBgStyle = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	exErrTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	exErrTextStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

	exBadgeBase   = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("0"))
	exBadgeOK     = exBadgeBase.Background(lipgloss.Color("10"))
	exBadgeWarn   = exBadgeBase.Background(lipgloss.Color("11"))
	exBadgeErr    = exBadgeBase.Background(lipgloss.Color("9"))
)

const (
	exNameColW    = 14
	exVersionColW = 22
	exDirColW     = 8
)

// ---------- view ----------

func (s executionPeersScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	header := s.renderHeader()
	body, cursorLine := s.renderBody()
	footer := exHelpStyle.Render("↑/↓ j/k navigate · enter detail · q back")

	headerLines := strings.Split(header, "\n")
	avail := s.height - len(headerLines) - 1
	if avail < 1 {
		avail = 1
	}

	bodyLines := strings.Split(body, "\n")
	off := s.offset
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
	return header + "\n" + strings.Join(visible, "\n") + "\n" + footer
}

// renderHeader builds the title + counts block. countErr / peersErr
// land here as compact red badges so the operator sees at a glance
// which call worked and which didn't.
func (s executionPeersScreen) renderHeader() string {
	var b strings.Builder
	b.WriteString(exTitleStyle.Render(s.backend.Name) + "  " +
		exURLStyle.Render(s.backend.ExecutionRPCURL) + "\n")

	countCell := exValueStyle.Render(fmt.Sprintf("%d", s.count))
	if s.countErr != nil {
		countCell = exErrTextStyle.Render("✕ " + s.countErr.Error())
	}
	b.WriteString("  " + exLabelStyle.Render(padRight("net_peerCount", 16)) + "  " + countCell + "\n")

	peersCell := exValueStyle.Render(fmt.Sprintf("%d", len(s.peers)))
	if s.peersErr != nil {
		if elnode.IsMethodNotFound(s.peersErr) {
			peersCell = exBadgeWarn.Render(" admin disabled ")
		} else {
			peersCell = exErrTextStyle.Render("✕ " + s.peersErr.Error())
		}
	}
	b.WriteString("  " + exLabelStyle.Render(padRight("admin_peers", 16)) + "  " + peersCell + "\n")
	return b.String()
}

// renderBody returns (body, cursorLine). When admin_peers errored we
// substitute an explanation block; cursorLine == -1 in that case so
// the auto-scroll math doesn't interfere.
func (s executionPeersScreen) renderBody() (string, int) {
	if s.peersErr != nil {
		return s.renderError(), -1
	}
	if len(s.peers) == 0 {
		return "\n  " + exMuteStyle.Render("(no peers reported)"), -1
	}
	return s.renderRows()
}

func (s executionPeersScreen) renderError() string {
	var b strings.Builder
	b.WriteString("\n")
	if elnode.IsMethodNotFound(s.peersErr) {
		b.WriteString("  " + exErrTitleStyle.Render("admin_peers unavailable") + "\n\n")
		b.WriteString("  " + exMuteStyle.Render(s.peersErr.Error()) + "\n\n")
		b.WriteString("  " + exValueStyle.Render(
			"This execution node hasn't enabled the admin JSON-RPC namespace,") + "\n")
		b.WriteString("  " + exValueStyle.Render(
			"so per-peer detail can't be retrieved. net_peerCount lives in the") + "\n")
		b.WriteString("  " + exValueStyle.Render(
			"net namespace and still works.") + "\n\n")
		b.WriteString("  " + exLabelStyle.Render("To enable on op-geth / geth:") + "\n")
		b.WriteString("  " + exValueStyle.Render("  --http.api eth,net,web3,admin") + "\n")
		b.WriteString("  " + exValueStyle.Render("  --http.addr 0.0.0.0  --http") + "\n\n")
		b.WriteString("  " + exLabelStyle.Render("To enable on op-reth:") + "\n")
		b.WriteString("  " + exValueStyle.Render("  --http  --http.api eth,net,web3,admin") + "\n")
	} else {
		b.WriteString("  " + exErrTitleStyle.Render("admin_peers failed") + "\n\n")
		b.WriteString("  " + exErrTextStyle.Render(s.peersErr.Error()) + "\n")
	}
	return b.String()
}

// renderRows formats the peer list with the cursor highlight. Returns
// the line index of the cursor row so View() can scroll it into view.
func (s executionPeersScreen) renderRows() (string, int) {
	idW := s.width - 4 /* gutter */ - 2 /* cursor */ - exNameColW - 1 - exVersionColW - 1 - exDirColW - 1 - 21 /* address */ - 2
	if idW < 12 {
		idW = 12
	}

	var b strings.Builder
	b.WriteString("\n")
	cursorLine := -1
	lineNo := 1 // we just wrote one "\n"
	for i, r := range s.peers {
		var nameCell string
		if r.name != "" {
			nameCell = exNameStyle.Render(padTrunc(r.name, exNameColW))
		} else {
			nameCell = exDimNameStyle.Render(padTrunc("·", exNameColW))
		}

		idCell := exValueStyle.Render(padTrunc(shrinkID(r.peer.ID, idW), idW))
		ver := r.peer.Name
		if ver == "" {
			ver = "(no name)"
		}
		versionCell := exMuteStyle.Render(padTrunc(ver, exVersionColW))

		dir := "out"
		dirStyle := exDirOutStyle
		if r.peer.Network.Inbound {
			dir = "in"
			dirStyle = exDirInStyle
		}
		dirCell := dirStyle.Render(padTrunc(dir, exDirColW))

		addr := r.peer.Network.RemoteAddress
		if addr == "" {
			addr = "—"
		}
		addrCell := exValueStyle.Render(padTrunc(addr, 21))

		cursor := "  "
		if i == s.cursor {
			cursor = exCursorStyle.Render("▸ ")
			cursorLine = lineNo
		}
		row := cursor + nameCell + " " + idCell + " " + versionCell + " " + dirCell + " " + addrCell
		if i == s.cursor {
			row = exSelectedBgStyle.Render(row)
		}
		b.WriteString("  " + row + "\n")
		lineNo++
	}
	return b.String(), cursorLine
}

// shrinkID compresses the long execution-layer node ID with a middle
// ellipsis so prefix and suffix both stay visible. With width <= 4 it
// falls back to a hard truncation.
func shrinkID(id string, w int) string {
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
