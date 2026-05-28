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
	"op-ctl/internal/tui/theme"
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

	// Right-pane (Claim Data list) state. cursor is the highlighted
	// claim index; expanded[i] toggles between the compact one-line
	// row and the multi-line per-field block for claim i. Survives
	// across refreshes so the operator does not lose their context
	// after pressing `r`.
	cursor   int
	expanded map[uint64]bool

	// Independent scroll offsets per pane: left scrolls the static
	// sections, right scrolls the (possibly much longer when claims
	// are expanded) claim list. ← / → switch which pane responds to
	// j/k/g/G/PgDn/PgUp.
	focus       detailPane
	leftOffset  int
	rightOffset int

	width, height int
}

// detailPane names the two-column layout's active scroll target.
// paneRight is the default focus on first paint because the claim
// list is where most operator interaction lives; ← flips to paneLeft
// so they can browse the static sections at length.
type detailPane int

const (
	paneRight detailPane = iota
	paneLeft
)

func newReadDisputeGameDetailScreen(l1RPCURL, gameAddr string, timeout time.Duration) readDisputeGameDetailScreen {
	return readDisputeGameDetailScreen{
		l1RPCURL: l1RPCURL,
		gameAddr: gameAddr,
		timeout:  timeout,
		loading:  true,
		expanded: map[uint64]bool{},
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
		k := m.String()
		switch k {
		case "q", "esc", "ctrl+c":
			return s, func() tea.Msg { return popMsg{} }
		case "left", "h":
			s.focus = paneLeft
		case "right", "l":
			s.focus = paneRight
		case "tab":
			// Single-key toggle for operators who prefer not to reach
			// for the arrow keys.
			if s.focus == paneLeft {
				s.focus = paneRight
			} else {
				s.focus = paneLeft
			}
		case "j", "down":
			if s.focus == paneLeft {
				s.leftOffset++
				s = s.clampLeftOffset()
			} else {
				if s.cursor < len(s.claims)-1 {
					s.cursor++
				}
				s = s.ensureCursorVisible()
			}
		case "k", "up":
			if s.focus == paneLeft {
				s.leftOffset--
				s = s.clampLeftOffset()
			} else {
				if s.cursor > 0 {
					s.cursor--
				}
				s = s.ensureCursorVisible()
			}
		case "g", "home":
			if s.focus == paneLeft {
				s.leftOffset = 0
			} else {
				s.cursor = 0
				s.rightOffset = 0
			}
		case "G", "end":
			if s.focus == paneLeft {
				s.leftOffset = 1 << 30
				s = s.clampLeftOffset()
			} else {
				if len(s.claims) > 0 {
					s.cursor = len(s.claims) - 1
				}
				s = s.ensureCursorVisible()
			}
		case "pgdown", "ctrl+d":
			if s.focus == paneLeft {
				s.leftOffset += halfPage(s.height)
				s = s.clampLeftOffset()
			} else {
				s.rightOffset += halfPage(s.height)
				s = s.clampRightOffset()
			}
		case "pgup", "ctrl+u", "b":
			if s.focus == paneLeft {
				s.leftOffset -= halfPage(s.height)
				s = s.clampLeftOffset()
			} else {
				s.rightOffset -= halfPage(s.height)
				s = s.clampRightOffset()
			}
		case "enter", " ":
			// Toggle the highlighted claim only when focus is on the
			// right pane — keeps left-pane scrolling pure.
			if s.focus == paneRight && s.cursor >= 0 && s.cursor < len(s.claims) {
				idx := s.claims[s.cursor].Index
				if s.expanded == nil {
					s.expanded = map[uint64]bool{}
				}
				s.expanded[idx] = !s.expanded[idx]
				s = s.ensureCursorVisible()
			}
		case "r":
			if !s.loading {
				s.loading = true
				s.claimsLoading = false
				s.claims = nil
				s.claimErrs = nil
				s.claimsHardErr = nil
				return s, fetchReadDGDetailCmd(s.l1RPCURL, s.gameAddr, s.timeout, s.gen+1)
			}
		}
	}
	return s, nil
}

