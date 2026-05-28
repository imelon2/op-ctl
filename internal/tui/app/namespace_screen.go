package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/theme"
)

// namespaceScreen drives the live snapshot flow: it fans out one
// consensus probe (opp2p_self) and one execution probe (admin_nodeInfo)
// per backend, marks each row as the results stream in, writes the
// per-backend JSON file as soon as both probes for that backend
// settle, and finally shows a structured per-backend detail view.
//
// The same screen handles both phases (in-flight + finished) — it just
// flips its own View based on allDone(). Keeping it on one screen
// means q always pops back to the root menu, no matter when it's
// pressed, and there's no flicker between "loading" and "result"
// states.
type namespaceScreen struct {
	rows    []nsRow
	dir     string
	timeout time.Duration

	width  int
	height int
	offset int

	spinFrame int
}

type probeState int

const (
	probePending probeState = iota
	probeRunning
	probeOK
	probeFailed
)

type writeState int

const (
	writeIdle writeState = iota
	writeRunning
	writeDone
	writeFailed
)

type nsRow struct {
	backend config.Backend

	consensus      probeState
	consensusInfo  *opnode.PeerInfo
	consensusErr   error
	consensusLatMs uint64

	execution      probeState
	executionInfo  *elnode.NodeInfo
	executionErr   error
	executionLatMs uint64

	write     writeState
	writePath string
	writeErr  error
}

// nsConsensusMsg / nsExecutionMsg / nsWriteMsg carry per-backend probe
// or write completions. backendIdx is a positional index into rows so
// the handler doesn't need to walk by name (and so name collisions
// can't misroute results).
type nsConsensusMsg struct {
	backendIdx int
	info       *opnode.PeerInfo
	latencyMS  uint64
	err        error
}

type nsExecutionMsg struct {
	backendIdx int
	info       *elnode.NodeInfo
	latencyMS  uint64
	err        error
}

type nsWriteMsg struct {
	backendIdx int
	path       string
	err        error
}

// nsTickMsg drives the in-progress spinner. Ticks stop firing once
// every backend has settled so a finished screen doesn't waste cpu.
type nsTickMsg struct{}

func newNamespaceScreen(bs []config.Backend, resolver *sshtunnel.Resolver, dir string, timeout time.Duration, retries int) (namespaceScreen, tea.Cmd) {
	rows := make([]nsRow, len(bs))
	for i, b := range bs {
		rows[i] = nsRow{backend: b, consensus: probeRunning, execution: probeRunning}
	}
	s := namespaceScreen{rows: rows, dir: dir, timeout: timeout}

	cmds := make([]tea.Cmd, 0, 1+2*len(bs))
	cmds = append(cmds, nsTick())
	for i, b := range bs {
		cmds = append(cmds, fetchConsensus(i, resolver, b, timeout, retries))
		cmds = append(cmds, fetchExecution(i, resolver, b, timeout, retries))
	}
	return s, tea.Batch(cmds...)
}

func (s namespaceScreen) Init() tea.Cmd { return nil }

