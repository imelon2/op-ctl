package app

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"op-ctl/internal/l1"
	"op-ctl/internal/l2"
	"op-ctl/internal/tui/theme"
)

// vaultRefreshInterval is the auto-tick cadence for the FeeVault
// section. 1s is aggressive enough to feel "live" on a busy chain
// without hammering the L2 RPC — each tick issues 1 batched eth_call
// (4 vaults) + 4 eth_getBalance roundtrips. Hard-coded by design: the
// operator wants to see balance drift in real time and a knob would
// invite drift toward unhelpful longer intervals.
const vaultRefreshInterval = 1 * time.Second

// --- messages ---

// readNetworkFeeSysCfgMsg carries the one-shot L1 SystemConfig fetch
// result. SystemConfig parameters change at deploy / governance pace
// — no need to retag with a generation since refresh is manual ('r'
// key) and a stale straggler can only land before the next manual
// refresh fires.
type readNetworkFeeSysCfgMsg struct {
	snap *l1.SystemConfigSnapshot
	err  error
}

// readNetworkFeeVaultsMsg carries one FeeVault sample. gen tags the
// sample with the tick generation that issued it; the screen drops
// any sample whose gen != s.vaultGen so a slow response from tick N
// can't overwrite tick N+1's already-displayed data.
type readNetworkFeeVaultsMsg struct {
	gen   uint64
	snaps []l2.VaultSnapshot
	lat   time.Duration
	err   error
}

// readNetworkFeeVaultTickMsg is the scheduled wake-up. The Update
// branch accepts only the next-expected generation (s.vaultGen+1),
// bumps s.vaultGen, fires a fetch tagged with the new gen, and
// schedules the following tick.
type readNetworkFeeVaultTickMsg struct{ gen uint64 }

// readNetworkFeeGPOMsg carries the one-shot L2 GasPriceOracle fetch
// result. The oracle's scaling parameters, pinned constants, and
// version are static (governance / deploy paced), so they are fetched
// once on entry and re-fetched only on manual 'r' refresh — the live
// L1 fee inputs are handled separately by readNetworkFeeL1FeeMsg.
type readNetworkFeeGPOMsg struct {
	snap *l2.GasPriceOracleSnapshot
	err  error
}

// readNetworkFeeL1FeeMsg carries one live L1 data-fee sample
// (l1BaseFee + blobBaseFee from the GasPriceOracle). It rides the same
// generation-tagged tick loop as the vault and gas samples so the
// Live > Gas Oracle > L1 Data Fee block refreshes every second; a slow
// response from tick N is dropped when it lands after tick N+1.
type readNetworkFeeL1FeeMsg struct {
	gen  uint64
	snap *l2.L1DataFeeSnapshot
}

// readNetworkFeeGasMsg carries one live gas-price sample (eth_gasPrice
// + eth_maxPriorityFeePerGas, with the derived baseFee). It rides the
// same generation-tagged tick loop as the vault samples so a slow
// response from tick N can't overwrite tick N+1's display.
type readNetworkFeeGasMsg struct {
	gen  uint64
	snap *l2.GasPriceSnapshot
}

// readNetworkFeeBlockMsg carries the one-shot latest-block extraData
// fetch (Jovian EIP-1559 params). Fetched once on entry and on 'r'
// refresh — not on the vault tick, since the operator asked for a
// point-in-time read, not a live stream.
type readNetworkFeeBlockMsg struct {
	snap *l2.BlockEIP1559Snapshot
}

// --- screen ---