// clampLeftOffset / clampRightOffset constrain the per-pane scroll
// offsets to [0, maxOff] where maxOff = paneLines - viewport. They're
// called from Update after any keystroke that moves the relevant
// offset so repeated keypresses at the bounds don't accumulate.
func (s readDisputeGameDetailScreen) clampLeftOffset() readDisputeGameDetailScreen {
	left := s.renderLeftPanel()
	leftH := strings.Count(left, "\n") + 1
	s.leftOffset = clampInt(s.leftOffset, 0, max0(leftH-s.viewportHeight()))
	return s
}

func (s readDisputeGameDetailScreen) clampRightOffset() readDisputeGameDetailScreen {
	right, _ := s.renderRightPanel()
	rightH := strings.Count(right, "\n") + 1
	s.rightOffset = clampInt(s.rightOffset, 0, max0(rightH-s.viewportHeight()))
	return s
}

// viewportHeight is the per-pane vertical space available for body
// rows. With the L1↔summary divider added and the in-pane tab row
// removed, the reserved chrome adds up to 10 lines:
//
//  1. title / breadcrumb
//  2. L1 URL
//  3. ─── divider
//  4. version · l2ChainId · gameType
//  5. absolutePrestate
//  6. status · createdAt · resolvedAt
//  7. blank separator
//  8. pane top border
//  9. pane bottom border
//  10. footer
//
// View() uses the returned value directly as the body slice length —
// no extra subtraction because the panes no longer carry an internal
// tab row.
func (s readDisputeGameDetailScreen) viewportHeight() int {
	h := s.height - 10
	if h < 1 {
		return 1
	}
	return h
}

// --- styles ---

var (
	// Both panes get a 4-sided NormalBorder; only BorderForeground
	// changes between focused and idle states (set per-render in
	// View). Inner Padding(0,1) keeps content from sitting flush
	// against the border.
	rdgDetailPaneBaseStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(theme.ColorDim).
				Padding(0, 1)

	// Active-pane border color — the only visual cue for focus now
	// that the tab markers are gone. The primary accent is the same
	// hue used for selected list rows so the operator can build a
	// consistent "this is interactive" vocabulary across screens.
	rdgDetailPaneActiveBorderColor = theme.ColorPrimary
	rdgDetailPaneIdleBorderColor   = theme.ColorDim
)

// minLeftPanelWidth is the floor used before the first window-size
// message arrives (or as a sanity guard against zero-width content).
// The actual width is computed per render from the longest visible
// line on the left, so bytes32 hashes and long Solidity labels stay
// fully visible.
const minLeftPanelWidth = 30

