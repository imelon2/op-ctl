package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/opnode"
	"op-ctl/internal/tui/theme"
)

// peerDetailScreen renders one PeerEntry from opp2p_peers in full —
// every field the JSON-RPC dump returns, including the nested scores
// blob (pretty-printed). It's pushed when the operator presses enter
// on a row in the peers list and is popped on q/esc/ctrl+c.
//
// Banned peers don't carry a PeerEntry (only an ID), so the screen
// renders a degraded view with just the ID + a "(banned)" marker.
type peerDetailScreen struct {
	backend config.Backend
	peerID  string
	name    string
	entry   *opnode.PeerEntry
	banned  bool

	body []string

	width  int
	height int
	offset int
}

func newPeerDetailScreen(backend config.Backend, peerID, name string, entry *opnode.PeerEntry, banned bool) peerDetailScreen {
	s := peerDetailScreen{
		backend: backend,
		peerID:  peerID,
		name:    name,
		entry:   entry,
		banned:  banned,
	}
	s.body = strings.Split(s.renderBody(), "\n")
	return s
}

func (s peerDetailScreen) Init() tea.Cmd { return nil }

func (s peerDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		case "j", "down":
			s.offset++
		case "k", "up":
			s.offset--
		case "g", "home":
			s.offset = 0
		case "G", "end":
			s.offset = 1 << 30
		case "pgdown", "ctrl+d", " ":
			s.offset += halfPage(s.height)
		case "pgup", "ctrl+u", "b":
			s.offset -= halfPage(s.height)
		}
	}
	return s, nil
}

func (s peerDetailScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return strings.Join(s.body, "\n")
	}
	header := s.renderHeader()
	footer := theme.Footer(theme.KeyScroll, theme.KeyTopBottom, theme.KeyBack)

	headerLines := strings.Split(header, "\n")
	avail := s.height - len(headerLines) - 1
	if avail < 1 {
		avail = 1
	}
	maxOffset := len(s.body) - avail
	if maxOffset < 0 {
		maxOffset = 0
	}
	off := s.offset
	if off > maxOffset {
		off = maxOffset
	}
	if off < 0 {
		off = 0
	}
	end := off + avail
	if end > len(s.body) {
		end = len(s.body)
	}
	visible := s.body[off:end]
	for len(visible) < avail {
		visible = append(visible, "")
	}
	return header + "\n" + strings.Join(visible, "\n") + "\n" + footer
}

func (s peerDetailScreen) renderHeader() string {
	title := theme.Title.Render("peer detail")
	var stateBadge string
	switch {
	case s.banned:
		stateBadge = theme.ErrBadge.Render(" banned ")
	case s.entry != nil && s.entry.Connectedness == 1:
		stateBadge = theme.OKBadge.Render(" connected ")
	case s.entry != nil:
		stateBadge = theme.WarnBadge.Render(" " + connectednessLabelDetail(s.entry.Connectedness) + " ")
	}
	src := theme.Subtitle.Render("from " + s.backend.Name + " · " + s.backend.ConsensusRPCURL)
	return title + "  " + stateBadge + "\n" + src
}

// renderBody assembles the scrollable content as a multi-line string.
// Layout: identification block, namespace name, state block, addresses
// list, protocols list, scores JSON. Empty/missing fields are
// rendered as "(empty)" so absence is visible rather than implicit.
func (s peerDetailScreen) renderBody() string {
	var b strings.Builder

	b.WriteString("\n")
	if s.name != "" {
		b.WriteString(field("namespace name", theme.Name.Render(s.name)))
	} else {
		b.WriteString(field("namespace name", theme.Mute.Render("(unknown)")))
	}
	if s.entry == nil {
		b.WriteString(field("peerID", theme.Value.Render(s.peerID)))
		b.WriteString("\n")
		b.WriteString(theme.Mute.Render("  banned peer — opp2p_peers reports only the ID,") + "\n")
		b.WriteString(theme.Mute.Render("  no further attributes available.") + "\n")
		return b.String()
	}

	e := s.entry
	b.WriteString(field("peerID", theme.Value.Render(e.PeerID)))
	b.WriteString(field("nodeID", theme.Value.Render(e.NodeID)))
	b.WriteString(field("userAgent", orMute(e.UserAgent)))
	b.WriteString(field("protocolVersion", orMute(e.ProtocolVersion)))
	b.WriteString(field("ENR", orMute(e.ENR)))

	b.WriteString("\n")
	b.WriteString(field("connectedness", theme.Value.Render(connectednessLabelDetail(e.Connectedness))))
	b.WriteString(field("direction", theme.Value.Render(directionLabelDetail(e.Direction))))
	b.WriteString(field("protected", theme.Value.Render(fmt.Sprintf("%t", e.Protected))))
	b.WriteString(field("chainID", theme.Value.Render(fmt.Sprintf("%d", e.ChainID))))
	b.WriteString(field("latency", theme.Value.Render(formatLatency(e.Latency))))
	b.WriteString(field("gossipBlocks", theme.Value.Render(fmt.Sprintf("%t", e.GossipBlocks))))

	b.WriteString("\n")
	b.WriteString(theme.Section.Render(fmt.Sprintf("addresses (%d)", len(e.Addresses))) + "\n")
	if len(e.Addresses) == 0 {
		b.WriteString("  " + theme.Mute.Render("(none)") + "\n")
	} else {
		for _, a := range e.Addresses {
			b.WriteString("  " + theme.Value.Render(a) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(theme.Section.Render(fmt.Sprintf("protocols (%d)", len(e.Protocols))) + "\n")
	if len(e.Protocols) == 0 {
		b.WriteString("  " + theme.Mute.Render("(none)") + "\n")
	} else {
		for _, p := range e.Protocols {
			b.WriteString("  " + theme.Value.Render(p) + "\n")
		}
	}

	if len(e.Scores) > 0 && string(e.Scores) != "null" {
		b.WriteString("\n")
		b.WriteString(theme.Section.Render("scores") + "\n")
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, e.Scores, "  ", "  "); err == nil {
			for _, line := range strings.Split(pretty.String(), "\n") {
				b.WriteString("  " + theme.Value.Render(line) + "\n")
			}
		} else {
			b.WriteString("  " + theme.Mute.Render("(unparseable: "+err.Error()+")") + "\n")
		}
	}

	return b.String()
}

func field(key, val string) string {
	return "  " + theme.Label.Render(padRight(key, 16)) + "  " + val + "\n"
}

func orMute(s string) string {
	if s == "" {
		return theme.Mute.Render("(empty)")
	}
	return theme.Value.Render(s)
}

func formatLatency(ns uint64) string {
	d := time.Duration(ns)
	switch {
	case d == 0:
		return "0"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", ns)
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return d.String()
	}
}

// connectednessLabelDetail / directionLabelDetail mirror the labels in
// internal/tui/peers but live here too so peer_detail_screen has no
// import-cycle on that package's private helpers.
func connectednessLabelDetail(c int) string {
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

func directionLabelDetail(d int) string {
	switch d {
	case 1:
		return "Inbound"
	case 2:
		return "Outbound"
	default:
		return "Unknown"
	}
}
