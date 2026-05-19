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
)

// readDGDetailFetchedMsg carries one snapshot fan-out result back to
// the detail screen. snap is non-nil even on hardErr so the breadcrumb
// header still has the game address.
type readDGDetailFetchedMsg struct {
	gen     uint64
	snap    *l1.GameSnapshot
	hardErr error
}

// readDGClaimDataFetchedMsg carries the per-claim getter results
// (claimData(i) + getChallengerDuration(i) for every i ∈ [0, N)),
// kicked off after the snapshot reveals ClaimDataLen. claims may be
// nil with hardErr set when the whole batch failed at transport level.
type readDGClaimDataFetchedMsg struct {
	gen     uint64
	claims  []l1.ClaimData
	errs    []error
	hardErr error
	latency time.Duration
}

// readDisputeGameDetailScreen renders the FaultDisputeGame snapshot
// grouped into 8 sections matching the contract layout the operator
// expects from the spec:
//
//  1. Identity                 — version, gameType, l2ChainId, gameCreator
//  2. Status & Timing          — status, createdAt, resolvedAt
//  3. Roles                    — proposer, challenger, l2BlockNumberChallenged + challenger
//  4. Output Root Claim        — rootClaim, l1Head, extraData, claimDataLen
//  5. Anchor                   — anchorStateRegistry, startingBlockNumber, startingRootHash
//  6. Execution VM             — absolutePrestate, vm
//  7. Bond Vault               — weth
//  8. Game Parameters          — maxGameDepth, splitDepth, maxClockDuration, clockExtension
//
// Per-field errors render inline as `ERR <message>` instead of values
// so a partial snapshot still renders (Permissionless games revert on
// proposer()/challenger()).
type readDisputeGameDetailScreen struct {
	l1RPCURL string
	gameAddr string
	timeout  time.Duration

	loading bool
	snap    *l1.GameSnapshot
	hardErr error
	gen     uint64

	// Claim-data phase: snapshot must finish first so we know N
	// (ClaimDataLen). When it does, Update kicks off a second batch
	// of 2N calls (claimData(i) + getChallengerDuration(i) per i).
	claimsLoading bool
	claims        []l1.ClaimData
	claimErrs     []error
	claimsHardErr error
	claimsLatency time.Duration

	offset        int
	width, height int
}

func newReadDisputeGameDetailScreen(l1RPCURL, gameAddr string, timeout time.Duration) readDisputeGameDetailScreen {
	return readDisputeGameDetailScreen{
		l1RPCURL: l1RPCURL,
		gameAddr: gameAddr,
		timeout:  timeout,
		loading:  true,
	}
}

func (s readDisputeGameDetailScreen) Init() tea.Cmd {
	return fetchReadDGDetailCmd(s.l1RPCURL, s.gameAddr, s.timeout, s.gen+1)
}

func fetchReadDGDetailCmd(l1RPCURL, gameAddr string, timeout time.Duration, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		snap, err := l1.FetchGameSnapshot(ctx, nil, l1RPCURL, gameAddr)
		return readDGDetailFetchedMsg{gen: gen, snap: snap, hardErr: err}
	}
}

// fetchReadDGClaimDataCmd issues the per-claim 2N batch (claimData(i) +
// getChallengerDuration(i) for i ∈ [0, n)). Fires only after the
// snapshot reveals ClaimDataLen — see the Phase-2 dispatch inside
// Update.
func fetchReadDGClaimDataCmd(l1RPCURL, gameAddr string, timeout time.Duration, n, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		claims, errs, latency, err := l1.FetchClaimData(ctx, nil, l1RPCURL, gameAddr, n)
		return readDGClaimDataFetchedMsg{
			gen:     gen,
			claims:  claims,
			errs:    errs,
			hardErr: err,
			latency: latency,
		}
	}
}