func (s readDisputeGameDetailScreen) View() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render(fmt.Sprintf("read / dispute-game / %s", s.gameAddr)))
	b.WriteString("\n")
	b.WriteString(theme.Subtitle.Render(fmt.Sprintf("L1: %s", s.l1RPCURL)))
	b.WriteString("\n")
	b.WriteString(s.renderHeaderDivider())
	b.WriteString("\n")
	b.WriteString(s.renderHeaderSummary())
	b.WriteString("\n")
	b.WriteString(s.renderHeaderPrestate())
	b.WriteString("\n")
	b.WriteString(s.renderHeaderStatus())
	b.WriteString("\n\n")

	// Footer announces the focused pane so the operator never wonders
	// which side ← / → / j / k / PgDn are acting on.
	focusLabel := "right"
	if s.focus == paneLeft {
		focusLabel = "left"
	}
	footer := theme.Help.Render(
		fmt.Sprintf("focus: %s · ← → switch · j/k ↑↓ scroll · ⏎/space toggle (right) · g/G first/last · PgDn/PgUp · r refresh · q back", focusLabel),
	)

	// Each pane scrolls independently via its own offset. Panes carry
	// no internal tab/title row anymore — focus is communicated by
	// the surrounding border color alone, and section titles inside
	// each pane (Roles, Output Root Claim, Anchor, Parameters on the
	// left; Claim Data on the right) carry the labelling job.
	leftLines, rightLines := s.buildPanelLines()
	avail := s.viewportHeight()

	leftSlice := sliceWithPad(leftLines, s.leftOffset, avail)
	rightSlice := sliceWithPad(rightLines, s.rightOffset, avail)

	// Both panes are 4-sided bordered cells. Active pane's border
	// glows in the bright accent color; idle pane stays muted.
	leftW := leftPaneWidth(leftLines)
	leftStyle := rdgDetailPaneBaseStyle.Width(leftW)
	rightStyle := rdgDetailPaneBaseStyle
	if s.focus == paneLeft {
		leftStyle = leftStyle.BorderForeground(rdgDetailPaneActiveBorderColor)
		rightStyle = rightStyle.BorderForeground(rdgDetailPaneIdleBorderColor)
	} else {
		leftStyle = leftStyle.BorderForeground(rdgDetailPaneIdleBorderColor)
		rightStyle = rightStyle.BorderForeground(rdgDetailPaneActiveBorderColor)
	}

	body := lipgloss.JoinHorizontal(
		lipgloss.Top,
		leftStyle.Render(strings.Join(leftSlice, "\n")),
		rightStyle.Render(strings.Join(rightSlice, "\n")),
	)
	b.WriteString(body)
	b.WriteString("\n")
	b.WriteString(footer)
	return b.String()
}

// renderHeaderDivider returns a thin horizontal rule used to split
// the "context" header (title + L1 URL) from the per-snapshot
// metadata rows (version / prestate / status). Rendered in the same
// muted color as the rest of the breadcrumb so it reads as a
// separator, not as content. Fixed width keeps the layout stable on
// narrow terminals; an adaptive width would force every snapshot
// fetch to invalidate the cached lipgloss render of the divider for
// no real gain.
const rdgDetailDividerWidth = 60

func (s readDisputeGameDetailScreen) renderHeaderDivider() string {
	return theme.Subtitle.Render(strings.Repeat("─", rdgDetailDividerWidth))
}

// renderHeaderSummary builds the "version · l2ChainId · gameType"
// inline summary row that lives directly under the L1 URL line in
// the header. Each pill turns into an ERR-styled cell when its
// underlying call failed so the operator sees which field broke
// without scrolling the data pane.
func (s readDisputeGameDetailScreen) renderHeaderSummary() string {
	if s.loading || s.snap == nil {
		return theme.Subtitle.Render("⏳ loading identity ...")
	}
	snap := s.snap
	pill := func(label, value string, err error) string {
		if err != nil {
			return theme.ErrTitle.Render(label + " ERR " + err.Error())
		}
		return theme.Label.Render(label+" ") + theme.Value.Render(value)
	}
	parts := []string{
		pill("version", snap.Version, snap.Errors["version"]),
		pill("l2ChainId", bigOrEmpty(snap.L2ChainID), snap.Errors["l2ChainId"]),
		pill("gameType", fmt.Sprintf("%d %s", snap.GameType, gameTypeLabel(snap.GameType)), snap.Errors["gameData"]),
	}
	return strings.Join(parts, theme.Subtitle.Render("  ·  "))
}

// renderHeaderPrestate builds the "absolutePrestate 0x..." row of
// the header. Kept on its own line because the 0x...bytes32 value is
// long enough that combining it with the summary row would overflow
// narrow terminals.
func (s readDisputeGameDetailScreen) renderHeaderPrestate() string {
	if s.loading || s.snap == nil {
		return ""
	}
	snap := s.snap
	if e := snap.Errors["absolutePrestate"]; e != nil {
		return theme.ErrTitle.Render("absolutePrestate ERR " + e.Error())
	}
	return theme.Label.Render("absolutePrestate ") + theme.Value.Render(snap.AbsolutePrestate)
}

