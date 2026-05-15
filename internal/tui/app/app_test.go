package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
)

// stubCobra builds a minimal cobra tree mirroring production:
// op-ctl > {list, namespace, p2p > peer > {consensus, execution}}.
// Tests that drive the menu navigate through the same shape.
func stubCobra() *cobra.Command {
	root := &cobra.Command{Use: "op-ctl"}
	root.AddCommand(&cobra.Command{Use: "list", Short: "show ns",
		Run: func(cmd *cobra.Command, _ []string) {}})
	root.AddCommand(&cobra.Command{Use: "namespace", Short: "snapshot",
		Run: func(cmd *cobra.Command, _ []string) {}})
	p2p := &cobra.Command{Use: "p2p", Short: "p2p"}
	peer := &cobra.Command{Use: "peer", Short: "per-peer queries"}
	peer.AddCommand(&cobra.Command{Use: "consensus", Short: "peers",
		Run: func(cmd *cobra.Command, _ []string) {}})
	peer.AddCommand(&cobra.Command{Use: "execution", Short: "exec peers",
		Run: func(cmd *cobra.Command, _ []string) {}})
	p2p.AddCommand(peer)
	root.AddCommand(p2p)
	return root
}

// stubConfig builds a Config containing two named backends in a known
// order. We construct it via TOML decode in real code, but for tests
// we just populate the fields directly via a tiny shim — Config has
// no public ctor that takes a slice, but the helper here uses an
// in-package approach by calling config.Load on a temp file would be
// overkill; instead we exploit the fact that BackendList only needs
// the keyOrder + Backends map, and we can't access them. So instead
// the test just relies on the config package: this function is
// declared in a separate _test.go to keep it close to the test that
// needs it.
func stubConfig(t *testing.T, names ...string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for _, n := range names {
		b.WriteString("[backends.")
		b.WriteString(n)
		b.WriteString("]\n")
		b.WriteString("consensus_rpc_url = \"http://127.0.0.1:1\"\n")
		b.WriteString("execution_rpc_url = \"http://127.0.0.1:2\"\n\n")
	}
	path := dir + "/config.toml"
	if err := writeFile(path, b.String()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeFile(path, content string) error {
	f, err := openFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

func feed(t *testing.T, a App, msgs ...tea.Msg) App {
	t.Helper()
	for _, m := range msgs {
		next, cmd := a.Update(m)
		a = next.(App)
		// Also follow up on the cmd: drain it once so message-emitting
		// cmds like `menu.SelectedMsg` propagate to the app's Update.
		if cmd != nil {
			out := cmd()
			if out != nil {
				next, _ = a.Update(out)
				a = next.(App)
			}
		}
	}
	return a
}

func TestAppRootMenuRendersCommandNames(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	out := stripANSI(a.View())
	for _, want := range []string{"list", "namespace", "p2p"} {
		if !strings.Contains(out, want) {
			t.Errorf("root menu missing %q\n%s", want, out)
		}
	}
}

func TestAppNavigatesIntoP2PSubmenu(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	// Drill: root → p2p (down·down·enter) → peer (enter, only child).
	a = feed(t, a, key("j"), key("j"), key("enter"), key("enter"))

	if got := len(a.stack); got != 3 {
		t.Fatalf("stack depth after p2p > peer: got %d, want 3", got)
	}
	out := stripANSI(a.View())
	if !strings.Contains(out, "consensus") || !strings.Contains(out, "execution") {
		t.Errorf("peer submenu missing leaves:\n%s", out)
	}
}

func TestAppPopOnEscReturnsToParent(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})
	a = feed(t, a, key("j"), key("j"), key("enter"), key("enter")) // root → p2p → peer
	if got := len(a.stack); got != 3 {
		t.Fatalf("expected depth 3 after p2p>peer, got %d", got)
	}
	a = feed(t, a, key("esc")) // pop peer
	if got := len(a.stack); got != 2 {
		t.Fatalf("after first esc stack should be 2 (root, p2p), got %d", got)
	}
	a = feed(t, a, key("esc")) // pop p2p
	if got := len(a.stack); got != 1 {
		t.Fatalf("after second esc stack should be 1, got %d", got)
	}
	out := stripANSI(a.View())
	if !strings.Contains(out, "list") || !strings.Contains(out, "p2p") {
		t.Errorf("root menu not restored:\n%s", out)
	}
}

func TestAppEscAtRootQuits(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})
	// Inject popMsg directly (simulating menu.CanceledMsg arrival).
	next, cmd := a.Update(popMsg{})
	if cmd == nil {
		t.Fatalf("popMsg at root should return tea.Quit cmd")
	}
	_ = next
}

