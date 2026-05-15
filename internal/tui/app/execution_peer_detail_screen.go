package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
)

// executionPeerDetailScreen renders one AdminPeer in full — every
// field admin_peers returns, plus a separated `network` block, the
// `caps` list, and a pretty-printed `protocols` JSON. Pushed on enter
// from executionPeersScreen, popped on q.
type executionPeerDetailScreen struct {
	backend config.Backend
	name    string
	peer    elnode.AdminPeer

	body []string

	width  int
	height int
	offset int
}

func newExecutionPeerDetailScreen(b config.Backend, name string, p elnode.AdminPeer) executionPeerDetailScreen {
	s := executionPeerDetailScreen{backend: b, name: name, peer: p}
	s.body = strings.Split(s.renderBody(), "\n")
	return s
}

func (s executionPeerDetailScreen) Init() tea.Cmd { return nil }

func (s executionPeerDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

var (
	epdTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	epdSubtitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	epdLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	epdValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	epdNameStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	epdSectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	epdHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	epdMuteStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	epdBadgeBase = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("0"))
	epdBadgeIn   = epdBadgeBase.Background(lipgloss.Color("12"))
	epdBadgeOut  = epdBadgeBase.Background(lipgloss.Color("13"))
)

func (s executionPeerDetailScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return strings.Join(s.body, "\n")
	}
	header := s.renderHeader()
	footer := epdHelpStyle.Render("j/k ↑/↓ scroll · g/G top/bottom · q back")

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

func (s executionPeerDetailScreen) renderHeader() string {
	title := epdTitleStyle.Render("execution peer detail")
	dirBadge := epdBadgeOut.Render(" outbound ")
	if s.peer.Network.Inbound {
		dirBadge = epdBadgeIn.Render(" inbound ")
	}
	src := epdSubtitleStyle.Render("from " + s.backend.Name + " · " + s.backend.ExecutionRPCURL)
	return title + "  " + dirBadge + "\n" + src
}

func (s executionPeerDetailScreen) renderBody() string {
	var b strings.Builder
	b.WriteString("\n")
	if s.name != "" {
		b.WriteString(detailField("namespace name", epdNameStyle.Render(s.name)))
	} else {
		b.WriteString(detailField("namespace name", epdMuteStyle.Render("(unknown)")))
	}
	b.WriteString(detailField("id", epdValueStyle.Render(s.peer.ID)))
	b.WriteString(detailField("name", epdValueStyle.Render(s.peer.Name)))
	b.WriteString(detailField("enode", epdValueStyle.Render(s.peer.Enode)))

	b.WriteString("\n")
	b.WriteString(epdSectionStyle.Render("network") + "\n")
	b.WriteString(detailField("  localAddress", epdValueStyle.Render(s.peer.Network.LocalAddress)))
	b.WriteString(detailField("  remoteAddress", epdValueStyle.Render(s.peer.Network.RemoteAddress)))
	b.WriteString(detailField("  inbound", epdValueStyle.Render(fmt.Sprintf("%t", s.peer.Network.Inbound))))
	b.WriteString(detailField("  trusted", epdValueStyle.Render(fmt.Sprintf("%t", s.peer.Network.Trusted))))
	b.WriteString(detailField("  static", epdValueStyle.Render(fmt.Sprintf("%t", s.peer.Network.Static))))

	b.WriteString("\n")
	b.WriteString(epdSectionStyle.Render(fmt.Sprintf("caps (%d)", len(s.peer.Caps))) + "\n")
	if len(s.peer.Caps) == 0 {
		b.WriteString("  " + epdMuteStyle.Render("(none)") + "\n")
	} else {
		for _, c := range s.peer.Caps {
			b.WriteString("  " + epdValueStyle.Render(c) + "\n")
		}
	}

	if len(s.peer.Protocols) > 0 {
		b.WriteString("\n")
		b.WriteString(epdSectionStyle.Render(fmt.Sprintf("protocols (%d)", len(s.peer.Protocols))) + "\n")
		// Sort keys for stable rendering — Go map iteration order
		// would otherwise jitter between redraws.
		keys := make([]string, 0, len(s.peer.Protocols))
		for k := range s.peer.Protocols {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("  " + epdLabelStyle.Render(k) + "\n")
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, s.peer.Protocols[k], "  ", "  "); err == nil {
				for _, line := range strings.Split(pretty.String(), "\n") {
					b.WriteString("    " + epdValueStyle.Render(line) + "\n")
				}
			} else {
				b.WriteString("    " + epdMuteStyle.Render("(unparseable: "+err.Error()+")") + "\n")
			}
		}
	}
	return b.String()
}

func detailField(key, val string) string {
	return "  " + epdLabelStyle.Render(padRight(key, 17)) + "  " + val + "\n"
}