// renderHeaderStatus builds the inline status row in the header:
// `status XX · createdAt YYYY-MM-DD…(Nm) · resolvedAt YYYY-MM-DD…(Nm)`
// Each pill carries the same color contract as the rest of the
// header: muted label + bright value, or a red ERR cell when its
// underlying call failed.
func (s readDisputeGameDetailScreen) renderHeaderStatus() string {
	if s.loading || s.snap == nil {
		return ""
	}
	snap := s.snap
	parts := []string{
		headerStatusPill(snap.Status, snap.Errors["status"]),
		headerTimePill("createdAt", snap.CreatedAt, snap.Errors["createdAt"]),
		headerTimePill("resolvedAt", snap.ResolvedAt, snap.Errors["resolvedAt"]),
	}
	return strings.Join(parts, theme.Subtitle.Render("  ·  "))
}

// headerStatusPill renders the colored enum cell for the status
// field. Color coding mirrors writeStatusRow's left-pane styling so
// the operator's mental map carries between the two surfaces.
func headerStatusPill(st l1.GameStatus, err error) string {
	if err != nil {
		return theme.ErrTitle.Render("status ERR " + err.Error())
	}
	style := theme.Value
	switch st {
	case l1.GameStatusInProgress:
		style = theme.WarnText.Bold(true)
	case l1.GameStatusChallengerWins:
		style = theme.ErrText.Bold(true)
	case l1.GameStatusDefenderWins:
		style = theme.OKText.Bold(true)
	}
	return theme.Label.Render("status ") + style.Render(st.String())
}

// headerTimePill renders a `createdAt / resolvedAt` cell as a
// muted-label + UTC-iso + relative-suffix value, or "—" when the
// timestamp is zero (i.e. the chain hasn't recorded it yet).
func headerTimePill(label string, unix uint64, err error) string {
	if err != nil {
		return theme.ErrTitle.Render(label + " ERR " + err.Error())
	}
	if unix == 0 {
		return theme.Label.Render(label+" ") + theme.Subtitle.Render("—")
	}
	t := time.Unix(int64(unix), 0).UTC()
	return theme.Label.Render(label+" ") + theme.Value.Render(t.Format("2006-01-02T15:04:05Z")+" ("+humanRelative(t)+")")
}

// sliceWithPad returns lines[off : off+avail] (clamped to the slice
// bounds) with empty-string padding when the visible window extends
// past the end of the content. Keeps the panel height stable across
// scroll positions so the footer never bounces.
func sliceWithPad(lines []string, off, avail int) []string {
	if avail < 1 {
		return nil
	}
	maxOff := max0(len(lines) - avail)
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	end := off + avail
	if end > len(lines) {
		end = len(lines)
	}
	out := append([]string{}, lines[off:end]...)
	for len(out) < avail {
		out = append(out, "")
	}
	return out
}

// buildPanelLines renders both panes as parallel line slices. No
// truncation is applied — the panel width is sized to the longest
// left-pane line at render time (see View) so values like bytes32
// hashes and long Solidity labels remain fully visible.
func (s readDisputeGameDetailScreen) buildPanelLines() (leftLines, rightLines []string) {
	left := s.renderLeftPanel()
	right, _ := s.renderRightPanel()
	leftLines = strings.Split(left, "\n")
	rightLines = strings.Split(right, "\n")
	return leftLines, rightLines
}

// leftPanelFurnitureWidth accounts for the columns lipgloss reserves
// inside the left pane style: BorderLeft(1) + PaddingLeft(1) +
// PaddingRight(1) + BorderRight(1) = 4 columns. lipgloss
// Style.Width(n) treats `n` as the total cell (content + padding +
// border), so we add furniture to the measured content width when
// telling lipgloss how wide to make the pane.
const leftPanelFurnitureWidth = 4

