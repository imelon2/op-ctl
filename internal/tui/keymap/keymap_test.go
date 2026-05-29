package keymap

import (
	"math/big"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBinding_Matches(t *testing.T) {
	cases := []struct {
		name    string
		binding Binding
		key     string
		want    bool
	}{
		{"down matches j", Down, "j", true},
		{"down matches down arrow", Down, "down", true},
		{"down does not match k", Down, "k", false},
		{"refresh matches r", Refresh, "r", true},
		{"refresh does not match R", Refresh, "R", false},
		{"back matches q", Back, "q", true},
		{"back matches esc", Back, "esc", true},
		{"timecycle matches t", TimeCycle, "t", true},
		{"unitcycle matches u", UnitCycle, "u", true},
		{"navigate has empty keys (footer only)", Navigate, "anything", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.binding.Matches(keyMsg(tc.key))
			if got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestBinding_Hint(t *testing.T) {
	h := Refresh.Hint()
	if h.Keys != "r" || h.Desc != "refresh" {
		t.Errorf("Refresh.Hint() = %+v, want {r refresh}", h)
	}
}

// TestStandard_KeysUnique guards against accidentally binding the
// same physical key to two semantic actions. Navigate / Scroll are
// excluded because they have empty Keys by design (footer-only).
func TestStandard_KeysUnique(t *testing.T) {
	registry := []struct {
		name string
		b    Binding
	}{
		{"Refresh", Refresh},
		{"Back", Back},
		{"Up", Up},
		{"Down", Down},
		{"Enter", Enter},
		{"PageNext", PageNext},
		{"PagePrev", PagePrev},
		{"Top", Top},
		{"Bottom", Bottom},
		{"TimeCycle", TimeCycle},
		{"UnitCycle", UnitCycle},
	}
	seen := map[string]string{}
	for _, entry := range registry {
		for _, k := range entry.b.Keys {
			if owner, dup := seen[k]; dup {
				t.Errorf("key %q is bound to both %s and %s", k, owner, entry.name)
			}
			seen[k] = entry.name
		}
	}
}

func TestFooter_RendersBindings(t *testing.T) {
	out := Footer(Refresh, Back)
	for _, want := range []string{"r", "refresh", "q", "back"} {
		if !contains(out, want) {
			t.Errorf("Footer output missing %q in %q", want, out)
		}
	}
}

func TestTimeMode_Cycle(t *testing.T) {
	m := TimeModeUTC
	m = m.Next()
	if m != TimeModeLocal {
		t.Errorf("UTC.Next() = %v, want Local", m)
	}
	m = m.Next()
	if m != TimeModeUnix {
		t.Errorf("Local.Next() = %v, want Unix", m)
	}
	m = m.Next()
	if m != TimeModeUTC {
		t.Errorf("Unix.Next() = %v, want UTC (wrap)", m)
	}
}

func TestTimeMode_Format(t *testing.T) {
	const ts int64 = 1700000000 // 2023-11-14T22:13:20Z
	if got := TimeModeUTC.Format(ts); got != "2023-11-14T22:13:20Z" {
		t.Errorf("UTC.Format = %q", got)
	}
	if got := TimeModeUnix.Format(ts); got != "1700000000" {
		t.Errorf("Unix.Format = %q", got)
	}
	if got := TimeModeUTC.Format(0); got != "" {
		t.Errorf("zero ts should yield empty, got %q", got)
	}
}

func TestTimeMode_HeaderLabel(t *testing.T) {
	cases := map[TimeMode]string{
		TimeModeUTC:   "time (utc)",
		TimeModeLocal: "time (local)",
		TimeModeUnix:  "time (unix)",
	}
	for m, want := range cases {
		if got := m.HeaderLabel(); got != want {
			t.Errorf("HeaderLabel(%v) = %q, want %q", m, got, want)
		}
	}
}

func TestFeeUnit_Cycle(t *testing.T) {
	u := UnitWei
	u = u.Next()
	if u != UnitGwei {
		t.Errorf("Wei.Next() = %v, want Gwei", u)
	}
	u = u.Next()
	if u != UnitEth {
		t.Errorf("Gwei.Next() = %v, want Eth", u)
	}
	u = u.Next()
	if u != UnitWei {
		t.Errorf("Eth.Next() = %v, want Wei (wrap)", u)
	}
}

func TestFeeUnit_DecimalsAndString(t *testing.T) {
	for _, tc := range []struct {
		u    FeeUnit
		dec  int
		name string
	}{
		{UnitWei, 0, "wei"},
		{UnitGwei, 9, "gwei"},
		{UnitEth, 18, "eth"},
	} {
		if tc.u.Decimals() != tc.dec {
			t.Errorf("%v.Decimals() = %d, want %d", tc.u, tc.u.Decimals(), tc.dec)
		}
		if tc.u.String() != tc.name {
			t.Errorf("%v.String() = %q, want %q", tc.u, tc.u.String(), tc.name)
		}
	}
}

func TestFeeUnit_Format(t *testing.T) {
	one := big.NewInt(1)
	gigaWei := new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil) // 1e9 wei = 1 gwei
	weiPerEth := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	cases := []struct {
		u    FeeUnit
		v    *big.Int
		want string
	}{
		{UnitWei, one, "1"},
		{UnitWei, gigaWei, "1000000000"},
		{UnitGwei, gigaWei, "1"},
		{UnitGwei, big.NewInt(1500000000), "1.5"},
		{UnitEth, weiPerEth, "1"},
		{UnitEth, new(big.Int).Mul(weiPerEth, big.NewInt(3)), "3"},
		{UnitWei, nil, ""},
	}
	for i, tc := range cases {
		if got := tc.u.Format(tc.v); got != tc.want {
			t.Errorf("case %d: %v.Format(%v) = %q, want %q", i, tc.u, tc.v, got, tc.want)
		}
	}
}

// keyMsg constructs a tea.KeyMsg whose String() equals s. Mirrors how
// bubbletea normalizes navigation key chords ("down", "pgup", "ctrl+d", ...).
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