// readNetworkFeeScreen is the TUI counterpart of
// `op-ctl read network-fee`. A fixed header (RPC endpoints) sits above
// a single scrollable body split into two groups:
//
//	Live    — the numbers that move:
//	            FeeVaults (balances, auto-refresh 1s)
//	            Gas Oracle
//	              GasPrice    (maxPriorityFeePerGas, baseFee — live 1s)
//	              L1 Data Fee (l1BaseFee, blobBaseFee — GasPriceOracle)
//	Config  — slow-changing chain parameters:
//	            GasPriceOracle (scalars, decimals, pinned constants)
//	            Block EIP-1559 (extraData-decoded params in force)
//	            SystemConfig   (deploy/governance-paced fee params)
//
// Live sits on top because it is the operationally interesting data
// (real-time balance + gas accrual); Config reads more like static
// metadata. The body scrolls (j/k, g/G, PgUp/PgDn) since the full
// parameter set overflows most terminals; the offset survives the 1s
// auto-refresh so the operator does not get bounced back to the top.
//
//	read / network-fee
//	L1: https://...
//	L2: http://...
//
//	Live
//	FeeVaults                                             latency: 27ms
//	  ...
//	Gas Oracle
//	  GasPrice                                            latency: 9ms
//	    maxPriorityFeePerGas  1000000
//	    baseFee               7
//	  L1 Data Fee                                         latency: 31ms
//	    l1BaseFee             ...
//	    blobBaseFee           ...
//
//	Config
//	GasPriceOracle (0x...)                                latency: 12ms
//	  ...
//
//	↑/↓ j/k scroll · g/G top/bottom · r refresh · q back · auto 1s
type readNetworkFeeScreen struct {
	l1RPCURL         string
	l2RPCURL         string
	systemConfigAddr string
	timeout          time.Duration

	sysCfgLoading bool
	sysCfg        *l1.SystemConfigSnapshot
	sysCfgErr     error

	// gpo is the GasPriceOracle's static parameters (scalars, decimals,
	// pinned constants, version). Fetched once on entry + 'r' refresh —
	// these are governance/deploy-paced, not live.
	gpoLoading bool
	gpo        *l2.GasPriceOracleSnapshot
	gpoErr     error

	blkLoading bool
	blk        *l2.BlockEIP1559Snapshot

	// gas + l1fee both ride the vaultGen tick loop (real-time, 1s). gas
	// is the live gasPrice / tip / baseFee; l1fee is the live l1BaseFee /
	// blobBaseFee from the GasPriceOracle (the moving subset of gpo).
	gasLoading bool
	gas        *l2.GasPriceSnapshot

	l1feeLoading bool
	l1fee        *l2.L1DataFeeSnapshot

	// vaultGen is the monotonic generation counter for the auto-tick
	// loop. Init() emits the gen=1 tick immediately so the first RPC
	// fan-out fires without waiting one full interval. After each
	// accepted tick, gen is bumped, fetches are tagged with the new
	// gen, and the next tick is scheduled.
	vaultGen      uint64
	vaultsLoading bool
	vaults        []l2.VaultSnapshot
	vaultsLat     time.Duration
	vaultsErr     error

	// offset is the scroll position of the body region (Live + Config).
	// Clamped against the rendered body height on each scroll key and
	// again in View, so it survives the 1s auto-refresh without losing
	// the operator's place.
	offset int

	width, height int
}

func newReadNetworkFeeScreen(l1RPCURL, l2RPCURL, systemConfigAddr string, timeout time.Duration) readNetworkFeeScreen {
	return readNetworkFeeScreen{
		l1RPCURL:         l1RPCURL,
		l2RPCURL:         l2RPCURL,
		systemConfigAddr: systemConfigAddr,
		timeout:          timeout,
		sysCfgLoading:    true,
		vaultsLoading:    true,
		gpoLoading:       true,
		blkLoading:       true,
		gasLoading:       true,
		l1feeLoading:     true,
	}
}

func (s readNetworkFeeScreen) Init() tea.Cmd {
	// Emit gen=1 immediately (no initial 1s wait), in parallel with the
	// one-shot SystemConfig + GasPriceOracle + latest-block fetches. The
	// live samples (vaults, gas, l1 data fee) ride the gen=1 tick; the
	// one-shot fetches here cover the static parameters.
	return tea.Batch(
		fetchSysCfgCmd(s.l1RPCURL, s.systemConfigAddr, s.timeout),
		fetchGPOCmd(s.l2RPCURL, s.timeout),
		fetchBlockEIP1559Cmd(s.l2RPCURL, s.timeout),
		func() tea.Msg { return readNetworkFeeVaultTickMsg{gen: 1} },
	)
}

// fetchSysCfgCmd issues a single batched RPC POST against the L1
// SystemConfig proxy. When the proxy address is empty (state.json
// missing SystemConfigProxy), the command short-circuits to a clear
// error message so the operator knows where to look.
func fetchSysCfgCmd(l1RPCURL, addr string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(addr) == "" {
			return readNetworkFeeSysCfgMsg{err: fmt.Errorf("SystemConfigProxy not in state.json — add it to opChainDeployments[0]")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap, err := l1.FetchSystemConfigSnapshot(ctx, nil, l1RPCURL, addr)
		return readNetworkFeeSysCfgMsg{snap: snap, err: err}
	}
}

// fetchGPOCmd issues the batched RPC POST against the L2
// GasPriceOracle predeploy for its static parameters (scalars,
// decimals, pinned constants, version). One-shot: issued on entry and
// on manual 'r' refresh — the live l1BaseFee / blobBaseFee subset is
// polled separately by fetchL1FeeCmd on the tick.
func fetchGPOCmd(l2RPCURL string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap, err := l2.FetchGasPriceOracleSnapshot(ctx, nil, l2RPCURL)
		return readNetworkFeeGPOMsg{snap: snap, err: err}
	}
}