func TestAppSelectingConsensusOpensBackendMenu(t *testing.T) {
	cfg := stubConfig(t, "sequencer", "fullnode")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})
	// root → p2p → peer → consensus.
	a = feed(t, a, key("j"), key("j"), key("enter"), key("enter"), key("enter"))

	if got := len(a.stack); got != 4 {
		t.Fatalf("expected depth 4 (root, p2p, peer, backend picker), got %d", got)
	}
	if _, ok := a.stack[3].(backendMenu); !ok {
		t.Fatalf("top of stack should be backendMenu, got %T", a.stack[3])
	}
	out := stripANSI(a.View())
	for _, want := range []string{"sequencer", "fullnode", "pick backend"} {
		if !strings.Contains(out, want) {
			t.Errorf("backend picker missing %q:\n%s", want, out)
		}
	}
}

func TestAppPeersFetchedPushesPeersScreen(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})
	// Push a loadingScreen to simulate the in-flight RPC state.
	a = a.push(newLoadingScreen("..."))
	preDepth := len(a.stack)

	dump := &opnode.PeerDump{
		TotalConnected: 0,
		Peers:          map[string]opnode.PeerEntry{},
	}
	next, _ := a.Update(peersFetchedMsg{
		backend: config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:1"},
		dump:    dump,
	})
	a = next.(App)

	// loading should be popped + peersScreen pushed: net depth same.
	if got := len(a.stack); got != preDepth {
		t.Fatalf("after peersFetched: depth %d, want %d", got, preDepth)
	}
	if _, ok := a.stack[len(a.stack)-1].(peersScreen); !ok {
		t.Fatalf("top of stack: got %T, want peersScreen", a.stack[len(a.stack)-1])
	}
}

func TestAppPeersFetchedErrorPushesErrScreen(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})
	a = a.push(newLoadingScreen("..."))

	next, _ := a.Update(peersFetchedMsg{err: errString("rpc kaboom")})
	a = next.(App)

	if _, ok := a.stack[len(a.stack)-1].(errScreen); !ok {
		t.Fatalf("expected errScreen on top, got %T", a.stack[len(a.stack)-1])
	}
}

func TestAppListLoadedPushesTextScreen(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	dir := t.TempDir()
	if err := writeFile(dir+"/sequencer.json",
		`{"name":"sequencer","consensus":{"peer_id":"x"},"execution":{}}`); err != nil {
		t.Fatal(err)
	}
	a := New(stubCobra(), cfg, nil, dir, 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	next := runListCmd(dir)()
	final, _ := a.Update(next)
	a = final.(App)

	ts, ok := a.stack[len(a.stack)-1].(textScreen)
	if !ok {
		t.Fatalf("expected textScreen, got %T", a.stack[len(a.stack)-1])
	}
	if !strings.Contains(ts.body, "sequencer") {
		t.Errorf("textScreen body missing backend name:\n%s", ts.body)
	}
}

// ---------- helpers ----------

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	in := false
	for _, r := range s {
		if r == 0x1b {
			in = true
			continue
		}
		if in {
			if r >= '@' && r <= '~' {
				in = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

type errString string

func (e errString) Error() string { return string(e) }

// openFile is a tiny os.Create wrapper so the file-using helpers above
// don't have to import "os" themselves (keeping the test file's
// import block tight).
func openFile(path string) (interface {
	WriteString(string) (int, error)
	Close() error
}, error) {
	return openFileImpl(path)
}

// stubNamespace ensures the namespace package's Index type is usable
// from test data (compile-time check; no runtime call).
var _ = namespace.BuildIndex