func (s namespaceScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
		return s, nil

	case nsConsensusMsg:
		if m.err != nil {
			s.rows[m.backendIdx].consensus = probeFailed
			s.rows[m.backendIdx].consensusErr = m.err
		} else {
			s.rows[m.backendIdx].consensus = probeOK
			s.rows[m.backendIdx].consensusInfo = m.info
		}
		s.rows[m.backendIdx].consensusLatMs = m.latencyMS
		return s, s.maybeWrite(m.backendIdx)

	case nsExecutionMsg:
		if m.err != nil {
			s.rows[m.backendIdx].execution = probeFailed
			s.rows[m.backendIdx].executionErr = m.err
		} else {
			s.rows[m.backendIdx].execution = probeOK
			s.rows[m.backendIdx].executionInfo = m.info
		}
		s.rows[m.backendIdx].executionLatMs = m.latencyMS
		return s, s.maybeWrite(m.backendIdx)

	case nsWriteMsg:
		if m.err != nil {
			s.rows[m.backendIdx].write = writeFailed
			s.rows[m.backendIdx].writeErr = m.err
		} else {
			s.rows[m.backendIdx].write = writeDone
			s.rows[m.backendIdx].writePath = m.path
		}
		return s, nil

	case nsTickMsg:
		s.spinFrame++
		if !s.allDone() {
			return s, nsTick()
		}
		return s, nil

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

// maybeWrite returns a tea.Cmd that writes the namespace file for row
// idx if both probes there have settled, otherwise nil. Marks the row
// as "writing" optimistically so the spinner can flip from probe to
// write before the disk responds.
func (s namespaceScreen) maybeWrite(idx int) tea.Cmd {
	r := s.rows[idx]
	if r.consensus == probeRunning || r.consensus == probePending {
		return nil
	}
	if r.execution == probeRunning || r.execution == probePending {
		return nil
	}
	if r.write != writeIdle {
		return nil
	}
	s.rows[idx].write = writeRunning

	dir := s.dir
	return func() tea.Msg {
		e := namespace.Entry{Name: r.backend.Name}
		if r.consensusInfo != nil {
			e.Consensus.PeerID = r.consensusInfo.PeerID
			e.Consensus.NodeID = r.consensusInfo.NodeID
			e.Consensus.ENR = r.consensusInfo.ENR
		}
		if r.executionInfo != nil {
			e.Execution.NodeID = r.executionInfo.ID
			e.Execution.Enode = r.executionInfo.Enode
			e.Execution.ENR = r.executionInfo.ENR
		}
		path, err := namespace.Write(dir, e)
		return nsWriteMsg{backendIdx: idx, path: path, err: err}
	}
}

func (s namespaceScreen) allDone() bool {
	for _, r := range s.rows {
		if r.consensus == probeRunning || r.consensus == probePending {
			return false
		}
		if r.execution == probeRunning || r.execution == probePending {
			return false
		}
		if r.write == writeIdle || r.write == writeRunning {
			return false
		}
	}
	return true
}

func (s namespaceScreen) View() string {
	if s.allDone() {
		return s.viewResult()
	}
	return s.viewProgress()
}

// ---------- progress phase ----------

func (s namespaceScreen) viewProgress() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("namespace") + "  " +
		theme.Subtitle.Render(fmt.Sprintf("snapshotting %d backend(s)...", len(s.rows))) + "\n\n")

	nameW := 0
	for _, r := range s.rows {
		if w := lipgloss.Width(r.backend.Name); w > nameW {
			nameW = w
		}
	}

	const probeColW = 12 // ✓/⚠/✕ + " 999ms" comfortably; pads short cells
	for _, r := range s.rows {
		name := theme.Name.Render(r.backend.Name) +
			strings.Repeat(" ", nameW-lipgloss.Width(r.backend.Name))
		consCell := padTrunc(s.probeCell(r.consensus, r.consensusLatMs), probeColW)
		execCell := padTrunc(s.probeCell(r.execution, r.executionLatMs), probeColW)
		writeCell := s.writeCell(r)

		fmt.Fprintf(&b, "  %s   %s %s  %s %s  %s %s\n",
			name,
			theme.Label.Render("consensus"), consCell,
			theme.Label.Render("execution"), execCell,
			theme.Label.Render("write"), writeCell,
		)
	}

	b.WriteString("\n")
	b.WriteString(theme.Help.Render("running... q quits and aborts pending writes"))
	return b.String()
}

func (s namespaceScreen) probeCell(p probeState, latencyMS uint64) string {
	switch p {
	case probePending:
		return theme.Label.Render("·")
	case probeRunning:
		return theme.WarnText.Bold(true).Render(theme.Spinner(s.spinFrame))
	case probeOK:
		return theme.OKText.Bold(true).Render("✓") + " " + theme.Label.Render(fmt.Sprintf("%dms", latencyMS))
	case probeFailed:
		return theme.ErrText.Bold(true).Render("✕")
	}
	return "?"
}

func (s namespaceScreen) writeCell(r nsRow) string {
	switch r.write {
	case writeIdle:
		return theme.Label.Render("·")
	case writeRunning:
		return theme.WarnText.Bold(true).Render(theme.Spinner(s.spinFrame))
	case writeDone:
		return theme.OKText.Bold(true).Render("✓")
	case writeFailed:
		return theme.ErrText.Bold(true).Render("✕")
	}
	return "?"
}

