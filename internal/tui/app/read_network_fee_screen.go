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
// result. Like SystemConfig, these are constants — fetched on entry
// and re-fetched on 'r' refresh.
type readNetworkFeeGPOMsg struct {
	snap *l2.GasPriceOracleSnapshot
	err  error
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
// `op-ctl read network-fee`. Two sections, top-to-bottom:
//
//	FeeVaults   — auto-refreshing every 1s
//	SystemConfig — fetched once on push, refresh on 'r'
//
// FeeVaults sits on top because it is the operationally interesting
// section (real-time balance accrual); SystemConfig parameters drift
// only on governance action so they read more like static metadata.
//
// Sketch:
//
//	read / network-fee
//	L1: https://...
//	L2: http://...
//
//	FeeVaults                                             latency: 27ms
//	┌──────────┬───────────────┬────────────────┬─────────────────────┐
//	│ balance  │ totalProcessed │ address        │ (vault name col)   │
//	└──────────┴───────────────┴────────────────┴─────────────────────┘
//
//	SystemConfig (0x...)                                  latency: 442ms
//	  basefeeScalar         1368
//	  ...
//
//	auto-refresh 1s · r refresh · q back
type readNetworkFeeScreen struct {
	l1RPCURL         string
	l2RPCURL         string
	systemConfigAddr string
	timeout          time.Duration

	sysCfgLoading bool
	sysCfg        *l1.SystemConfigSnapshot
	sysCfgErr     error

	gpoLoading bool
	gpo        *l2.GasPriceOracleSnapshot
	gpoErr     error

	blkLoading bool
	blk        *l2.BlockEIP1559Snapshot

	// gas rides the vaultGen tick loop (real-time, 1s) — the operator
	// wants live gasPrice / tip / baseFee, so it refreshes alongside the
	// vault balances rather than once on entry.
	gasLoading bool
	gas        *l2.GasPriceSnapshot

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
	}
}