// fetchL1FeeCmd issues l1BaseFee() + blobBaseFee() against the L2
// GasPriceOracle, tagged with the tick gen so Update can drop stale
// samples — the live subset that rides the 1s vault tick loop.
func fetchL1FeeCmd(l2RPCURL string, timeout time.Duration, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap := l2.FetchL1DataFeeSnapshot(ctx, nil, l2RPCURL)
		return readNetworkFeeL1FeeMsg{gen: gen, snap: snap}
	}
}

// fetchBlockEIP1559Cmd fetches the latest L2 block and decodes its
// extraData into the Jovian EIP-1559 params. One-shot: issued on entry
// and on manual 'r' refresh, never on the vault tick.
func fetchBlockEIP1559Cmd(l2RPCURL string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap, _ := l2.FetchLatestBlockEIP1559(ctx, nil, l2RPCURL)
		return readNetworkFeeBlockMsg{snap: snap}
	}
}

// fetchVaultsCmd issues balance + totalProcessed reads for the 4
// FeeVault predeploys against the L2 RPC endpoint. gen flows back
// out unchanged so Update can drop stale samples.
func fetchVaultsCmd(l2RPCURL string, timeout time.Duration, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snaps, lat, err := l2.FetchAllVaultSnapshots(ctx, nil, l2RPCURL)
		return readNetworkFeeVaultsMsg{gen: gen, snaps: snaps, lat: lat, err: err}
	}
}

// fetchGasCmd issues the live gas-price reads (eth_gasPrice +
// eth_maxPriorityFeePerGas) against the L2 RPC, tagged with the tick
// gen so Update can drop stale samples — same loop as fetchVaultsCmd.
func fetchGasCmd(l2RPCURL string, timeout time.Duration, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap := l2.FetchGasPriceSnapshot(ctx, nil, l2RPCURL)
		return readNetworkFeeGasMsg{gen: gen, snap: snap}
	}
}

// vaultTickCmd schedules the next readNetworkFeeVaultTickMsg, carrying
// `nextGen`. Update enforces msg.gen == s.vaultGen+1 before accepting.
func vaultTickCmd(interval time.Duration, nextGen uint64) tea.Cmd {
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return readNetworkFeeVaultTickMsg{gen: nextGen}
	})
}

func (s readNetworkFeeScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height

	case readNetworkFeeSysCfgMsg:
		s.sysCfgLoading = false
		s.sysCfg = m.snap
		s.sysCfgErr = m.err

	case readNetworkFeeGPOMsg:
		// One-shot (entry + 'r'): the static oracle parameters, no gen
		// gate. The live l1/blob subset arrives via readNetworkFeeL1FeeMsg.
		s.gpoLoading = false
		s.gpo = m.snap
		s.gpoErr = m.err

	case readNetworkFeeL1FeeMsg:
		// Drop stragglers from earlier ticks — same generation gate as
		// the vault and gas samples.
		if m.gen != s.vaultGen {
			return s, nil
		}
		s.l1feeLoading = false
		s.l1fee = m.snap

	case readNetworkFeeBlockMsg:
		s.blkLoading = false
		s.blk = m.snap

	case readNetworkFeeVaultTickMsg:
		// Drop ticks that don't match the next-expected gen — happens
		// when an out-of-order/stale tick lands after we already paused
		// the loop (e.g. after a manual 'r' refresh fires its own
		// fresh tick chain).
		if m.gen != s.vaultGen+1 {
			return s, nil
		}
		s.vaultGen = m.gen
		return s, tea.Batch(
			fetchVaultsCmd(s.l2RPCURL, s.timeout, s.vaultGen),
			fetchGasCmd(s.l2RPCURL, s.timeout, s.vaultGen),
			fetchL1FeeCmd(s.l2RPCURL, s.timeout, s.vaultGen),
			vaultTickCmd(vaultRefreshInterval, s.vaultGen+1),
		)

	case readNetworkFeeVaultsMsg:
		// Drop stragglers from earlier ticks — only the most recent
		// generation's sample is allowed to update the display.
		if m.gen != s.vaultGen {
			return s, nil
		}
		s.vaultsLoading = false
		s.vaults = m.snaps
		s.vaultsLat = m.lat
		s.vaultsErr = m.err

	case readNetworkFeeGasMsg:
		// Drop stragglers from earlier ticks — same generation gate as
		// the vault samples above.
		if m.gen != s.vaultGen {
			return s, nil
		}
		s.gasLoading = false
		s.gas = m.snap

	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc":
			return s, func() tea.Msg { return popMsg{} }
		case "ctrl+c":
			return s, tea.Quit
		case "j", "down":
			s.offset++
			return s.clampOffset(), nil
		case "k", "up":
			s.offset--
			return s.clampOffset(), nil
		case "g", "home":
			s.offset = 0
			return s, nil
		case "G", "end":
			s.offset = s.maxScrollOffset()
			return s, nil
		case "pgdown", "ctrl+d", " ":
			s.offset += halfPage(s.height)
			return s.clampOffset(), nil
		case "pgup", "ctrl+u", "b":
			s.offset -= halfPage(s.height)
			return s.clampOffset(), nil
		case "r":
			// Manual refresh: re-fire the one-shot SystemConfig +
			// GasPriceOracle + latest block fetches AND restart the vault
			// tick chain by issuing the next expected gen immediately. The
			// existing tea.Tick still delivers its scheduled tick but the
			// gen check discards it as stale. The live samples (vaults,
			// gas, l1 data fee) refresh via the restarted chain.
			s.sysCfgLoading = true
			s.sysCfg = nil
			s.sysCfgErr = nil
			s.gpoLoading = true
			s.gpo = nil
			s.gpoErr = nil
			s.blkLoading = true
			s.blk = nil
			nextGen := s.vaultGen + 1
			return s, tea.Batch(
				fetchSysCfgCmd(s.l1RPCURL, s.systemConfigAddr, s.timeout),
				fetchGPOCmd(s.l2RPCURL, s.timeout),
				fetchBlockEIP1559Cmd(s.l2RPCURL, s.timeout),
				func() tea.Msg { return readNetworkFeeVaultTickMsg{gen: nextGen} },
			)
		}
	}
	return s, nil
}

