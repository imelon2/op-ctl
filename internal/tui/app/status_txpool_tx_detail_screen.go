package app

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
)

// statusTxPoolTxDetailScreen is the Stage-2 drill-down: the full
// scrollable detail of one transaction selected from the detail
// list. The tx pointer comes from the parent detail screen's cache
// (filled by txpool_content) — no per-click RPC. The nil-tx branch
// is kept as a defensive guard: future callers that hand off a nil
// pointer still render the "tx no longer in pool" message rather
// than panicking.
type statusTxPoolTxDetailScreen struct {
	backend config.Backend
	tx      *elnode.TxPoolTx

	body []string

	width  int
	height int
	offset int
}

func newStatusTxPoolTxDetailScreen(backend config.Backend, tx *elnode.TxPoolTx) statusTxPoolTxDetailScreen {
	s := statusTxPoolTxDetailScreen{
		backend: backend,
		tx:      tx,
	}
	s.body = strings.Split(s.renderBody(), "\n")
	return s
}

func (s statusTxPoolTxDetailScreen) Init() tea.Cmd { return nil }

func (s statusTxPoolTxDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

// ---------- styles ----------

var (
	txfTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	txfSubtitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	txfLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	txfValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	txfSectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	txfMuteStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	txfHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
)

func (s statusTxPoolTxDetailScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return strings.Join(s.body, "\n")
	}
	header := s.renderHeader()
	footer := txfHelpStyle.Render("j/k ↑/↓ scroll · g/G top/bottom · q back")

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

func (s statusTxPoolTxDetailScreen) renderHeader() string {
	var b strings.Builder
	b.WriteString(txfTitleStyle.Render("tx detail · "+s.backend.Name) + "  ")
	b.WriteString(txfSubtitleStyle.Render(s.backend.ExecutionRPCURL))
	if s.tx != nil {
		b.WriteString("\n  ")
		b.WriteString(txfSubtitleStyle.Render(fmt.Sprintf("sender %s · nonce %d", s.tx.From, s.tx.Nonce)))
	}
	return b.String()
}

// renderBody returns the scrollable content for the screen as one
// newline-joined string. Caller splits it once into s.body so the
// View() scroll math stays cheap.
func (s statusTxPoolTxDetailScreen) renderBody() string {
	var b strings.Builder
	b.WriteString("\n")

	if s.tx == nil {
		b.WriteString("  " + txfMuteStyle.Render(
			"tx no longer in pool — it may have been mined between list-fetch and detail-fetch.") + "\n")
		b.WriteString("  " + txfMuteStyle.Render(
			"press q to return to the list and r to refresh.") + "\n")
		return b.String()
	}

	tx := s.tx
	b.WriteString(field("hash", txfValueStyle.Render(tx.Hash)))
	b.WriteString(field("from", txfValueStyle.Render(tx.From)))
	b.WriteString(field("to", txfValueStyle.Render(tx.To)))
	b.WriteString(field("nonce", txfValueStyle.Render(fmt.Sprintf("%d", tx.Nonce))))
	b.WriteString(field("gas", txfValueStyle.Render(fmt.Sprintf("%d", tx.Gas))))
	b.WriteString(field("value", txfValueStyle.Render(formatValueDecimal(tx.Value))))

	b.WriteString("\n")
	b.WriteString(field("type", txfValueStyle.Render(fmt.Sprintf("%d", tx.Type))))
	b.WriteString(field("chainId", txfValueStyle.Render(fmt.Sprintf("%d", tx.ChainID))))
	b.WriteString(field("gasPrice", txfValueStyle.Render(formatBigOrEmpty(tx.GasPrice)+" wei")))
	b.WriteString(field("maxFeePerGas", txfValueStyle.Render(formatBigOrEmpty(tx.MaxFee)+" wei")))
	b.WriteString(field("maxPriorityFeePerGas", txfValueStyle.Render(formatBigOrEmpty(tx.MaxTip)+" wei")))

	b.WriteString("\n")
	b.WriteString(txfSectionStyle.Render(fmt.Sprintf("input (%d bytes)", len(tx.Input))) + "\n")
	if len(tx.Input) == 0 {
		b.WriteString("  " + txfMuteStyle.Render("(empty)") + "\n")
	} else {
		h := "0x" + hex.EncodeToString(tx.Input)
		if len(h) > 64 {
			b.WriteString("  " + txfValueStyle.Render(h[:64]) + "\n")
			b.WriteString("  " + txfMuteStyle.Render(
				fmt.Sprintf("... (%d more bytes)", len(tx.Input)-32)) + "\n")
		} else {
			b.WriteString("  " + txfValueStyle.Render(h) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(txfSectionStyle.Render("signature") + "\n")
	b.WriteString(field("r", txfValueStyle.Render(txfOrEmpty(tx.R))))
	b.WriteString(field("s", txfValueStyle.Render(txfOrEmpty(tx.S))))
	b.WriteString(field("v", txfValueStyle.Render(txfOrEmpty(tx.V))))
	b.WriteString(field("yParity", txfValueStyle.Render(txfOrEmpty(tx.YParity))))

	if len(tx.AccessList) > 0 && string(tx.AccessList) != "null" && string(tx.AccessList) != "[]" {
		b.WriteString("\n")
		b.WriteString(txfSectionStyle.Render("accessList") + "\n")
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, tx.AccessList, "  ", "  "); err == nil {
			for _, line := range strings.Split(pretty.String(), "\n") {
				b.WriteString("  " + txfValueStyle.Render(line) + "\n")
			}
		} else {
			b.WriteString("  " + txfMuteStyle.Render("(unparseable: "+err.Error()+")") + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(txfSectionStyle.Render("in-pool flags") + "\n")
	b.WriteString(field("blockHash", maybeStr(tx.BlockHash)))
	b.WriteString(field("blockNumber", maybeStr(tx.BlockNumber)))
	b.WriteString(field("transactionIndex", maybeStr(tx.TxIndex)))
	return b.String()
}

// formatValueDecimal renders a wei value as `<wei> wei (<ether> ETH)`.
// For very small values (<1 ether) we omit the parenthetical so the
// row stays compact.
func formatValueDecimal(v *big.Int) string {
	if v == nil {
		return "(empty)"
	}
	if v.Sign() == 0 {
		return "0 wei"
	}
	wei := v.String() + " wei"
	oneEther := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	if v.Cmp(oneEther) < 0 {
		return wei
	}
	whole := new(big.Int).Quo(v, oneEther)
	frac := new(big.Int).Mod(v, oneEther)
	fracStr := fmt.Sprintf("%018d", frac)
	if len(fracStr) > 6 {
		fracStr = fracStr[:6]
	}
	return fmt.Sprintf("%s (%s.%s ETH)", wei, whole, fracStr)
}

func formatBigOrEmpty(v *big.Int) string {
	if v == nil {
		return "(empty)"
	}
	return v.String()
}

func txfOrEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

func maybeStr(p *string) string {
	if p == nil {
		return txfMuteStyle.Render("(null — in pool)")
	}
	return txfValueStyle.Render(*p)
}