// ---------- result phase ----------

func (s namespaceScreen) viewResult() string {
	okCount, partialCount, emptyCount := 0, 0, 0
	for _, r := range s.rows {
		switch {
		case r.consensus == probeOK && r.execution == probeOK:
			okCount++
		case r.consensus == probeOK || r.execution == probeOK:
			partialCount++
		default:
			emptyCount++
		}
	}

	var head strings.Builder
	head.WriteString(theme.Title.Render("namespace") + "  " +
		theme.Subtitle.Render("snapshot complete") + "\n")
	head.WriteString("  ")
	head.WriteString(theme.OKBadge.Render(fmt.Sprintf(" %d ok ", okCount)))
	head.WriteString("  ")
	head.WriteString(theme.WarnBadge.Render(fmt.Sprintf(" %d partial ", partialCount)))
	head.WriteString("  ")
	head.WriteString(theme.ErrBadge.Render(fmt.Sprintf(" %d empty ", emptyCount)))
	head.WriteString("\n")

	var body strings.Builder
	for i, r := range s.rows {
		if i > 0 {
			body.WriteString("\n")
		}
		body.WriteString(s.renderRowDetail(r))
	}

	footer := theme.Footer(theme.KeyScroll, theme.KeyTopBottom, theme.KeyBack)

	if s.width == 0 || s.height == 0 {
		return head.String() + "\n" + body.String() + "\n\n" + footer
	}

	headLines := strings.Split(strings.TrimRight(head.String(), "\n"), "\n")
	bodyLines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
	avail := s.height - len(headLines) - 2 // blank + footer
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
	return strings.Join(headLines, "\n") + "\n\n" +
		strings.Join(visible, "\n") + "\n" +
		footer
}

// renderRowDetail formats one backend's full result. Backend name +
// status badge on the first line; file path on the second; then a
// labeled key/value block for each of consensus/execution. Failed
// probes show their error message inline instead of empty rows so the
// operator immediately sees *why* a field is missing.
func (s namespaceScreen) renderRowDetail(r nsRow) string {
	var b strings.Builder

	statusBadge, statusText := rowStatus(r)
	b.WriteString(statusBadge + " ")
	b.WriteString(theme.Name.Render(r.backend.Name))
	if statusText != "" {
		b.WriteString("  " + theme.Subtitle.Render(statusText))
	}
	b.WriteString("\n")
	if r.write == writeDone && r.writePath != "" {
		b.WriteString("  " + theme.Label.Render("file") + "  " + theme.Value.Render(r.writePath) + "\n")
	} else if r.write == writeFailed && r.writeErr != nil {
		b.WriteString("  " + theme.Label.Render("file") + "  " +
			theme.ErrText.Render("write failed: "+r.writeErr.Error()) + "\n")
	}

	b.WriteString(s.detailBlock("consensus", r.consensus, r.consensusErr,
		[]kv{
			{"peer_id", consensusPeerID(r)},
			{"node_id", consensusNodeID(r)},
			{"enr", consensusENR(r)},
		}))
	b.WriteString(s.detailBlock("execution", r.execution, r.executionErr,
		[]kv{
			{"node_id", executionNodeID(r)},
			{"enode", executionEnode(r)},
			{"enr", executionENR(r)},
		}))
	return b.String()
}

type kv struct{ key, val string }

func (s namespaceScreen) detailBlock(label string, st probeState, err error, fields []kv) string {
	switch st {
	case probeFailed:
		msg := "failed"
		if err != nil {
			msg = "failed: " + err.Error()
		}
		return "  " + theme.Label.Render(label) + "  " + theme.ErrText.Bold(true).Render("✕ ") +
			theme.ErrText.Render(msg) + "\n"
	case probeOK:
		var b strings.Builder
		b.WriteString("  " + theme.Label.Render(label) + "\n")
		for _, f := range fields {
			val := f.val
			if val == "" {
				val = theme.Label.Render("(empty)")
			} else {
				val = theme.Value.Render(val)
			}
			b.WriteString("    " + theme.Label.Render(padRight(f.key, 8)) + "  " + val + "\n")
		}
		return b.String()
	default:
		return "  " + theme.Label.Render(label) + "  " + theme.Label.Render("(not run)") + "\n"
	}
}

