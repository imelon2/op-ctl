# TUI Interactive Keys

Op-ctl's bubbletea screens share one **declarative key registry** at
`internal/tui/keymap` so every new screen reuses the same key chords,
the same footer format, and the same wei/time cycle state machines.
A new screen that wants `r` for refresh, `t` to cycle the time
column, or `u` to cycle the fee unit just picks the matching binding
out of this package instead of re-implementing it.

## Package

| Path | Role |
|------|------|
| `internal/tui/keymap/keymap.go` | `Binding` type + standard bindings + `Footer()` |
| `internal/tui/keymap/timemode.go` | `TimeMode` (UTC / Local / Unix) cycler + formatter |
| `internal/tui/keymap/feeunit.go` | `FeeUnit` (Wei / Gwei / Eth) cycler + formatter |
| `internal/tui/theme/theme.go` | `theme.Key` + `theme.Footer` (existing footer chrome) |

## Standard Bindings

Each binding pairs `Keys` (what the dispatcher matches against
`tea.KeyMsg.String()`) with `Display` (footer label) and `Desc`
(description).

| Binding | Keys | Display | Desc |
|---------|------|---------|------|
| `keymap.Refresh` | `r` | `r` | refresh |
| `keymap.Back` | `q`, `esc` | `q` | back |
| `keymap.Quit` | `q`, `esc`, `ctrl+c` | `q/esc` | quit |
| `keymap.Up` | `k`, `up` | `↑/k` | up |
| `keymap.Down` | `j`, `down` | `↓/j` | down |
| `keymap.Enter` | `enter` | `⏎` | open |
| `keymap.PageNext` | `pgdown`, `ctrl+d`, `n` | `PgDn` | next page |
| `keymap.PagePrev` | `pgup`, `ctrl+u`, `b`, `p` | `PgUp` | prev page |
| `keymap.Top` | `g`, `home` | `g` | top |
| `keymap.Bottom` | `G`, `end` | `G` | bottom |
| `keymap.TimeCycle` | `t` | `t` | time (utc/local/unix) |
| `keymap.UnitCycle` | `u` | `u` | unit (wei/gwei/eth) |
| `keymap.Navigate` | _(footer only)_ | `↑↓/jk` | navigate |
| `keymap.Scroll` | _(footer only)_ | `↑↓/jk` | scroll |

`Navigate` and `Scroll` have empty `Keys` on purpose — they exist
for the footer when a screen already binds `Up`/`Down` individually
and doesn't want the dispatcher to match the same chord twice.

`PageNext` / `PagePrev` deliberately do **not** include `right`/`l`,
`left`/`h`, or space because some screens (e.g.
`read_dispute_game_detail_screen.go`) bind those to section
navigation or toggle. A screen that wants the extra aliases for
paging can add them inline (see the `read_dispute_game_screen.go`
list for the pattern).

## Cycle State Types

### `keymap.TimeMode`

Backs the `t` key — cycles a unix-seconds timestamp column.

| Value | `String()` | `HeaderLabel()` | `Format(unix)` example |
|-------|------------|-----------------|------------------------|
| `TimeModeUTC` (default) | `utc` | `time (utc)` | `2026-05-29T10:14:32Z` |
| `TimeModeLocal` | `local` | `time (local)` | `2026-05-29 19:14:32` |
| `TimeModeUnix` | `unix` | `time (unix)` | `1748528072` |

`m.Next()` advances UTC → Local → Unix → UTC. `m.Format(0)` returns
the empty string so missing values stay clean instead of printing
`1970-01-01`.

### `keymap.FeeUnit`

Backs the `u` key — cycles a `*big.Int` wei value.

| Value | `String()` | `Decimals()` | `Format(1e9 wei)` |
|-------|------------|--------------|-------------------|
| `UnitWei` (default) | `wei` | 0 | `1000000000` |
| `UnitGwei` | `gwei` | 9 | `1` |
| `UnitEth` | `eth` | 18 | `0.000000001` |

`u.Next()` wraps Wei → Gwei → Eth → Wei. `u.Format(nil)` returns the
empty string. Trailing zeros are stripped from the fractional part.

## How to add a key to a new screen

1. Match in `Update` via `keymap.X.Matches(m)`:

```go
case tea.KeyMsg:
    switch {
    case keymap.Back.Matches(m):
        return s, func() tea.Msg { return popMsg{} }
    case keymap.Refresh.Matches(m):
        return s, s.refreshCmd()
    case keymap.TimeCycle.Matches(m):
        s.tsMode = s.tsMode.Next()
        return s, nil
    }
```

2. Compose the footer in `View`:

```go
b.WriteString(keymap.Footer(
    keymap.Navigate, keymap.Enter, keymap.PageNext, keymap.PagePrev,
    keymap.Refresh, keymap.TimeCycle, keymap.Back,
))
```

3. Use cycle types for stateful toggles:

```go
type myScreen struct {
    tsMode keymap.TimeMode
    unit   keymap.FeeUnit
}
// ...
header := s.tsMode.HeaderLabel()  // "time (utc)" etc.
cell   := s.tsMode.Format(row.TimeStamp)
gwei   := s.unit.Format(weiAmount)
```

## Canonical usage examples

| Screen | Bindings used |
|--------|---------------|
| `internal/tui/app/read_batch_screen.go` | `Refresh`, `TimeCycle`, `Back`, `Up`, `Down`, `PageNext`, `PagePrev`, `Enter`, `Navigate` (footer) |
| `internal/tui/app/read_batch_detail_screen.go` | `Back`, `Up`, `Down`, `Scroll` (footer) |
| `internal/tui/app/read_network_fee_screen.go` | `Refresh`, `UnitCycle`, `Back`, `Up`, `Down`, `Top`, `Bottom`, `PageNext`, `PagePrev`, `Scroll` (footer) |
| `internal/tui/app/read_dispute_game_screen.go` | `Refresh`, `Back`, `Up`, `Down`, `Top`, `Bottom`, `PageNext`, `PagePrev`, `Enter` |
| `internal/tui/app/read_dispute_game_detail_screen.go` | `Back` (rest still uses screen-local cases for tab + section nav) |

## Adding a new standard binding

Edit `internal/tui/keymap/keymap.go`, append the new `Binding` to
the `var (...)` block, and add it to the `TestStandard_KeysUnique`
registry in `keymap_test.go`. The uniqueness test guards against
silently binding the same chord to two semantic actions.

## When to inline a key instead

The footer-only bindings (`Navigate`, `Scroll`) and the keymap's
exclusion of `right/l`, `left/h`, and space from PageNext exist
because a binding's keys must be **uniform across every screen** to
deserve a place in `keymap.go`. Anything that means "section next"
on one screen and "page next" on another belongs inline in that
screen's switch, not in the shared registry.