// rnfHeaderLines is the fixed (non-scrolling) header height: the title
// plus the L1 and L2 endpoint lines.
const rnfHeaderLines = 3

func (s readNetworkFeeScreen) renderHeader() string {
	return theme.Title.Render("read / network-fee") + "\n" +
		theme.Subtitle.Render("L1: "+s.l1RPCURL) + "\n" +
		theme.Subtitle.Render("L2: "+s.l2RPCURL)
}

// renderBody assembles the scrollable Live + Config content. Kept
// separate from View so the scroll math (and maxScrollOffset) can
// measure the full content height without the fixed header/footer.
func (s readNetworkFeeScreen) renderBody() string {
	var b strings.Builder

	// ----- Live: the numbers that move -----
	b.WriteString(s.renderLiveSection())

	b.WriteString("\n\n")

	// ----- Config: slow-changing chain parameters -----
	b.WriteString(theme.Title.Render("Config"))
	b.WriteString("\n")
	b.WriteString(s.renderGPOSection())
	b.WriteString("\n")
	b.WriteString(s.renderBlockSection())
	b.WriteString("\n")
	b.WriteString(s.renderSysCfgSection())

	return b.String()
}

func (s readNetworkFeeScreen) View() string {
	header := s.renderHeader()
	footer := theme.Footer(theme.KeyScroll, theme.KeyTopBottom, theme.KeyRefresh, theme.KeyBack) +
		theme.Help.Render(fmt.Sprintf(" · auto %s", vaultRefreshInterval))
	body := s.renderBody()

	// Before the first WindowSizeMsg there is no viewport to scroll
	// within — render the whole thing top-to-bottom.
	if s.width == 0 || s.height == 0 {
		return header + "\n\n" + body + "\n" + footer
	}

	bodyLines := strings.Split(strings.TrimRight(body, "\n"), "\n")

	// Rows for the scrolling body = total height minus the fixed header,
	// the blank line under it, and the footer.
	avail := s.height - rnfHeaderLines - 2
	if avail < 1 {
		avail = 1
	}
	off := clampInt(s.offset, 0, max0(len(bodyLines)-avail))
	end := off + avail
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[off:end]
	for len(visible) < avail {
		visible = append(visible, "")
	}
	return header + "\n\n" + strings.Join(visible, "\n") + "\n" + footer
}

// maxScrollOffset is the largest offset that still shows content on the
// last line — used by the 'G'/'end' jump and the per-key clamp.
func (s readNetworkFeeScreen) maxScrollOffset() int {
	avail := s.height - rnfHeaderLines - 2
	if avail < 1 {
		avail = 1
	}
	bodyLines := strings.Count(strings.TrimRight(s.renderBody(), "\n"), "\n") + 1
	return max0(bodyLines - avail)
}

// clampOffset keeps s.offset within [0, maxScrollOffset] after an
// incremental scroll so j/k from a clamped position respond immediately
// instead of walking back through a stale out-of-range value.
func (s readNetworkFeeScreen) clampOffset() readNetworkFeeScreen {
	s.offset = clampInt(s.offset, 0, s.maxScrollOffset())
	return s
}

// rnfGasOracleColWidth pins the Gas Oracle column to a constant width.
// Without this the column's width tracked its widest line, so a 1-digit
// → 2-digit change in the latency/tick or a fee value reflowed the
// column and visibly shifted the divider + Fee Vaults to its right every
// second. The value comfortably exceeds the longest realistic line
// (label padded to 22 + a ~13-digit gwei value, or the latency header).
const rnfGasOracleColWidth = 48