func (s readDisputeGameDetailScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = m.Width
		s.height = m.Height
	case readDGDetailFetchedMsg:
		if m.gen != s.gen+1 {
			return s, nil // stale
		}
		s.gen = m.gen
		s.loading = false
		s.snap = m.snap
		s.hardErr = m.hardErr
		// Phase 2 kicks off only when snapshot succeeded AND we have
		// at least one claim to fetch — otherwise leave the section
		// in the "(no claims)" empty state.
		if m.hardErr == nil && m.snap != nil && m.snap.ClaimDataLen != nil && m.snap.ClaimDataLen.Sign() > 0 {
			s.claimsLoading = true
			n := m.snap.ClaimDataLen.Uint64()
			return s, fetchReadDGClaimDataCmd(s.l1RPCURL, s.gameAddr, s.timeout, n, s.gen)
		}
		return s, nil
	case readDGClaimDataFetchedMsg:
		if m.gen != s.gen {
			return s, nil // stale (snapshot refresh raced ahead)
		}
		s.claimsLoading = false
		s.claims = m.claims
		s.claimErrs = m.errs
		s.claimsHardErr = m.hardErr
		s.claimsLatency = m.latency
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
		case "r":
			if !s.loading {
				// Reset both phases so stale rows from the prior fetch
				// don't visually shadow the in-flight one.
				s.loading = true
				s.claimsLoading = false
				s.claims = nil
				s.claimErrs = nil
				s.claimsHardErr = nil
				return s, fetchReadDGDetailCmd(s.l1RPCURL, s.gameAddr, s.timeout, s.gen+1)
			}
		}
		// Clamp offset immediately so repeated keypresses past the
		// boundary do not accumulate — without this, holding `j` at
		// the bottom inflates s.offset arbitrarily and the operator
		// has to press `k` the same number of times before the view
		// starts moving.
		s = s.clampOffset()
	}
	return s, nil
}

// clampOffset constrains s.offset to [0, maxOff] given the current
// body height and viewport. Called from Update after any keystroke
// that moves the scroll position; View() still runs the same clamp
// defensively (snapshot may grow / shrink between Update and View
// when content is dynamic).
func (s readDisputeGameDetailScreen) clampOffset() readDisputeGameDetailScreen {
	bodyLines := strings.Count(s.renderBody(), "\n") + 1
	avail := s.height - 5
	if avail < 1 {
		avail = 1
	}
	maxOff := bodyLines - avail
	if maxOff < 0 {
		maxOff = 0
	}
	if s.offset > maxOff {
		s.offset = maxOff
	}
	if s.offset < 0 {
		s.offset = 0
	}
	return s
}

// --- styles ---

var (
	rdgDetailTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	rdgDetailContextStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	rdgDetailSectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	rdgDetailLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	rdgDetailValueStyle    = lipgloss.NewStyle()
	rdgDetailErrStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	rdgDetailStatusInProg  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")) // yellow
	rdgDetailStatusChall   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))  // red
	rdgDetailStatusDef     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")) // green
	rdgDetailHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
)

