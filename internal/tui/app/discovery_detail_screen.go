package app

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/tui/theme"
)

// discoveryDetailScreen renders one ENR from the discovery table in
// full, with namespace-name resolution prominently surfaced. ENRs are
// opaque structured records (RLP-encoded inside the Base64URL "enr:"
// payload) — fully decoding them would require pulling in
// go-ethereum's enode package, which we've intentionally avoided.
// The detail view shows the literal string + the resolved name so
// the operator can correlate by hand or pipe to `enrtree` / `enr-cli`
// externally.
type discoveryDetailScreen struct {
	backend config.Backend
	name    string
	enr     string

	width  int
	height int
	offset int
}

func newDiscoveryDetailScreen(b config.Backend, name, enr string) discoveryDetailScreen {
	return discoveryDetailScreen{backend: b, name: name, enr: enr}
}

func (s discoveryDetailScreen) Init() tea.Cmd { return nil }

func (s discoveryDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (s discoveryDetailScreen) View() string {
	header := s.renderHeader()
	body := s.renderBody()
	footer := theme.Footer(theme.KeyScroll, theme.KeyTopBottom, theme.KeyBack)

	if s.width == 0 || s.height == 0 {
		return header + "\n" + body + "\n" + footer
	}

	bodyLines := strings.Split(body, "\n")
	headerLines := strings.Split(header, "\n")
	avail := s.height - len(headerLines) - 1
	if avail < 1 {
		avail = 1
	}
	maxOffset := len(bodyLines) - avail
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
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[off:end]
	for len(visible) < avail {
		visible = append(visible, "")
	}
	return header + "\n" + strings.Join(visible, "\n") + "\n" + footer
}

func (s discoveryDetailScreen) renderHeader() string {
	title := theme.Title.Render("discovery entry")
	matchBadge := ""
	if s.name != "" {
		matchBadge = "  " + theme.OKBadge.Render(" "+s.name+" ")
	}
	src := theme.Subtitle.Render("from " + s.backend.Name + " · " + s.backend.ConsensusRPCURL)
	return title + matchBadge + "\n" + src
}

func (s discoveryDetailScreen) renderBody() string {
	var b strings.Builder
	b.WriteString("\n")
	if s.name != "" {
		b.WriteString("  " + theme.Label.Render(padRight("namespace name", 16)) + "  " +
			theme.Name.Render(s.name) + "\n")
	} else {
		b.WriteString("  " + theme.Label.Render(padRight("namespace name", 16)) + "  " +
			theme.Mute.Render("(unknown — no matching consensus.enr in namespace dir)") + "\n")
	}
	b.WriteString("\n")
	b.WriteString("  " + theme.Label.Render("enr") + "\n")

	// ENRs are 200-300 chars and don't have natural break points, so
	// we wrap on a fixed column width tied to the terminal. Trailing
	// blank line keeps the help footer separated.
	wrapW := s.width - 4
	if wrapW < 40 {
		wrapW = 40
	}
	for _, line := range chunk(s.enr, wrapW) {
		b.WriteString("    " + theme.Value.Render(line) + "\n")
	}
	return b.String()
}

// chunk splits s into width-wide pieces. Used to wrap ENRs which
// have no spaces or other natural break points.
func chunk(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var out []string
	for len(s) > width {
		out = append(out, s[:width])
		s = s[width:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}