// leftPaneWidth returns the dynamic width the left pane should
// occupy. It scans every left line for the widest one (rendered
// width, ANSI-aware) and pads with the lipgloss border + padding
// overhead. minLeftPanelWidth is a sanity floor used before the first
// content arrives.
func leftPaneWidth(leftLines []string) int {
	w := 0
	for _, line := range leftLines {
		if v := lipgloss.Width(line); v > w {
			w = v
		}
	}
	if w < minLeftPanelWidth {
		w = minLeftPanelWidth
	}
	return w + leftPanelFurnitureWidth
}

// cursorLineInRight returns the logical line index of the highlighted
// claim row inside the right pane, or -1 if no row is selectable.
// Mirrors renderRightPanel's layout so ensureCursorVisible can scroll
// to the right spot without re-rendering both panes redundantly.
func (s readDisputeGameDetailScreen) cursorLineInRight() int {
	_, line := s.renderRightPanel()
	return line
}

// ensureCursorVisible adjusts s.offset so the currently-highlighted
// claim row sits inside the viewport. Called from Update only after
// keystrokes that move the cursor (j/k/g/G/enter). PgDn/PgUp do NOT
// invoke this — the operator may intentionally page past the cursor
// to read static left-pane sections.
func (s readDisputeGameDetailScreen) ensureCursorVisible() readDisputeGameDetailScreen {
	avail := s.viewportHeight()
	cursorLine := s.cursorLineInRight()
	if cursorLine < 0 {
		return s
	}
	if cursorLine < s.rightOffset {
		s.rightOffset = cursorLine
	}
	if cursorLine >= s.rightOffset+avail {
		s.rightOffset = cursorLine - avail + 1
	}
	// Final clamp so a snapshot shrink (refresh with fewer claims)
	// can't leave rightOffset stranded past the end of the new content.
	return s.clampRightOffset()
}

// max0 lives in read_dispute_game_screen.go and is shared package-wide.

