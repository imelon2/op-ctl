package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
)

// sampleENRs are real-world records captured from a live op-node
// running with discovery enabled. Used as fixtures so the screen
// tests don't depend on a reachable node.
func sampleENRs() []string {
	return []string{
		"enr:-Le4QGm63WzmqIA-h7c8zhksJNY0ZB4FupyPSpipUJ0RL94OCtlw9C2kKMK9u9DjOLuyWsazmN3DXlNRwIc1RRHaXeEgh2F0dG5ldHOIAQAAAAAAAICDY2djBIRldGgykIyfYv4GAAAA__________-CaWSCdjSCaXCEV_LTtolzZWNwMjU2azGhAgwnS1o7Z4r3nrlisqxY7mdElodKfwnfGZShp98Qasd9g3RjcIIjKYN1ZHCCIyk",
		"enr:-KO4QL1WQoxv-oy_lT5CYkJGTz45GYfI3JgawGdM8TQQ1I6iGVNTQR9tey77W6Z7RkQoZZWByw8xcBEfY-J5WlKtOm-GAZfOkhyTg2V0aMfGhJ8HaoiAgmlkgnY0gmlwhDmBVgeJc2VjcDI1NmsxoQNUUJt369jHtkoifBx1UFc2bDmEFRWP80vMK_m6vOvohIRzbmFwwIN0Y3CCdl-DdWRwgnZf",
		"enr:-MK4QC6Mj0khVgY27ovqyTZJA5jS_Hlh92VOaqver_eoL6mPQbDzHNlPc2COnNxnGpMQmeABzTodgCGrTbxOkLs9fMmCDrmHYXR0bmV0c4gBAAAAAAAAgINjZ2MIhGV0aDKQjJ9i_gYAAAD__________4JpZIJ2NIJpcIRJz1tzg25mZIQAAAAAiXNlY3AyNTZrMaED39zFRCHm6KQ7beNFoAx-Wi4Rmr2Z_xsaYzIAJAQmYrqDdGNwgiMog3VkcIIjKA",
		"enr:-Iu4QH11CuFakeHashForTestingOnly1234567890abcdefghijklmnopqrs",
	}
}

func TestDiscoveryHappyPathWithMatch(t *testing.T) {
	idx := namespace.BuildIndex([]namespace.Entry{
		// Pretend the second ENR belongs to our `archive` backend
		{Name: "archive", Consensus: namespace.Consensus{
			ENR: "enr:-KO4QL1WQoxv-oy_lT5CYkJGTz45GYfI3JgawGdM8TQQ1I6iGVNTQR9tey77W6Z7RkQoZZWByw8xcBEfY-J5WlKtOm-GAZfOkhyTg2V0aMfGhJ8HaoiAgmlkgnY0gmlwhDmBVgeJc2VjcDI1NmsxoQNUUJt369jHtkoifBx1UFc2bDmEFRWP80vMK_m6vOvohIRzbmFwwIN0Y3CCdl-DdWRwgnZf",
		}},
	})
	s := newDiscoveryConsensusScreen(
		config.Backend{Name: "ingress", ConsensusRPCURL: "http://localhost:8004"},
		sampleENRs(), nil, idx,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s = next.(discoveryConsensusScreen)
	out := stripANSI(s.View())
	for _, want := range []string{
		"ingress", "http://localhost:8004",
		"total entries", "4",
		"matched", "1",
		"archive",
		"enr:-Le4QGm63WzmqIA",
		"enr:-KO4QL1WQoxv",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("happy view missing %q:\n%s", want, out)
		}
	}
	// The matched row should sort to the top (named first).
	if s.rows[0].name != "archive" {
		t.Errorf("first row should be matched 'archive', got %q", s.rows[0].name)
	}
}

func TestDiscoveryDisabledShowsHint(t *testing.T) {
	rpcErr := &opnode.RPCError{Code: -32000, Message: "discovery disabled"}
	s := newDiscoveryConsensusScreen(
		config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004"},
		nil, rpcErr, namespace.BuildIndex(nil),
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s = next.(discoveryConsensusScreen)
	out := stripANSI(s.View())
	for _, want := range []string{
		"sequencer", "http://127.0.0.1:4004",
		"discovery disabled",
		"--p2p.no-discovery",
		"rpc error -32000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("disabled view missing %q:\n%s", want, out)
		}
	}
}

