package app

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
	"op-ctl/internal/tui/theme"
)

// discoveryConsensusScreen renders the result of opp2p_discoveryTable
// for one consensus-layer backend: a cursor-selectable list of ENR
// entries with namespace name resolution. When the node has discovery
// turned off (op-node returns RPCError "discovery disabled") we
// replace the list with a tailored hint instead of a raw error
// string.
type discoveryConsensusScreen struct {
	backend config.Backend
	enrs    []string
	err     error
	rows    []discoveryRow

	cursor int
	width  int
	height int
	offset int
}

type discoveryRow struct {
	enr  string
	name string // resolved namespace name; "" if unknown
}

func newDiscoveryConsensusScreen(b config.Backend, enrs []string, err error, idx *namespace.Index) discoveryConsensusScreen {
	rows := make([]discoveryRow, 0, len(enrs))
	for _, e := range enrs {
		rows = append(rows, discoveryRow{enr: e, name: idx.Lookup(e)})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		an, bn := rows[i].name, rows[j].name
		if (an != "") != (bn != "") {
			return an != ""
		}
		if an != bn {
			return an < bn
		}
		return rows[i].enr < rows[j].enr
	})
	return discoveryConsensusScreen{
		backend: b,
		enrs:    enrs,
		err:     err,
		rows:    rows,
	}
}

func (s discoveryConsensusScreen) Init() tea.Cmd { return nil }

func (s discoveryConsensusScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		case "enter":
			if len(s.rows) == 0 {
				return s, nil
			}
			r := s.rows[s.cursor]
			return s, func() tea.Msg {
				return discoveryDetailMsg{backend: s.backend, name: r.name, enr: r.enr}
			}
		case "down", "j":
			if len(s.rows) > 0 {
				s.cursor++
				if s.cursor >= len(s.rows) {
					s.cursor = len(s.rows) - 1
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
			if len(s.rows) > 0 {
				s.cursor = len(s.rows) - 1
			}
		case "pgdown", "ctrl+d", " ":
			s.cursor += halfPage(s.height)
			if s.cursor >= len(s.rows) {
				s.cursor = len(s.rows) - 1
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

const (
	dscIdxColW  = 5
	dscNameColW = 14
)

func (s discoveryConsensusScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	header := s.renderHeader()
	body, cursorLine := s.renderBody()
	footer := theme.Footer(theme.KeyNav, theme.KeyOpenDetail, theme.KeyBack)

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

func (s discoveryConsensusScreen) renderHeader() string {
	matched := 0
	for _, r := range s.rows {
		if r.name != "" {
			matched++
		}
	}

	var b strings.Builder
	b.WriteString(theme.Title.Render(s.backend.Name) + "  " +
		theme.Subtitle.Render(s.backend.ConsensusRPCURL) + "\n")

	totalCell := theme.Value.Render(fmt.Sprintf("%d", len(s.rows)))
	if s.err != nil {
		if opnode.IsDiscoveryDisabled(s.err) {
			totalCell = theme.WarnBadge.Render(" discovery disabled ")
		} else {
			totalCell = theme.ErrText.Render("✕ " + s.err.Error())
		}
	}
	b.WriteString("  " + theme.Label.Render(padRight("total entries", 16)) + "  " + totalCell + "\n")

	if s.err == nil {
		matchedCell := theme.Value.Render(fmt.Sprintf("%d", matched))
		if matched > 0 {
			matchedCell = theme.OKBadge.Render(fmt.Sprintf(" %d ", matched)) + " " +
				theme.Mute.Render("of "+fmt.Sprintf("%d", len(s.rows))+" matched namespace")
		}
		b.WriteString("  " + theme.Label.Render(padRight("matched", 16)) + "  " + matchedCell + "\n")
	}
	return b.String()
}

func (s discoveryConsensusScreen) renderBody() (string, int) {
	if s.err != nil {
		return s.renderError(), -1
	}
	if len(s.rows) == 0 {
		return "\n  " + theme.Mute.Render("(discovery table is empty)"), -1
	}
	return s.renderRows()
}

func (s discoveryConsensusScreen) renderError() string {
	var b strings.Builder
	b.WriteString("\n")
	if opnode.IsDiscoveryDisabled(s.err) {
		b.WriteString("  " + theme.ErrTitle.Render("discovery disabled") + "\n\n")
		b.WriteString("  " + theme.Mute.Render(s.err.Error()) + "\n\n")
		b.WriteString("  " + theme.Value.Render(
			"This op-node is running with discovery turned off, so") + "\n")
		b.WriteString("  " + theme.Value.Render(
			"opp2p_discoveryTable can't return any ENRs.") + "\n\n")
		b.WriteString("  " + theme.Label.Render("To re-enable on op-node:") + "\n")
		b.WriteString("  " + theme.Value.Render(
			"  drop --p2p.no-discovery (default behavior is on)") + "\n")
		b.WriteString("  " + theme.Value.Render(
			"  ensure --p2p.priv.path / --p2p.priv.raw is set") + "\n")
		b.WriteString("  " + theme.Value.Render(
			"  ensure UDP port (default 9003) is reachable") + "\n")
	} else {
		b.WriteString("  " + theme.ErrTitle.Render("opp2p_discoveryTable failed") + "\n\n")
		b.WriteString("  " + theme.ErrText.Render(s.err.Error()) + "\n")
	}
	return b.String()
}

func (s discoveryConsensusScreen) renderRows() (string, int) {
	enrW := s.width - 4 /* gutter */ - 2 /* cursor */ - dscIdxColW - 1 - dscNameColW - 1 - 2
	if enrW < 24 {
		enrW = 24
	}

	var b strings.Builder
	b.WriteString("\n")
	cursorLine := -1
	lineNo := 1
	for i, r := range s.rows {
		idxCell := theme.WarnText.Render(padRight(fmt.Sprintf("#%d", i+1), dscIdxColW))
		var nameCell string
		if r.name != "" {
			nameCell = theme.Name.Render(padTrunc(r.name, dscNameColW))
		} else {
			nameCell = theme.Mute.Render(padTrunc("·", dscNameColW))
		}
		enrCell := theme.Value.Render(padTrunc(shrinkENR(r.enr, enrW), enrW))

		cursor := "  "
		if i == s.cursor {
			cursor = theme.Cursor.Render("▸ ")
			cursorLine = lineNo
		}
		row := cursor + idxCell + " " + nameCell + " " + enrCell
		if i == s.cursor {
			row = theme.SelectedRow.Render(row)
		}
		b.WriteString("  " + row + "\n")
		lineNo++
	}
	return b.String(), cursorLine
}

// shrinkENR keeps the prefix (which signals the discv5 record kind:
// `enr:-Le4Q...`, `-Ku4Q...`, etc.) and the suffix (which contains
// the ports/UDP fragment) while ellipsis-ing the middle. Falls back
// to a hard prefix truncation when the budget is too small.
func shrinkENR(enr string, w int) string {
	if len(enr) <= w {
		return enr
	}
	if w < 8 {
		return enr[:w]
	}
	head := w*2/3 - 1
	tail := w - 1 - head
	if head < 4 {
		head = 4
	}
	if tail < 4 {
		tail = 4
	}
	if head+tail+1 > w {
		head = w - tail - 1
	}
	return enr[:head] + "…" + enr[len(enr)-tail:]
}