// clampInt constrains n to [lo, hi].
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// renderLeftPanel emits the 5 data sections that sit under the HEADER:
//
//   - Status & Timing
//   - Roles
//   - Output Root Claim   (extraData child: l2BlockNumber)
//   - Anchor (Starting Point)
//   - Parameters          (vm, weth, depth/clock knobs)
//
// Identity-tier fields (version, gameType, l2ChainId, absolutePrestate)
// live on the HEADER row above the panes — see View() — so they stay
// visible while the pane scrolls. Claim-array data has its own
// dedicated right pane.
func (s readDisputeGameDetailScreen) renderLeftPanel() string {
	if s.loading {
		return theme.Subtitle.Render("⏳ fetching snapshot ...")
	}
	if s.hardErr != nil {
		return theme.ErrTitle.Render(fmt.Sprintf("ERR fetching snapshot: %v", s.hardErr))
	}
	if s.snap == nil {
		return theme.ErrTitle.Render("ERR no snapshot data")
	}

	var b strings.Builder
	snap := s.snap

	// Roles
	writeSection(&b, "Roles")
	writeRow(&b, "gameCreator", snap.GameCreator, snap.Errors["gameCreator"])
	writeRow(&b, "proposer", snap.Proposer, snap.Errors["proposer"])
	writeRow(&b, "challenger", snap.Challenger, snap.Errors["challenger"])

	// Output Root Claim — l2BlockNumber is the decoded form of
	// extraData, so it renders as a child row directly beneath it.
	writeSection(&b, "Output Root Claim")
	writeRow(&b, "rootClaim", snap.RootClaim, snap.Errors["gameData"])
	writeRow(&b, "l1Head", snap.L1Head, snap.Errors["l1Head"])
	writeRow(&b, "extraData", truncHex(snap.ExtraData, 80), snap.Errors["gameData"])
	writeChildRow(&b, "l2BlockNumber", bigOrEmpty(snap.L2BlockNumber), snap.Errors["l2BlockNumber"])

	// Anchor (Starting Point)
	writeSection(&b, "Anchor (Starting Point)")
	writeRow(&b, "anchorStateRegistry", snap.AnchorStateRegistry, snap.Errors["anchorStateRegistry"])
	writeRow(&b, "startingBlockNumber", bigOrEmpty(snap.StartingBlockNumber), snap.Errors["startingBlockNumber"])
	writeRow(&b, "startingRootHash", snap.StartingRootHash, snap.Errors["startingRootHash"])

	// Parameters
	writeSection(&b, "Parameters")
	writeRow(&b, "vm (MIPS64)", snap.VM, snap.Errors["vm"])
	writeRow(&b, "weth (DelayedWETH)", snap.WETH, snap.Errors["weth"])
	writeRow(&b, "maxGameDepth", bigOrEmpty(snap.MaxGameDepth), snap.Errors["maxGameDepth"])
	writeRow(&b, "splitDepth", bigOrEmpty(snap.SplitDepth), snap.Errors["splitDepth"])
	writeDurationRow(&b, "maxClockDuration", snap.MaxClockDuration, snap.Errors["maxClockDuration"])
	writeDurationRow(&b, "clockExtension", snap.ClockExtension, snap.Errors["clockExtension"])

	b.WriteString("\n")
	latencies := []string{fmt.Sprintf("snapshot %dms", snap.Latency.Milliseconds())}
	if s.claimsLatency > 0 {
		latencies = append(latencies, fmt.Sprintf("claim %dms", s.claimsLatency.Milliseconds()))
	}
	b.WriteString(theme.Subtitle.Render(strings.Join(latencies, " · ")))

	return b.String()
}

// writeChildRow renders an indented label-value row, e.g. for the
// l2BlockNumber decoded form sitting under extraData. Uses a tree
// "└─" prefix so the parent/child relationship reads naturally in
// terminals with box-drawing support.
func writeChildRow(b *strings.Builder, label, value string, err error) {
	b.WriteString("    └─ ")
	const childLabelW = labelW - 5 // account for the "  └─ " indent
	b.WriteString(theme.Label.Render(padRight(label, childLabelW)))
	b.WriteString(" ")
	if err != nil {
		b.WriteString(theme.ErrTitle.Render(fmt.Sprintf("ERR %v", err)))
	} else if value == "" {
		b.WriteString(theme.Subtitle.Render("—"))
	} else {
		b.WriteString(theme.Value.Render(value))
	}
	b.WriteString("\n")
}