// renderLiveSection lays the two live groups out side by side —
// [Gas Oracle] │ [Fee Vaults] — as plain text columns separated by a
// vertical rule. The left column is fixed-width so the divider and Fee
// Vaults stay put as values tick; the body still scrolls vertically and
// overflows on terminals too narrow to hold both.
func (s readNetworkFeeScreen) renderLiveSection() string {
	gasOracle := lipgloss.NewStyle().Width(rnfGasOracleColWidth).
		Render(strings.TrimRight(s.renderGasOracleSection(), "\n"))
	feeVaults := strings.TrimRight(s.renderVaultsSection(), "\n")

	// Vertical divider spanning the taller of the two columns.
	h := lipgloss.Height(gasOracle)
	if hv := lipgloss.Height(feeVaults); hv > h {
		h = hv
	}
	bars := make([]string, h)
	for i := range bars {
		bars[i] = "│"
	}
	divider := lipgloss.NewStyle().Foreground(theme.ColorDim).Render(strings.Join(bars, "\n"))

	row := lipgloss.JoinHorizontal(lipgloss.Top, gasOracle, "  ", divider, "  ", feeVaults)
	return theme.Title.Render("Live") + "\n" + row
}

// renderGasOracleSection is the Live "Gas Oracle" group: the GasPrice
// sub-block (live execution-gas suggestion, refreshed every tick) and
// the L1 Data Fee sub-block (l1BaseFee / blobBaseFee from the
// GasPriceOracle predeploy).
func (s readNetworkFeeScreen) renderGasOracleSection() string {
	var b strings.Builder
	b.WriteString(theme.Header.Render("Gas Oracle"))
	// Both sub-blocks ride the same tick, so the latency + tick indicator
	// lives once on the group header rather than on each sub-header. The
	// "<n>ms" token is right-padded to a fixed width so the "(tick #...)"
	// label keeps its column as the latency value changes.
	if s.gas != nil {
		latTok := rnfPadRight(fmt.Sprintf("%dms", s.gas.Latency.Milliseconds()), 6)
		b.WriteString(theme.Subtitle.Render(fmt.Sprintf("    latency: %s (tick #%d)", latTok, s.vaultGen)))
	}
	b.WriteString("\n")

	// --- GasPrice: live, rides the 1s vault tick ---
	b.WriteString("  ")
	b.WriteString(theme.Section.Render("GasPrice"))
	b.WriteString("\n")
	switch {
	case s.gasLoading && s.gas == nil:
		b.WriteString(rnfIndentLine(4, theme.Subtitle.Render("loading ...")))
	case s.gas == nil:
		b.WriteString(rnfIndentLine(4, theme.ErrText.Render("ERR no data")))
	default:
		b.WriteString(rnfBigRow(4, "maxPriorityFeePerGas", s.gas.MaxPriorityFee, s.gas.Errors["maxPriorityFee"]))
		// baseFee = gasPrice - maxPriorityFeePerGas; only meaningful when
		// both inputs succeeded.
		if s.gas.Errors["gasPrice"] != nil || s.gas.Errors["maxPriorityFee"] != nil {
			b.WriteString(rnfErrRow(4, "baseFee", fmt.Errorf("needs gasPrice and maxPriorityFeePerGas")))
		} else {
			b.WriteString(rnfBigRow(4, "baseFee", s.gas.BaseFee, nil))
		}
	}

	// --- L1 Data Fee: the live L1 cost inputs (l1BaseFee / blobBaseFee),
	// polled each tick — separate from the one-shot GasPriceOracle params.
	b.WriteString("  ")
	b.WriteString(theme.Section.Render("L1 Data Fee"))
	b.WriteString("\n")
	switch {
	case s.l1feeLoading && s.l1fee == nil:
		b.WriteString(rnfIndentLine(4, theme.Subtitle.Render("loading ...")))
	case s.l1fee == nil:
		b.WriteString(rnfIndentLine(4, theme.ErrText.Render("ERR no data")))
	default:
		b.WriteString(rnfBigRow(4, "l1BaseFee", s.l1fee.L1BaseFee, s.l1fee.Errors["l1BaseFee"]))
		b.WriteString(rnfBigRow(4, "blobBaseFee", s.l1fee.BlobBaseFee, s.l1fee.Errors["blobBaseFee"]))
	}
	return b.String()
}

// rnfIndentLine renders one already-styled string at the given indent.
func rnfIndentLine(indent int, s string) string {
	return strings.Repeat(" ", indent) + s + "\n"
}

// rnfBigRow / rnfErrRow are indent-aware label/value rows for the nested
// Live sub-blocks (the flat Config rows use the indent-2 format*Row
// helpers below).
func rnfBigRow(indent int, label string, v *big.Int, e error) string {
	if e != nil {
		return rnfErrRow(indent, label, e)
	}
	return fmt.Sprintf("%s%s  %s\n",
		strings.Repeat(" ", indent),
		theme.Label.Render(padLabel(label)),
		theme.Value.Render(bigOrZero(v)),
	)
}

