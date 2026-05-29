package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/etherscan"
	"op-ctl/internal/tui/keymap"
	"op-ctl/internal/tui/theme"
)

// readBatchTxFetchedMsg returns one cached transaction (or nil + err)
// to the detail screen's Init goroutine. nil tx + nil err means the
// row was not found — the detail screen renders "missing" rather
// than erroring.
type readBatchTxFetchedMsg struct {
	tx  *etherscan.Tx
	err error
}

// readBatchDetailScreen renders the full field set for one batch tx,
// matching the spec's 12-field requirement. The input blob is the
// only field that can overflow the viewport (256KB+ on blob batches),
// so it gets word-wrapped + scrollable. The other 11 fields are
// fixed-height label / value pairs.
type readBatchDetailScreen struct {
	store  *batchcache.Store
	txHash string
	tx     *etherscan.Tx
	err    error
	offset int

	width, height int
}

func newReadBatchDetailScreen(store *batchcache.Store, txHash string) readBatchDetailScreen {
	return readBatchDetailScreen{store: store, txHash: txHash}
}

func (s readBatchDetailScreen) Init() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := s.store.Get(ctx, s.txHash)
		return readBatchTxFetchedMsg{tx: tx, err: err}
	}
}

func (s readBatchDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		return s, nil
	case readBatchTxFetchedMsg:
		s.tx = m.tx
		s.err = m.err
		return s, nil
	case tea.KeyMsg:
		switch {
		case keymap.Back.Matches(m):
			return s, func() tea.Msg { return popMsg{} }
		case keymap.Down.Matches(m):
			s.offset++
			return s, nil
		case keymap.Up.Matches(m):
			if s.offset > 0 {
				s.offset--
			}
			return s, nil
		}
	}
	return s, nil
}

func (s readBatchDetailScreen) View() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("read / batch / detail"))
	b.WriteString("  ")
	b.WriteString(theme.Subtitle.Render(s.txHash))
	b.WriteString("\n\n")

	if s.err != nil {
		b.WriteString(theme.ErrLine("ERR: " + s.err.Error()))
		b.WriteString("\n")
		return b.String()
	}
	if s.tx == nil {
		b.WriteString(theme.Label.Render("(loading or not found)"))
		b.WriteString("\n\n")
		b.WriteString(keymap.Footer(keymap.Back))
		return b.String()
	}

	// 12 labeled fields per spec: tx_hash, block, timestamp, from, to,
	// value(wei), gas, gasUsed, gasPrice, methodId, status, input.
	// input_size is shown adjacent to the input header below; the
	// scrollable region itself is the "input" field.
	t := s.tx
	rows := [][2]string{
		{"tx_hash", t.Hash},
		{"block", fmt.Sprintf("%d", t.BlockNumber)},
		{"timestamp", time.Unix(t.TimeStamp, 0).UTC().Format(time.RFC3339)},
		{"from", t.From},
		{"to", t.To},
		{"value (wei)", t.Value},
		{"gas", fmt.Sprintf("%d", t.Gas)},
		{"gas_used", fmt.Sprintf("%d", t.GasUsed)},
		{"gas_price (wei)", t.GasPrice},
		{"method_id", t.MethodID},
		{"status", statusLabel(t.Status)},
	}
	for _, kv := range rows {
		b.WriteString(theme.Label.Render(fmt.Sprintf("%-14s", kv[0])))
		b.WriteString(" ")
		b.WriteString(kv[1])
		b.WriteString("\n")
	}
	// input is the 12th labeled field — a scrollable region rather
	// than a one-liner because blob batches routinely carry 100s of
	// KB. offset rolls a single-line viewport across the input data
	// so the operator can inspect arbitrarily long payloads without
	// the rest of the screen scrolling out of view.
	b.WriteString("\n")
	b.WriteString(theme.Label.Render(fmt.Sprintf("input (%s)", humanBytes(len(t.Input)))))
	b.WriteString("\n")
	b.WriteString(wrapAt(t.Input, max(s.width-2, 40), s.offset, 8))
	b.WriteString("\n\n")
	b.WriteString(keymap.Footer(keymap.Scroll, keymap.Back))
	return b.String()
}

func statusLabel(n int) string {
	if n == 1 {
		return "success"
	}
	return "failed"
}

// wrapAt slices s into width-wide chunks, skips the first `skip`
// chunks (used as a vertical scroll offset), and returns up to
// `lines` of joined output. Keeps the detail screen bounded even on
// 256KB blob payloads where rendering the full string would scroll
// the rest of the screen out of view.
func wrapAt(s string, width, skip, lines int) string {
	if width <= 0 {
		width = 80
	}
	var out []string
	for i := 0; i < len(s); i += width {
		out = append(out, s[i:min(i+width, len(s))])
	}
	if skip >= len(out) {
		skip = max(len(out)-1, 0)
	}
	end := min(skip+lines, len(out))
	return strings.Join(out[skip:end], "\n")
}