// renderRightPanel produces the per-claim list on the right side. Each
// claim is one collapsed row by default; pressing enter on it toggles
// an expansion that inlines all seven ClaimData fields plus the
// challenger duration. Returns the logical line index of the cursor
// row so View() can auto-scroll the shared viewport to keep the
// highlighted row visible.
//
// The right pane always has a header line and (when applicable) an
// empty / loading / error state, so it never collapses to zero height
// even when the snapshot has no claims.
func (s readDisputeGameDetailScreen) renderRightPanel() (string, int) {
	var b strings.Builder
	cursorLine := -1

	b.WriteString(theme.Section.Render("Claim Data"))
	b.WriteString("\n")

	switch {
	case s.snap == nil || s.snap.ClaimDataLen == nil:
		if s.loading {
			b.WriteString(theme.Subtitle.Render("  (waiting for snapshot)"))
		} else if s.hardErr != nil {
			b.WriteString(theme.ErrTitle.Render("  ERR snapshot unavailable"))
		} else {
			b.WriteString(theme.ErrTitle.Render("  ERR claimDataLen unavailable"))
		}
		b.WriteString("\n")
		return b.String(), cursorLine
	case s.snap.ClaimDataLen.Sign() == 0:
		b.WriteString(theme.Subtitle.Render("  (no claims submitted yet)"))
		b.WriteString("\n")
		return b.String(), cursorLine
	}

	b.WriteString(theme.Subtitle.Render(fmt.Sprintf("(%s entries · ⏎ toggle)", s.snap.ClaimDataLen.String())))
	b.WriteString("\n")

	if s.claimsLoading {
		b.WriteString(theme.Subtitle.Render("⏳ fetching claimData[] + getChallengerDuration() ..."))
		b.WriteString("\n")
		return b.String(), cursorLine
	}
	if s.claimsHardErr != nil {
		b.WriteString(theme.ErrTitle.Render(fmt.Sprintf("ERR loading claim data: %v", s.claimsHardErr)))
		b.WriteString("\n")
		return b.String(), cursorLine
	}

	// Per-claim block. Each row tracks its own start line so we can
	// hand the View() auto-scroll the cursor's logical line.
	for i, cd := range s.claims {
		isCursor := i == s.cursor
		expanded := s.expanded != nil && s.expanded[cd.Index]

		if isCursor {
			cursorLine = strings.Count(b.String(), "\n") // 0-based line of this row
		}

		writeClaimSummary(&b, cd, claimErrAt(s.claimErrs, i), isCursor, expanded)
		if expanded && s.claimErrs == nil || (i < len(s.claimErrs) && s.claimErrs[i] == nil && expanded) {
			writeClaimExpansion(&b, cd)
		}
	}
	return b.String(), cursorLine
}

// writeClaimSummary renders the single compact row for one claim:
//
//	`▶ [i] 0xabcd…1234 · bond N · rem Nh`
//
// or, when expanded:
//
//	`▼ [i] 0xabcd…1234 · bond N · rem Nh`
//
// The cursor row is fully-highlighted (background + bold) so it stays
// visible alongside the left pane.
func writeClaimSummary(b *strings.Builder, cd l1.ClaimData, err error, isCursor, isExpanded bool) {
	disclosure := "▶"
	if isExpanded {
		disclosure = "▼"
	}
	cursorMark := "  "
	if isCursor {
		cursorMark = "▸ "
	}
	// On per-row error, replace the value cells with the error so the
	// summary still occupies one line — keeps the list navigation sane.
	var summary string
	if err != nil {
		summary = fmt.Sprintf("%s %s [%d] ERR %v", cursorMark, disclosure, cd.Index, err)
	} else {
		summary = fmt.Sprintf("%s %s [%d] %s · bond %s · rem %s",
			cursorMark,
			disclosure,
			cd.Index,
			shortAddr(cd.Claimant),
			shortBigOrDash(cd.Bond),
			shortRemaining(cd.ChallengerDuration),
		)
	}
	if isCursor {
		summary = rdgListSelectStyle.Render(strings.TrimRight(summary, " "))
	}
	b.WriteString(summary)
	b.WriteString("\n")
}

// writeClaimExpansion appends the eight-field detail block under an
// expanded claim. Indentation matches the cursor gutter so the field
// labels sit a few columns to the right of the summary.
func writeClaimExpansion(b *strings.Builder, cd l1.ClaimData) {
	writeClaimField(b, "parent", parentLabel(cd))
	writeClaimField(b, "claimant", cd.Claimant)
	writeClaimField(b, "counteredBy", counteredByLabel(cd))
	writeClaimField(b, "bond", bigOrEmpty(cd.Bond))
	writeClaimField(b, "claim", cd.Claim)
	writeClaimField(b, "position", bigOrEmpty(cd.Position))
	writeClaimField(b, "clock", bigOrEmpty(cd.Clock))
	writeClaimDurationField(b, cd.ChallengerDuration)
}