func rnfErrRow(indent int, label string, e error) string {
	return fmt.Sprintf("%s%s  %s\n",
		strings.Repeat(" ", indent),
		theme.Label.Render(padLabel(label)),
		theme.ErrText.Render(fmt.Sprintf("ERR %v", e)),
	)
}

func (s readNetworkFeeScreen) renderVaultsSection() string {
	var b strings.Builder
	b.WriteString(theme.Header.Render("FeeVaults"))
	if s.vaults != nil {
		b.WriteString(theme.Subtitle.Render(fmt.Sprintf("    latency: %dms  (tick #%d)", s.vaultsLat.Milliseconds(), s.vaultGen)))
	}
	b.WriteString("\n")

	if s.vaultsLoading && len(s.vaults) == 0 {
		b.WriteString(theme.Subtitle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	if s.vaultsErr != nil {
		b.WriteString(theme.ErrText.Render(fmt.Sprintf("  ERR %v", s.vaultsErr)))
		b.WriteString("\n")
	}
	b.WriteString(s.renderVaultsTable())
	b.WriteString("\n")
	return b.String()
}

// Column widths for the vaults section. Chosen so the longest vault
// name ("SequencerFeeVault" = 17) fits in the label column and the
// 20-byte hex address fits without truncation. Balance/totalProcessed
// widths are sized for typical wei values seen on this chain (<= 14
// digits); larger values overflow into the next column rather than
// being truncated — preferable to silently dropping precision.
const (
	rnfVaultNameWidth      = 20
	rnfVaultBalanceWidth   = 16
	rnfVaultTotalProcWidth = 16
)

// renderVaultsTable lays the 4 vault rows out as plain aligned text
// — same key/value rhythm as the SystemConfig section, just with a
// single header row above naming the three data columns (balance,
// totalProcessed, address). No box-drawing borders.
func (s readNetworkFeeScreen) renderVaultsTable() string {
	var b strings.Builder
	// Header row: empty vault-name slot + 3 column titles.
	b.WriteString("  ")
	b.WriteString(theme.ColHeader.Render(rnfPadRight("", rnfVaultNameWidth)))
	b.WriteString("  ")
	b.WriteString(theme.ColHeader.Render(rnfPadRight("balance", rnfVaultBalanceWidth)))
	b.WriteString("  ")
	b.WriteString(theme.ColHeader.Render(rnfPadRight("totalProcessed", rnfVaultTotalProcWidth)))
	b.WriteString("  ")
	b.WriteString(theme.ColHeader.Render("address"))
	b.WriteString("\n")

	for _, v := range s.vaults {
		bal := bigStrOrErrStr(v.Balance, v.Errors["balance"])
		tp := bigStrOrErrStr(v.TotalProcessed, v.Errors["totalProcessed"])
		b.WriteString("  ")
		b.WriteString(theme.Label.Render(rnfPadRight(v.Name, rnfVaultNameWidth)))
		b.WriteString("  ")
		b.WriteString(theme.Value.Render(rnfPadRight(bal, rnfVaultBalanceWidth)))
		b.WriteString("  ")
		b.WriteString(theme.Value.Render(rnfPadRight(tp, rnfVaultTotalProcWidth)))
		b.WriteString("  ")
		b.WriteString(theme.Subtitle.Render(v.Address))
		b.WriteString("\n")
	}
	return b.String()
}

// rnfPadRight returns s padded with spaces to at least width. Overlong
// values are returned unchanged (a single overflowed cell is better
// than a silent truncation that hides precision in a wei value).
func rnfPadRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// renderGPOSection prints the GasPriceOracle compressed-size
// regression constants (costIntercept / costFastlzCoef /
// minTransactionSize) as a small key/value list. Same row formatting
// as SystemConfig so the two sections read uniformly.
func (s readNetworkFeeScreen) renderGPOSection() string {
	var b strings.Builder
	b.WriteString(theme.Header.Render("GasPriceOracle"))
	if s.gpo != nil {
		b.WriteString(theme.Subtitle.Render(" (" + s.gpo.Address + ")"))
		b.WriteString(theme.Subtitle.Render(fmt.Sprintf("    latency: %dms", s.gpo.Latency.Milliseconds())))
	}
	b.WriteString("\n")

	if s.gpoLoading {
		b.WriteString(theme.Subtitle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	if s.gpoErr != nil {
		b.WriteString(theme.ErrText.Render(fmt.Sprintf("  ERR %v", s.gpoErr)))
		b.WriteString("\n")
		if s.gpo == nil {
			return b.String()
		}
	}
	snap := s.gpo
	// The live Ecotone+ fee inputs (baseFee / l1BaseFee / blobBaseFee)
	// now live in the Live > Gas Oracle group; what remains here are the
	// scaling parameters and pinned constants.
	b.WriteString(formatU32Row("baseFeeScalar", snap.BaseFeeScalar, snap.Errors["baseFeeScalar"]))
	b.WriteString(formatU32Row("blobBaseFeeScalar", snap.BlobBaseFeeScalar, snap.Errors["blobBaseFeeScalar"]))
	b.WriteString(formatBigRow("decimals", snap.Decimals, snap.Errors["decimals"]))
	// The three constants are compile-time `private constant` values
	// inlined into bytecode — not readable via eth_call. We display
	// the pinned literals and surface drift via the version() probe
	// below. Values are padded to a uniform width so the trailing
	// "(constant, pinned ...)" suffix lines up across rows.
	pinnedSuffix := theme.Subtitle.Render(fmt.Sprintf("  (constant, pinned to v%s)", snap.ConstantsSourceVersion))
	vCost := fmt.Sprintf("%d", snap.CostIntercept)
	vCoef := fmt.Sprintf("%d", snap.CostFastlzCoef)
	vMin := bigOrZero(snap.MinTransactionSize)
	valW := max(len(vCost), len(vCoef), len(vMin))
	pad := func(v string) string { return v + strings.Repeat(" ", valW-len(v)) }
	b.WriteString(formatStrRow("costIntercept", pad(vCost)+pinnedSuffix))
	b.WriteString(formatStrRow("costFastlzCoef", pad(vCoef)+pinnedSuffix))
	b.WriteString(formatStrRow("minTransactionSize", pad(vMin)+pinnedSuffix))
	if e := snap.Errors["version"]; e != nil {
		b.WriteString(formatStrRow("version",
			theme.ErrText.Render(fmt.Sprintf("ERR %v — drift undetectable", e))))
	} else if snap.VersionMatches {
		b.WriteString(formatStrRow("version",
			snap.Version+theme.Subtitle.Render(fmt.Sprintf("  (matches pinned v%s)", snap.ConstantsSourceVersion))))
	} else {
		b.WriteString(formatStrRow("version",
			theme.ErrText.Render(fmt.Sprintf("%s (DRIFT: pinned to v%s — values may not match)",
				snap.Version, snap.ConstantsSourceVersion))))
	}
	return b.String()
}

// renderBlockSection prints the EIP-1559 params decoded from the
// latest block's extraData (Jovian 17-byte layout). Same key/value
// rhythm as the other sections; the raw extraData is shown as a muted
// subtitle so a format mismatch is debuggable at a glance.
func (s readNetworkFeeScreen) renderBlockSection() string {
	var b strings.Builder
	b.WriteString(theme.Header.Render("Block EIP-1559"))
	if s.blk != nil && s.blk.BlockNumber != nil {
		b.WriteString(theme.Subtitle.Render(fmt.Sprintf(" (block %s)", s.blk.BlockNumber)))
		b.WriteString(theme.Subtitle.Render(fmt.Sprintf("    latency: %dms", s.blk.Latency.Milliseconds())))
	}
	b.WriteString("\n")

	if s.blkLoading {
		b.WriteString(theme.Subtitle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	snap := s.blk
	if snap == nil {
		b.WriteString(theme.ErrText.Render("  ERR no data"))
		b.WriteString("\n")
		return b.String()
	}
	if len(snap.ExtraData) > 0 {
		b.WriteString(formatStrRow("extraData", fmt.Sprintf("0x%x", snap.ExtraData)))
	}
	if snap.Err != nil {
		b.WriteString(formatErrRow("decode", snap.Err))
		return b.String()
	}
	b.WriteString(formatStrRow("version", fmt.Sprintf("%d (%s)", snap.Version, snap.ForkName())))
	b.WriteString(formatU32Row("denominator", snap.Denominator, nil))
	b.WriteString(formatU32Row("elasticity", snap.Elasticity, nil))
	if snap.HasMinBaseFee {
		b.WriteString(formatU64Row("minBaseFee", snap.MinBaseFee, nil))
	}
	return b.String()
}

func (s readNetworkFeeScreen) renderSysCfgSection() string {
	var b strings.Builder
	b.WriteString(theme.Header.Render("SystemConfig"))
	if s.systemConfigAddr != "" {
		b.WriteString(theme.Subtitle.Render(" (" + s.systemConfigAddr + ")"))
	}
	if s.sysCfg != nil {
		b.WriteString(theme.Subtitle.Render(fmt.Sprintf("    latency: %dms", s.sysCfg.Latency.Milliseconds())))
	}
	b.WriteString("\n")

	if s.sysCfgLoading {
		b.WriteString(theme.Subtitle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	if s.sysCfgErr != nil {
		b.WriteString(theme.ErrText.Render(fmt.Sprintf("  ERR %v", s.sysCfgErr)))
		b.WriteString("\n")
		if s.sysCfg == nil {
			return b.String()
		}
	}
	snap := s.sysCfg
	b.WriteString(formatU32Row("basefeeScalar", snap.BasefeeScalar, snap.Errors["basefeeScalar"]))
	b.WriteString(formatU32Row("blobbasefeeScalar", snap.BlobBasefeeScalar, snap.Errors["blobbasefeeScalar"]))
	b.WriteString(formatBigRow("scalar", snap.Scalar, snap.Errors["scalar"]))
	b.WriteString(formatBigRow("overhead", snap.Overhead, snap.Errors["overhead"]))
	b.WriteString(formatU64Row("gasLimit", snap.GasLimit, snap.Errors["gasLimit"]))
	b.WriteString(formatU32Row("eip1559Denominator", snap.EIP1559Denominator, snap.Errors["eip1559Denominator"]))
	b.WriteString(formatU32Row("eip1559Elasticity", snap.EIP1559Elasticity, snap.Errors["eip1559Elasticity"]))
	b.WriteString(formatU32Row("operatorFeeScalar", snap.OperatorFeeScalar, snap.Errors["operatorFeeScalar"]))
	b.WriteString(formatU64Row("operatorFeeConstant", snap.OperatorFeeConstant, snap.Errors["operatorFeeConstant"]))
	b.WriteString(formatU16Row("daFootprintGasScalar", snap.DAFootprintGasScalar, snap.Errors["daFootprintGasScalar"]))
	b.WriteString(formatU64Row("minBaseFee", snap.MinBaseFee, snap.Errors["minBaseFee"]))
	if e := snap.Errors["resourceConfig"]; e != nil {
		b.WriteString(formatErrRow("resourceConfig", e))
	} else {
		rc := snap.ResourceConfig
		val := fmt.Sprintf("maxResourceLimit=%d elasticityMultiplier=%d baseFeeMaxChangeDenominator=%d minimumBaseFee=%d systemTxMaxGas=%d maximumBaseFee=%s",
			rc.MaxResourceLimit, rc.ElasticityMultiplier, rc.BaseFeeMaxChangeDenominator,
			rc.MinimumBaseFee, rc.SystemTxMaxGas, bigOrZero(rc.MaximumBaseFee),
		)
		b.WriteString(formatStrRow("resourceConfig", val))
	}
	return b.String()
}

// --- row formatters (SystemConfig key/value list) ---

// labelWidth pads the label column so values line up vertically. Wide
// enough for "daFootprintGasScalar" (20 chars), the longest label in
// the SystemConfig section.
const labelWidth = 22

func formatU32Row(label string, v uint32, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		theme.Label.Render(padLabel(label)),
		theme.Value.Render(fmt.Sprintf("%d", v)),
	)
}

func formatU16Row(label string, v uint16, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		theme.Label.Render(padLabel(label)),
		theme.Value.Render(fmt.Sprintf("%d", v)),
	)
}

func formatU64Row(label string, v uint64, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		theme.Label.Render(padLabel(label)),
		theme.Value.Render(fmt.Sprintf("%d", v)),
	)
}

func formatBigRow(label string, v *big.Int, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		theme.Label.Render(padLabel(label)),
		theme.Value.Render(bigOrZero(v)),
	)
}

func formatStrRow(label, value string) string {
	return fmt.Sprintf("  %s  %s\n",
		theme.Label.Render(padLabel(label)),
		theme.Value.Render(value),
	)
}

func formatErrRow(label string, e error) string {
	return fmt.Sprintf("  %s  %s\n",
		theme.Label.Render(padLabel(label)),
		theme.ErrText.Render(fmt.Sprintf("ERR %v", e)),
	)
}

func padLabel(label string) string {
	if len(label) >= labelWidth {
		return label
	}
	return label + strings.Repeat(" ", labelWidth-len(label))
}

func bigOrZero(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func bigStrOrErrStr(n *big.Int, e error) string {
	if e != nil {
		return fmt.Sprintf("ERR(%v)", e)
	}
	if n == nil {
		return "0"
	}
	return n.String()
}

// RunReadNetworkFee runs the screen as a standalone alt-screen tea
// program — invoked from the CLI for parity with RunReadDisputeGame.
// The standard `op-ctl read network-fee` path uses --plain by default
// (the existing CLI flow); this entry point is reserved for a future
// `--tui` flag without baking in another binary.
func RunReadNetworkFee(ctx context.Context, l1RPCURL, l2RPCURL, systemConfigAddr string, timeout time.Duration) error {
	scr := newReadNetworkFeeScreen(l1RPCURL, l2RPCURL, systemConfigAddr, timeout)
	p := tea.NewProgram(scr, tea.WithContext(ctx), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