func (s readNetworkFeeScreen) Init() tea.Cmd {
	// Emit gen=1 immediately (no initial 1s wait), in parallel with the
	// one-shot SystemConfig + GasPriceOracle fetches.
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
// GasPriceOracle predeploy for the three FastLZ→Brotli regression
// constants (costIntercept / costFastlzCoef / minTransactionSize).
// These are contract constants — no auto-tick, fetched on entry and
// on manual 'r' refresh.
func fetchGPOCmd(l2RPCURL string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap, err := l2.FetchGasPriceOracleSnapshot(ctx, nil, l2RPCURL)
		return readNetworkFeeGPOMsg{snap: snap, err: err}
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
		s.gpoLoading = false
		s.gpo = m.snap
		s.gpoErr = m.err

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
		case "r":
			// Manual refresh: re-fire SystemConfig + GasPriceOracle
			// fetches AND restart the vault tick chain by issuing the
			// next expected gen immediately. The existing tea.Tick will
			// still deliver its scheduled tick but the gen check will
			// discard it as stale.
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

// --- view styles ---

var (
	rnfTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	rnfMutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	rnfHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	rnfErrStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	rnfHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	rnfLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	rnfValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	rnfColHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
)

func (s readNetworkFeeScreen) View() string {
	var b strings.Builder

	b.WriteString(rnfTitleStyle.Render("read / network-fee"))
	b.WriteString("\n")
	b.WriteString(rnfMutedStyle.Render("L1: " + s.l1RPCURL))
	b.WriteString("\n")
	b.WriteString(rnfMutedStyle.Render("L2: " + s.l2RPCURL))
	b.WriteString("\n\n")

	// FeeVaults first — operationally interesting + auto-refreshing.
	b.WriteString(s.renderVaultsSection())
	b.WriteString("\n")
	// Live gas price — refreshes on the same 1s tick as the vaults.
	b.WriteString(s.renderGasSection())
	b.WriteString("\n")
	// GasPriceOracle constants — small, static, sits between the live
	// section and the slow-changing chain params for at-a-glance
	// inspection alongside the fee scalars.
	b.WriteString(s.renderGPOSection())
	b.WriteString("\n")
	// Latest block EIP-1559 params — one-shot decode of the Jovian
	// extraData (the params actually in force on-chain), sitting next to
	// SystemConfig's configured denominator/elasticity for comparison.
	b.WriteString(s.renderBlockSection())
	b.WriteString("\n")
	// SystemConfig second — slow-changing chain params.
	b.WriteString(s.renderSysCfgSection())
	b.WriteString("\n")

	b.WriteString(rnfHelpStyle.Render(fmt.Sprintf("auto-refresh %s · r refresh · q back", vaultRefreshInterval)))
	return b.String()
}

func (s readNetworkFeeScreen) renderVaultsSection() string {
	var b strings.Builder
	b.WriteString(rnfHeaderStyle.Render("FeeVaults"))
	if s.vaults != nil {
		b.WriteString(rnfMutedStyle.Render(fmt.Sprintf("    latency: %dms  (tick #%d)", s.vaultsLat.Milliseconds(), s.vaultGen)))
	}
	b.WriteString("\n")

	if s.vaultsLoading && len(s.vaults) == 0 {
		b.WriteString(rnfMutedStyle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	if s.vaultsErr != nil {
		b.WriteString(rnfErrStyle.Render(fmt.Sprintf("  ERR %v", s.vaultsErr)))
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
	rnfVaultNameWidth     = 20
	rnfVaultBalanceWidth  = 16
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
	b.WriteString(rnfColHeaderStyle.Render(rnfPadRight("", rnfVaultNameWidth)))
	b.WriteString("  ")
	b.WriteString(rnfColHeaderStyle.Render(rnfPadRight("balance", rnfVaultBalanceWidth)))
	b.WriteString("  ")
	b.WriteString(rnfColHeaderStyle.Render(rnfPadRight("totalProcessed", rnfVaultTotalProcWidth)))
	b.WriteString("  ")
	b.WriteString(rnfColHeaderStyle.Render("address"))
	b.WriteString("\n")

	for _, v := range s.vaults {
		bal := bigStrOrErrStr(v.Balance, v.Errors["balance"])
		tp := bigStrOrErrStr(v.TotalProcessed, v.Errors["totalProcessed"])
		b.WriteString("  ")
		b.WriteString(rnfLabelStyle.Render(rnfPadRight(v.Name, rnfVaultNameWidth)))
		b.WriteString("  ")
		b.WriteString(rnfValueStyle.Render(rnfPadRight(bal, rnfVaultBalanceWidth)))
		b.WriteString("  ")
		b.WriteString(rnfValueStyle.Render(rnfPadRight(tp, rnfVaultTotalProcWidth)))
		b.WriteString("  ")
		b.WriteString(rnfMutedStyle.Render(v.Address))
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

// renderGasSection prints the live gas suggestions (eth_gasPrice +
// eth_maxPriorityFeePerGas) and the derived baseFee. Refreshes on the
// vault tick, so it shows "loading ..." only until the first sample
// lands and then updates in place every second.
func (s readNetworkFeeScreen) renderGasSection() string {
	var b strings.Builder
	b.WriteString(rnfHeaderStyle.Render("GasPrice"))
	if s.gas != nil {
		b.WriteString(rnfMutedStyle.Render(fmt.Sprintf("    latency: %dms  (tick #%d)", s.gas.Latency.Milliseconds(), s.vaultGen)))
	}
	b.WriteString("\n")

	if s.gasLoading && s.gas == nil {
		b.WriteString(rnfMutedStyle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	snap := s.gas
	if snap == nil {
		b.WriteString(rnfErrStyle.Render("  ERR no data"))
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString(formatBigRow("gasPrice", snap.GasPrice, snap.Errors["gasPrice"]))
	b.WriteString(formatBigRow("maxPriorityFeePerGas", snap.MaxPriorityFee, snap.Errors["maxPriorityFee"]))
	// baseFee = gasPrice - maxPriorityFeePerGas; only meaningful when
	// both inputs succeeded.
	if snap.Errors["gasPrice"] != nil || snap.Errors["maxPriorityFee"] != nil {
		b.WriteString(formatErrRow("baseFee", fmt.Errorf("needs gasPrice and maxPriorityFeePerGas")))
	} else {
		b.WriteString(formatBigRow("baseFee", snap.BaseFee, nil))
	}
	return b.String()
}

// renderGPOSection prints the GasPriceOracle compressed-size
// regression constants (costIntercept / costFastlzCoef /
// minTransactionSize) as a small key/value list. Same row formatting
// as SystemConfig so the two sections read uniformly.
func (s readNetworkFeeScreen) renderGPOSection() string {
	var b strings.Builder
	b.WriteString(rnfHeaderStyle.Render("GasPriceOracle"))
	if s.gpo != nil {
		b.WriteString(rnfMutedStyle.Render(" (" + s.gpo.Address + ")"))
		b.WriteString(rnfMutedStyle.Render(fmt.Sprintf("    latency: %dms", s.gpo.Latency.Milliseconds())))
	}
	b.WriteString("\n")

	if s.gpoLoading {
		b.WriteString(rnfMutedStyle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	if s.gpoErr != nil {
		b.WriteString(rnfErrStyle.Render(fmt.Sprintf("  ERR %v", s.gpoErr)))
		b.WriteString("\n")
		if s.gpo == nil {
			return b.String()
		}
	}
	snap := s.gpo
	// Live Ecotone+ fee inputs first — public getters read via
	// eth_call each refresh, the operationally interesting numbers.
	b.WriteString(formatBigRow("baseFee", snap.BaseFee, snap.Errors["baseFee"]))
	b.WriteString(formatBigRow("l1BaseFee", snap.L1BaseFee, snap.Errors["l1BaseFee"]))
	b.WriteString(formatBigRow("blobBaseFee", snap.BlobBaseFee, snap.Errors["blobBaseFee"]))
	b.WriteString(formatU32Row("baseFeeScalar", snap.BaseFeeScalar, snap.Errors["baseFeeScalar"]))
	b.WriteString(formatU32Row("blobBaseFeeScalar", snap.BlobBaseFeeScalar, snap.Errors["blobBaseFeeScalar"]))
	b.WriteString(formatBigRow("decimals", snap.Decimals, snap.Errors["decimals"]))
	// The three constants are compile-time `private constant` values
	// inlined into bytecode — not readable via eth_call. We display
	// the pinned literals and surface drift via the version() probe
	// below. Values are padded to a uniform width so the trailing
	// "(constant, pinned ...)" suffix lines up across rows.
	pinnedSuffix := rnfMutedStyle.Render(fmt.Sprintf("  (constant, pinned to v%s)", snap.ConstantsSourceVersion))
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
			rnfErrStyle.Render(fmt.Sprintf("ERR %v — drift undetectable", e))))
	} else if snap.VersionMatches {
		b.WriteString(formatStrRow("version",
			snap.Version+rnfMutedStyle.Render(fmt.Sprintf("  (matches pinned v%s)", snap.ConstantsSourceVersion))))
	} else {
		b.WriteString(formatStrRow("version",
			rnfErrStyle.Render(fmt.Sprintf("%s (DRIFT: pinned to v%s — values may not match)",
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
	b.WriteString(rnfHeaderStyle.Render("Block EIP-1559"))
	if s.blk != nil && s.blk.BlockNumber != nil {
		b.WriteString(rnfMutedStyle.Render(fmt.Sprintf(" (block %s)", s.blk.BlockNumber)))
		b.WriteString(rnfMutedStyle.Render(fmt.Sprintf("    latency: %dms", s.blk.Latency.Milliseconds())))
	}
	b.WriteString("\n")

	if s.blkLoading {
		b.WriteString(rnfMutedStyle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	snap := s.blk
	if snap == nil {
		b.WriteString(rnfErrStyle.Render("  ERR no data"))
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
	b.WriteString(rnfHeaderStyle.Render("SystemConfig"))
	if s.systemConfigAddr != "" {
		b.WriteString(rnfMutedStyle.Render(" (" + s.systemConfigAddr + ")"))
	}
	if s.sysCfg != nil {
		b.WriteString(rnfMutedStyle.Render(fmt.Sprintf("    latency: %dms", s.sysCfg.Latency.Milliseconds())))
	}
	b.WriteString("\n")

	if s.sysCfgLoading {
		b.WriteString(rnfMutedStyle.Render("  loading ..."))
		b.WriteString("\n")
		return b.String()
	}
	if s.sysCfgErr != nil {
		b.WriteString(rnfErrStyle.Render(fmt.Sprintf("  ERR %v", s.sysCfgErr)))
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
		rnfLabelStyle.Render(padLabel(label)),
		rnfValueStyle.Render(fmt.Sprintf("%d", v)),
	)
}

func formatU16Row(label string, v uint16, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		rnfLabelStyle.Render(padLabel(label)),
		rnfValueStyle.Render(fmt.Sprintf("%d", v)),
	)
}

func formatU64Row(label string, v uint64, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		rnfLabelStyle.Render(padLabel(label)),
		rnfValueStyle.Render(fmt.Sprintf("%d", v)),
	)
}

func formatBigRow(label string, v *big.Int, e error) string {
	if e != nil {
		return formatErrRow(label, e)
	}
	return fmt.Sprintf("  %s  %s\n",
		rnfLabelStyle.Render(padLabel(label)),
		rnfValueStyle.Render(bigOrZero(v)),
	)
}

func formatStrRow(label, value string) string {
	return fmt.Sprintf("  %s  %s\n",
		rnfLabelStyle.Render(padLabel(label)),
		rnfValueStyle.Render(value),
	)
}

func formatErrRow(label string, e error) string {
	return fmt.Sprintf("  %s  %s\n",
		rnfLabelStyle.Render(padLabel(label)),
		rnfErrStyle.Render(fmt.Sprintf("ERR %v", e)),
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