// shortAddr returns "0xABCD…1234" — first 4 + last 4 hex chars after
// the 0x prefix. Used for compact summaries; the expanded block still
// shows the full address.
func shortAddr(a string) string {
	if len(a) < 10 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

// shortBigOrDash formats a uint128/uint256 for the summary row:
// returns "—" for nil or zero, otherwise the full decimal string
// (operators care about exact bond amounts).
func shortBigOrDash(n *big.Int) string {
	if n == nil || n.Sign() == 0 {
		return "—"
	}
	return n.String()
}

// shortRemaining formats a Duration (seconds) for the summary cell:
// "expired" (0), "Ns" (<60), "Nm" (<3600), "Nh" (<86400), "Nd"
// (otherwise). Color is applied at the cursor-row level so this just
// returns text.
func shortRemaining(seconds uint64) string {
	switch {
	case seconds == 0:
		return "expired"
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}

// renderClaimDataSection appends Section 9 to the detail body.
// Layout: one indented label-value block per claim (vertical) so each
// entry's seven fields stay readable without horizontal overflow.
// The challenger-duration line is colored amber when the clock is
// still running and red when it's already expired, so the operator
// can scan timing pressure across claims at a glance.
func claimErrAt(errs []error, i int) error {
	if i >= 0 && i < len(errs) {
		return errs[i]
	}
	return nil
}

func writeClaimField(b *strings.Builder, label, value string) {
	b.WriteString("      ")
	b.WriteString(theme.Label.Render(padRight(label, 14)))
	b.WriteString(" ")
	if value == "" {
		b.WriteString(theme.Subtitle.Render("—"))
	} else {
		b.WriteString(theme.Value.Render(value))
	}
	b.WriteString("\n")
}

// writeClaimDurationField colors the remaining-time row so timing
// pressure is visible without reading the number: green = comfortable
// (>1h), amber = squeezed (<1h), red = expired.
func writeClaimDurationField(b *strings.Builder, seconds uint64) {
	b.WriteString("      ")
	b.WriteString(theme.Label.Render(padRight("remaining", 14)))
	b.WriteString(" ")
	switch {
	case seconds == 0:
		b.WriteString(theme.ErrTitle.Render("expired (challenge clock = 0)"))
	case seconds < 3600:
		b.WriteString(theme.WarnText.Bold(true).Render(fmt.Sprintf("%ds (%s)", seconds, time.Duration(seconds)*time.Second)))
	default:
		b.WriteString(theme.Value.Render(fmt.Sprintf("%ds (%s)", seconds, time.Duration(seconds)*time.Second)))
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

// writeSection emits a section header with a leading blank line as a
// separator between sections. The leading blank is skipped when the
// buffer is empty so the first section in a pane sits flush against
// the top border instead of starting with an awkward empty row.
func writeSection(b *strings.Builder, title string) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(theme.Section.Render(title))
	b.WriteString("\n")
}

const labelW = 24

func writeRow(b *strings.Builder, label, value string, err error) {
	b.WriteString("  ")
	b.WriteString(theme.Label.Render(padRight(label, labelW)))
	b.WriteString(" ")
	if err != nil {
		b.WriteString(theme.ErrTitle.Render(fmt.Sprintf("ERR %v", err)))
	} else if value == "" {
		b.WriteString(theme.Subtitle.Render("—"))
	} else {
		b.WriteString(theme.Value.Render(value))
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
	b.WriteString(theme.Label.Render(padRight("status", labelW)))
	b.WriteString(" ")
	if err != nil {
		b.WriteString(theme.ErrTitle.Render(fmt.Sprintf("ERR %v", err)))
		b.WriteString("\n")
		return
	}
	style := theme.Value
	switch st {
	case l1.GameStatusInProgress:
		style = theme.WarnText.Bold(true)
	case l1.GameStatusChallengerWins:
		style = theme.ErrText.Bold(true)
	case l1.GameStatusDefenderWins:
		style = theme.OKText.Bold(true)
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
