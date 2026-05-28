// Package theme is the single source of truth for op-ctl's TUI styling.
//
// Every screen used to declare its own prefixed copies of the same dozen
// lipgloss styles (pd*, txf*, epd*, dsc*, ...) with hard-coded ANSI codes.
// That made cross-screen consistency a matter of luck and meant a
// light-terminal fix had to be applied ~20 times. This package centralizes
// the palette, the derived styles, and a few render helpers (Footer,
// Spinner, ErrLine) so the look is defined once.
//
// Colors are lipgloss.AdaptiveColor: the Dark variant preserves the
// original bright-ANSI look on dark terminals; the Light variant swaps to
// darker tones so the muted grays and accents stay legible on a light
// background (the previous "240"/"244"/"250" greys were near-invisible on
// white).
package theme

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Palette — semantic colors. Dark mirrors the codebase's original ANSI
// choices; Light is a legibility swap for light terminals.
var (
	ColorPrimary = lipgloss.AdaptiveColor{Light: "4", Dark: "12"}    // titles, cursor, primary highlight
	ColorAccent  = lipgloss.AdaptiveColor{Light: "6", Dark: "14"}    // resolved names (peer / backend)
	ColorOK      = lipgloss.AdaptiveColor{Light: "2", Dark: "10"}    // success / connected / OK
	ColorWarn    = lipgloss.AdaptiveColor{Light: "3", Dark: "11"}    // warning / partial / queued
	ColorErr     = lipgloss.AdaptiveColor{Light: "1", Dark: "9"}     // error / failure / banned
	ColorOut     = lipgloss.AdaptiveColor{Light: "5", Dark: "13"}    // outbound direction
	ColorMuted   = lipgloss.AdaptiveColor{Light: "240", Dark: "244"} // labels, subtitles, help, urls
	ColorDim     = lipgloss.AdaptiveColor{Light: "245", Dark: "240"} // placeholders, borders, dim names
	ColorValue   = lipgloss.AdaptiveColor{Light: "236", Dark: "250"} // data values
	ColorSelBg   = lipgloss.AdaptiveColor{Light: "252", Dark: "237"} // selected-row background

	colorBadgeFg = lipgloss.AdaptiveColor{Light: "15", Dark: "0"}  // text on a colored badge
	colorSelFg   = lipgloss.AdaptiveColor{Light: "15", Dark: "15"} // text on a primary-bg selected cell
)

// Core text styles. Compose with .Padding(...)/.Width(...) at call sites
// for table cells — lipgloss styles are values, so this never mutates the
// shared definition.
var (
	Title    = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	Subtitle = lipgloss.NewStyle().Foreground(ColorMuted)
	Label    = lipgloss.NewStyle().Foreground(ColorMuted)
	Value    = lipgloss.NewStyle().Foreground(ColorValue)
	Name     = lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	Help     = lipgloss.NewStyle().Foreground(ColorMuted).Italic(true)
	Mute     = lipgloss.NewStyle().Foreground(ColorDim)
	Pending  = lipgloss.NewStyle().Foreground(ColorMuted).Italic(true)

	// Section is a sub-group heading inside a detail view. It was an
	// inconsistent mix of yellow / magenta / blue across screens; unified
	// here to yellow. ColHeader is a table column-header row (blue), and
	// Header is the green success-toned heading used above the read/list
	// tables. The three are distinct roles, each now used consistently.
	Section   = lipgloss.NewStyle().Bold(true).Foreground(ColorWarn)
	ColHeader = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	Header    = lipgloss.NewStyle().Bold(true).Foreground(ColorOK)

	// Status text (non-badge variants).
	OKText   = lipgloss.NewStyle().Foreground(ColorOK)
	WarnText = lipgloss.NewStyle().Foreground(ColorWarn)
	ErrText  = lipgloss.NewStyle().Foreground(ColorErr)
	ErrTitle = lipgloss.NewStyle().Bold(true).Foreground(ColorErr)

	// Selection.
	Cursor       = lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)
	SelectedRow  = lipgloss.NewStyle().Background(ColorSelBg)
	SelectedCell = lipgloss.NewStyle().Bold(true).Foreground(colorSelFg).Background(ColorPrimary)

	// Peer direction.
	DirIn  = lipgloss.NewStyle().Foreground(ColorPrimary)
	DirOut = lipgloss.NewStyle().Foreground(ColorOut)
)

// Badges — bold text on a saturated background. Build a custom-colored
// badge (e.g. inbound/outbound) with BadgeBase.Background(c).
var (
	BadgeBase = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(colorBadgeFg)
	OKBadge   = BadgeBase.Background(ColorOK)
	WarnBadge = BadgeBase.Background(ColorWarn)
	ErrBadge  = BadgeBase.Background(ColorErr)
)

// Borders.
var (
	Border = lipgloss.RoundedBorder()
	// HeaderBox frames a screen header with a dim border; ErrorBox frames
	// a full-screen error with a red border.
	HeaderBox = lipgloss.NewStyle().Border(Border).BorderForeground(ColorDim).Padding(0, 1)
	ErrorBox  = lipgloss.NewStyle().Border(Border).BorderForeground(ColorErr).Padding(1, 2)
)

// ---------- footer ----------

// Key is one hint shown in a screen footer: the key chord and what it does.
type Key struct{ Keys, Desc string }

// Common footer hints. Screens compose these through Footer so the wording
// and order stay identical everywhere (previously each screen hand-wrote
// its own "j/k move" vs "navigate" vs "scroll" string).
var (
	KeyNav        = Key{"↑/↓ j/k", "navigate"}
	KeyScroll     = Key{"↑/↓ j/k", "scroll"}
	KeyTopBottom  = Key{"g/G", "top/bottom"}
	KeyOpenDetail = Key{"⏎", "detail"}
	KeyRefresh    = Key{"r", "refresh"}
	KeyBack       = Key{"q", "back"}
	KeyQuit       = Key{"q/esc", "quit"}
)

// Footer renders a help line from hints joined by " · ", in the muted
// italic Help style. Zero-value hints are skipped, so callers can splice
// in an optional KeyRefresh without branching on the string.
func Footer(keys ...Key) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k.Keys == "" && k.Desc == "" {
			continue
		}
		parts = append(parts, strings.TrimSpace(k.Keys+" "+k.Desc))
	}
	return Help.Render(strings.Join(parts, " · "))
}

// ---------- loading ----------

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner returns the braille spinner glyph for the given animation frame.
// Screens that track a frame counter (e.g. the namespace probe view) use
// this so every animated spinner is identical.
func Spinner(frame int) string {
	return spinnerFrames[frame%len(spinnerFrames)]
}

// ---------- errors ----------

// ErrLine renders an inline soft error as "ERR <msg>" in the error color —
// the shared form for per-row / per-section failures that should not
// replace the whole screen.
func ErrLine(msg string) string {
	return ErrText.Render("ERR " + msg)
}