func TestDiscoveryEnterEmitsDetailMsg(t *testing.T) {
	s := newDiscoveryConsensusScreen(
		config.Backend{Name: "ingress", ConsensusRPCURL: "http://localhost:8004"},
		sampleENRs(), nil, namespace.BuildIndex(nil),
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s = next.(discoveryConsensusScreen)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should emit a detail msg")
	}
	got, ok := cmd().(discoveryDetailMsg)
	if !ok {
		t.Fatalf("got %T, want discoveryDetailMsg", cmd())
	}
	if !strings.HasPrefix(got.enr, "enr:") {
		t.Errorf("detail msg should carry an ENR, got %q", got.enr)
	}
}

func TestDiscoveryDetailRendersENR(t *testing.T) {
	enr := sampleENRs()[0]
	s := newDiscoveryDetailScreen(
		config.Backend{Name: "ingress", ConsensusRPCURL: "http://localhost:8004"},
		"archive", enr,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	s = next.(discoveryDetailScreen)
	out := stripANSI(s.View())
	for _, want := range []string{
		"discovery entry",
		"archive",
		"namespace name",
		"enr",
		"enr:-Le4QGm63WzmqIA",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
}

func TestDiscoveryDetailUnknownNameDegrades(t *testing.T) {
	s := newDiscoveryDetailScreen(
		config.Backend{Name: "ingress", ConsensusRPCURL: "http://localhost:8004"},
		"", sampleENRs()[0],
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	s = next.(discoveryDetailScreen)
	out := stripANSI(s.View())
	if !strings.Contains(out, "(unknown") {
		t.Errorf("detail should show '(unknown ...)' when name is empty:\n%s", out)
	}
}

func TestIsDiscoveryDisabled(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&opnode.RPCError{Code: -32000, Message: "discovery disabled"}, true},
		{&opnode.RPCError{Code: -32000, Message: "Discovery is Disabled now"}, true},
		{&opnode.RPCError{Code: -32601, Message: "method not found"}, false},
		{&opnode.RPCError{Code: -32000, Message: "node syncing"}, false},
		{nil, false},
	}
	for i, c := range cases {
		if got := opnode.IsDiscoveryDisabled(c.err); got != c.want {
			t.Errorf("case %d (%v): got %v, want %v", i, c.err, got, c.want)
		}
	}
}

func TestDiscoverySnapshot(t *testing.T) {
	idx := namespace.BuildIndex([]namespace.Entry{
		{Name: "archive", Consensus: namespace.Consensus{ENR: sampleENRs()[1]}},
	})
	s := newDiscoveryConsensusScreen(
		config.Backend{Name: "ingress", ConsensusRPCURL: "http://localhost:8004"},
		sampleENRs(), nil, idx,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 110, Height: 16})
	s = next.(discoveryConsensusScreen)
	t.Logf("\n[discovery enabled]\n%s", stripANSI(s.View()))

	rpcErr := &opnode.RPCError{Code: -32000, Message: "discovery disabled"}
	s2 := newDiscoveryConsensusScreen(
		config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004"},
		nil, rpcErr, namespace.BuildIndex(nil),
	)
	next2, _ := s2.Update(tea.WindowSizeMsg{Width: 110, Height: 25})
	s2 = next2.(discoveryConsensusScreen)
	t.Logf("\n[discovery disabled]\n%s", stripANSI(s2.View()))

	s3 := newDiscoveryDetailScreen(
		config.Backend{Name: "ingress", ConsensusRPCURL: "http://localhost:8004"},
		"archive", sampleENRs()[1],
	)
	next3, _ := s3.Update(tea.WindowSizeMsg{Width: 80, Height: 25})
	s3 = next3.(discoveryDetailScreen)
	t.Logf("\n[discovery detail]\n%s", stripANSI(s3.View()))
}