func (s readDisputeGameDetailScreen) View() string {
	var b strings.Builder
	b.WriteString(rdgDetailTitleStyle.Render(fmt.Sprintf("read / dispute-game / %s", s.gameAddr)))
	b.WriteString("\n")
	b.WriteString(rdgDetailContextStyle.Render(fmt.Sprintf("L1: %s", s.l1RPCURL)))
	b.WriteString("\n\n")

	body := s.renderBody()
	footer := rdgDetailHelpStyle.Render("j/k ↑↓ scroll · g/G top/bottom · PgDn/PgUp · r refresh · q back")

	// Scroll-window the body, but always show header + footer.
	if s.width > 0 && s.height > 0 {
		bodyLines := strings.Split(body, "\n")
		// 4 lines reserved for title+context+blank+footer (~4)
		avail := s.height - 5
		if avail < 1 {
			avail = 1
		}
		maxOff := len(bodyLines) - avail
		if maxOff < 0 {
			maxOff = 0
		}
		off := s.offset
		if off > maxOff {
			off = maxOff
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
		b.WriteString(strings.Join(visible, "\n"))
		b.WriteString("\n")
	} else {
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString(footer)
	return b.String()
}

func (s readDisputeGameDetailScreen) renderBody() string {
	if s.loading {
		return rdgDetailContextStyle.Render("⏳ fetching FaultDisputeGame snapshot (single batched RPC) ...")
	}
	if s.hardErr != nil {
		return rdgDetailErrStyle.Render(fmt.Sprintf("ERR fetching snapshot: %v", s.hardErr))
	}
	if s.snap == nil {
		return rdgDetailErrStyle.Render("ERR no snapshot data")
	}

	var b strings.Builder
	snap := s.snap

	// 1. Identity
	writeSection(&b, "1. Identity")
	writeRow(&b, "version", snap.Version, snap.Errors["version"])
	writeRowf(&b, "gameType", "%d %s", uint64(snap.GameType), gameTypeLabel(snap.GameType))
	writeRow(&b, "l2ChainId", bigOrEmpty(snap.L2ChainID), snap.Errors["l2ChainId"])
	writeRow(&b, "gameCreator", snap.GameCreator, snap.Errors["gameCreator"])

	// 2. Status & Timing
	writeSection(&b, "2. Status & Timing")
	writeStatusRow(&b, snap.Status, snap.Errors["status"])
	writeTimeRow(&b, "createdAt", snap.CreatedAt, snap.Errors["createdAt"])
	writeTimeRow(&b, "resolvedAt", snap.ResolvedAt, snap.Errors["resolvedAt"])

	// 3. Roles
	writeSection(&b, "3. Roles")
	writeRow(&b, "proposer", snap.Proposer, snap.Errors["proposer"])
	writeRow(&b, "challenger", snap.Challenger, snap.Errors["challenger"])
	writeBoolRow(&b, "l2BlockNumberChallenged", snap.L2BlockNumberChallenged, snap.Errors["l2BlockNumberChallenged"])
	writeRow(&b, "l2BlockNumberChallenger", snap.L2BlockNumberChallenger, snap.Errors["l2BlockNumberChallenger"])

	// 4. Output Root Claim
	writeSection(&b, "4. Output Root Claim")
	writeRow(&b, "rootClaim", snap.RootClaim, snap.Errors["gameData"])
	writeRow(&b, "l1Head", snap.L1Head, snap.Errors["l1Head"])
	writeRow(&b, "extraData", truncHex(snap.ExtraData, 80), snap.Errors["gameData"])
	writeRow(&b, "l2BlockNumber", bigOrEmpty(snap.L2BlockNumber), snap.Errors["l2BlockNumber"])
	writeRow(&b, "claimDataLen", bigOrEmpty(snap.ClaimDataLen), snap.Errors["claimDataLen"])

	// 5. Anchor
	writeSection(&b, "5. Anchor (Starting Point)")
	writeRow(&b, "anchorStateRegistry", snap.AnchorStateRegistry, snap.Errors["anchorStateRegistry"])
	writeRow(&b, "startingBlockNumber", bigOrEmpty(snap.StartingBlockNumber), snap.Errors["startingBlockNumber"])
	writeRow(&b, "startingRootHash", snap.StartingRootHash, snap.Errors["startingRootHash"])

	// 6. Execution VM
	writeSection(&b, "6. Execution VM")
	writeRow(&b, "absolutePrestate", snap.AbsolutePrestate, snap.Errors["absolutePrestate"])
	writeRow(&b, "vm (MIPS64)", snap.VM, snap.Errors["vm"])

	// 7. Bond Vault
	writeSection(&b, "7. Bond Vault")
	writeRow(&b, "weth (DelayedWETH)", snap.WETH, snap.Errors["weth"])

	// 8. Game Parameters
	writeSection(&b, "8. Game Parameters")
	writeRow(&b, "maxGameDepth", bigOrEmpty(snap.MaxGameDepth), snap.Errors["maxGameDepth"])
	writeRow(&b, "splitDepth", bigOrEmpty(snap.SplitDepth), snap.Errors["splitDepth"])
	writeDurationRow(&b, "maxClockDuration", snap.MaxClockDuration, snap.Errors["maxClockDuration"])
	writeDurationRow(&b, "clockExtension", snap.ClockExtension, snap.Errors["clockExtension"])

	// 9. Claim Data — the per-claim array indexed by claimDataLen.
	// Phase-2 batch (claimData(i) + getChallengerDuration(i) per i).
	s.renderClaimDataSection(&b)

	b.WriteString("\n")
	b.WriteString(rdgDetailContextStyle.Render(fmt.Sprintf("snapshot latency: %dms", snap.Latency.Milliseconds())))
	if s.claimsLatency > 0 {
		b.WriteString(rdgDetailContextStyle.Render(fmt.Sprintf(" · claimData latency: %dms", s.claimsLatency.Milliseconds())))
	}

	return b.String()
}

// renderClaimDataSection appends Section 9 to the detail body.
// Layout: one indented label-value block per claim (vertical) so each
// entry's seven fields stay readable without horizontal overflow.
// The challenger-duration line is colored amber when the clock is
// still running and red when it's already expired, so the operator
// can scan timing pressure across claims at a glance.
func (s readDisputeGameDetailScreen) renderClaimDataSection(b *strings.Builder) {
	switch {
	case s.snap == nil:
		return
	case s.snap.ClaimDataLen == nil:
		writeSection(b, "9. Claim Data")
		b.WriteString("  ")
		b.WriteString(rdgDetailErrStyle.Render("ERR claimDataLen unavailable"))
		b.WriteString("\n")
		return
	case s.snap.ClaimDataLen.Sign() == 0:
		writeSection(b, "9. Claim Data (0 entries)")
		b.WriteString("  ")
		b.WriteString(rdgDetailContextStyle.Render("(no claims submitted yet)"))
		b.WriteString("\n")
		return
	}
	writeSection(b, fmt.Sprintf("9. Claim Data (%s entries)", s.snap.ClaimDataLen.String()))
	if s.claimsLoading {
		b.WriteString("  ")
		b.WriteString(rdgDetailContextStyle.Render("⏳ fetching claimData[] + getChallengerDuration() ..."))
		b.WriteString("\n")
		return
	}
	if s.claimsHardErr != nil {
		b.WriteString("  ")
		b.WriteString(rdgDetailErrStyle.Render(fmt.Sprintf("ERR loading claim data: %v", s.claimsHardErr)))
		b.WriteString("\n")
		return
	}
	for i, cd := range s.claims {
		writeClaimEntry(b, cd, claimErrAt(s.claimErrs, i))
	}
}

func claimErrAt(errs []error, i int) error {
	if i >= 0 && i < len(errs) {
		return errs[i]
	}
	return nil
}

// writeClaimEntry renders one ClaimData as an indented label-value
// block. Special-case formatting:
//   - parentIndex == type(uint32).max → "—" (root claim has no parent)
//   - counteredBy == zero address → "—" (uncountered)
//   - challengerDuration == 0 → "expired" in red, else "<n>s" with
//     amber if under 1 hour remaining
func writeClaimEntry(b *strings.Builder, cd l1.ClaimData, err error) {
	header := rdgDetailSectionStyle.Render(fmt.Sprintf("  [%d]", cd.Index))
	b.WriteString(header)
	if err != nil {
		b.WriteString("  ")
		b.WriteString(rdgDetailErrStyle.Render(fmt.Sprintf("ERR %v", err)))
		b.WriteString("\n")
		return
	}
	b.WriteString("\n")
	writeClaimField(b, "parent", parentLabel(cd))
	writeClaimField(b, "claimant", cd.Claimant)
	writeClaimField(b, "counteredBy", counteredByLabel(cd))
	writeClaimField(b, "bond", bigOrEmpty(cd.Bond))
	writeClaimField(b, "claim", cd.Claim)
	writeClaimField(b, "position", bigOrEmpty(cd.Position))
	writeClaimField(b, "clock", bigOrEmpty(cd.Clock))
	writeClaimDurationField(b, cd.ChallengerDuration)
}

func writeClaimField(b *strings.Builder, label, value string) {
	b.WriteString("      ")
	b.WriteString(rdgDetailLabelStyle.Render(padRight(label, 14)))
	b.WriteString(" ")
	if value == "" {
		b.WriteString(rdgDetailContextStyle.Render("—"))
	} else {
		b.WriteString(rdgDetailValueStyle.Render(value))
	}
	b.WriteString("\n")
}

// writeClaimDurationField colors the remaining-time row so timing
// pressure is visible without reading the number: green = comfortable
// (>1h), amber = squeezed (<1h), red = expired.
func writeClaimDurationField(b *strings.Builder, seconds uint64) {
	b.WriteString("      ")
	b.WriteString(rdgDetailLabelStyle.Render(padRight("remaining", 14)))
	b.WriteString(" ")
	switch {
	case seconds == 0:
		b.WriteString(rdgDetailErrStyle.Render("expired (challenge clock = 0)"))
	case seconds < 3600:
		b.WriteString(rdgDetailStatusInProg.Render(fmt.Sprintf("%ds (%s)", seconds, time.Duration(seconds)*time.Second)))
	default:
		b.WriteString(rdgDetailValueStyle.Render(fmt.Sprintf("%ds (%s)", seconds, time.Duration(seconds)*time.Second)))
	}
	b.WriteString("\n")
}

func parentLabel(cd l1.ClaimData) string {
	if !cd.HasParent() {
		return "—" // root claim
	}
	return fmt.Sprintf("%d", cd.ParentIndex)
}

func counteredByLabel(cd l1.ClaimData) string {
	if !cd.IsCountered() {
		return "—"
	}
	return cd.CounteredBy
}

// --- row helpers ---

func writeSection(b *strings.Builder, title string) {
	b.WriteString("\n")
	b.WriteString(rdgDetailSectionStyle.Render(title))
	b.WriteString("\n")
}

const labelW = 24

func writeRow(b *strings.Builder, label, value string, err error) {
	b.WriteString("  ")
	b.WriteString(rdgDetailLabelStyle.Render(padRight(label, labelW)))
	b.WriteString(" ")
	if err != nil {
		b.WriteString(rdgDetailErrStyle.Render(fmt.Sprintf("ERR %v", err)))
	} else if value == "" {
		b.WriteString(rdgDetailContextStyle.Render("—"))
	} else {
		b.WriteString(rdgDetailValueStyle.Render(value))
	}
	b.WriteString("\n")
}

func writeRowf(b *strings.Builder, label, format string, args ...any) {
	writeRow(b, label, fmt.Sprintf(format, args...), nil)
}

func writeBoolRow(b *strings.Builder, label string, v bool, err error) {
	if err != nil {
		writeRow(b, label, "", err)
		return
	}
	writeRow(b, label, fmt.Sprintf("%t", v), nil)
}

func writeStatusRow(b *strings.Builder, st l1.GameStatus, err error) {
	b.WriteString("  ")
	b.WriteString(rdgDetailLabelStyle.Render(padRight("status", labelW)))
	b.WriteString(" ")
	if err != nil {
		b.WriteString(rdgDetailErrStyle.Render(fmt.Sprintf("ERR %v", err)))
		b.WriteString("\n")
		return
	}
	style := rdgDetailValueStyle
	switch st {
	case l1.GameStatusInProgress:
		style = rdgDetailStatusInProg
	case l1.GameStatusChallengerWins:
		style = rdgDetailStatusChall
	case l1.GameStatusDefenderWins:
		style = rdgDetailStatusDef
	}
	b.WriteString(style.Render(st.String()))
	b.WriteString("\n")
}

func writeTimeRow(b *strings.Builder, label string, unix uint64, err error) {
	if err != nil {
		writeRow(b, label, "", err)
		return
	}
	if unix == 0 {
		writeRow(b, label, "", nil)
		return
	}
	t := time.Unix(int64(unix), 0).UTC()
	writeRow(b, label, fmt.Sprintf("%s (%s)", t.Format("2006-01-02T15:04:05Z"), humanRelative(t)), nil)
}

func writeDurationRow(b *strings.Builder, label string, seconds uint64, err error) {
	if err != nil {
		writeRow(b, label, "", err)
		return
	}
	d := time.Duration(seconds) * time.Second
	writeRow(b, label, fmt.Sprintf("%d s (%s)", seconds, d.String()), nil)
}

// --- formatting helpers ---

func bigOrEmpty(n *big.Int) string {
	if n == nil {
		return ""
	}
	return n.String()
}

func gameTypeLabel(gt uint32) string {
	switch gt {
	case 0:
		return "(Cannon)"
	case 1:
		return "(Permissioned)"
	default:
		return ""
	}
}

// padRight lives in namespace_screen.go and is shared across screens.

func truncHex(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RunReadDisputeGameDetail launches the detail alt-screen as a
// standalone bubbletea program — the CLI-only entry point invoked by
// `op-ctl read dispute-game --address <0x...>`. Mirrors the
// RunReadDisputeGame contract: when the unified App pushes the screen
// via dispatch instead, it reuses newReadDisputeGameDetailScreen
// directly so two tea programs never contend for stdin/stdout.
func RunReadDisputeGameDetail(ctx context.Context, l1RPCURL, gameAddr string, timeout time.Duration) error {
	screen := newReadDisputeGameDetailScreen(l1RPCURL, gameAddr, timeout)
	_, err := tea.NewProgram(screen, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}