func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// padTrunc pads s with trailing spaces to displayed width n. Unlike
// padRight, it relies on lipgloss.Width so ANSI color codes don't
// inflate the count and break column alignment.
func padTrunc(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// rowStatus picks a colored bullet + textual descriptor for the
// status header line. All bullets are single-rune colored glyphs so
// every card lines up on the same left margin regardless of status.
func rowStatus(r nsRow) (badge, text string) {
	switch {
	case r.write == writeFailed:
		return theme.ErrText.Bold(true).Render("✕"), "write failed"
	case r.consensus == probeOK && r.execution == probeOK:
		return theme.OKText.Bold(true).Render("✓"), ""
	case r.consensus == probeOK || r.execution == probeOK:
		side := "consensus"
		if r.consensus == probeOK {
			side = "execution"
		}
		return theme.WarnText.Bold(true).Render("⚠"), "partial: " + side + " failed"
	default:
		return theme.ErrText.Bold(true).Render("✕"), "empty: both probes failed"
	}
}

func consensusPeerID(r nsRow) string {
	if r.consensusInfo != nil {
		return r.consensusInfo.PeerID
	}
	return ""
}
func consensusNodeID(r nsRow) string {
	if r.consensusInfo != nil {
		return r.consensusInfo.NodeID
	}
	return ""
}
func consensusENR(r nsRow) string {
	if r.consensusInfo != nil {
		return r.consensusInfo.ENR
	}
	return ""
}
func executionNodeID(r nsRow) string {
	if r.executionInfo != nil {
		return r.executionInfo.ID
	}
	return ""
}
func executionEnode(r nsRow) string {
	if r.executionInfo != nil {
		return r.executionInfo.Enode
	}
	return ""
}
func executionENR(r nsRow) string {
	if r.executionInfo != nil {
		return r.executionInfo.ENR
	}
	return ""
}

// ---------- async work ----------

// nsRetryBackoff mirrors probe.retryBackoff. Hardcoded here (rather
// than imported) because probe.go's var is package-private and the TUI
// path is independent — the operationally meaningful value is "the
// same 500ms cadence everywhere", not "imported from one place".
const nsRetryBackoff = 500 * time.Millisecond

func fetchConsensus(idx int, resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration, retries int) tea.Cmd {
	return func() tea.Msg {
		var (
			lastErr error
			info    *opnode.PeerInfo
			lat     time.Duration
		)
		for attempt := 0; attempt <= retries; attempt++ {
			if attempt > 0 {
				time.Sleep(nsRetryBackoff)
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			hc, herr := resolver.HTTPClient(ctx, b.SSHJump)
			if herr != nil {
				cancel()
				lastErr = herr
				continue
			}
			info, lat, lastErr = opnode.Self(ctx, hc, b.ConsensusRPCURL)
			cancel()
			if lastErr == nil {
				return nsConsensusMsg{backendIdx: idx, info: info, latencyMS: uint64(lat / time.Millisecond)}
			}
		}
		return nsConsensusMsg{backendIdx: idx, info: info, latencyMS: uint64(lat / time.Millisecond), err: lastErr}
	}
}

func fetchExecution(idx int, resolver *sshtunnel.Resolver, b config.Backend, timeout time.Duration, retries int) tea.Cmd {
	return func() tea.Msg {
		var (
			lastErr error
			info    *elnode.NodeInfo
			lat     time.Duration
		)
		for attempt := 0; attempt <= retries; attempt++ {
			if attempt > 0 {
				time.Sleep(nsRetryBackoff)
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			hc, herr := resolver.HTTPClient(ctx, b.SSHJump)
			if herr != nil {
				cancel()
				lastErr = herr
				continue
			}
			info, lat, lastErr = elnode.Self(ctx, hc, b.ExecutionRPCURL)
			cancel()
			if lastErr == nil {
				return nsExecutionMsg{backendIdx: idx, info: info, latencyMS: uint64(lat / time.Millisecond)}
			}
		}
		return nsExecutionMsg{backendIdx: idx, info: info, latencyMS: uint64(lat / time.Millisecond), err: lastErr}
	}
}

func nsTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return nsTickMsg{} })
}
