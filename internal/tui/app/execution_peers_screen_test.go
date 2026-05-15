package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/namespace"
)

func sampleAdminPeers() []elnode.AdminPeer {
	return []elnode.AdminPeer{
		{
			Enode: "enode://566b7d943c3ed6519c4476812ac9e88cabc394560416c877a68924d8178ed48339f1076067e05c671353a98cfb4ad3ff3594be7d6c4bd3515fa5fc66cbbaebb4@172.17.0.7:30303",
			ID:    "01afc118245a8d38f18cf2df1c2e3d8844b655079d4b372eb3150c0cf645dd9e",
			Name:  "reth/v2.0.0-eb4c15e/x86_64-unknown-linux-gnu",
			Caps:  []string{"eth/69", "eth/68", "eth/67", "eth/66"},
			Network: elnode.AdminPeerNetwork{
				LocalAddress:  "172.17.0.2:33530",
				RemoteAddress: "172.17.0.7:30303",
				Inbound:       false,
			},
		},
	}
}

func TestExecutionPeersHappyPath(t *testing.T) {
	idx := namespace.BuildIndex([]namespace.Entry{
		{Name: "fullnode", Execution: namespace.Execution{NodeID: "01afc118245a8d38f18cf2df1c2e3d8844b655079d4b372eb3150c0cf645dd9e"}},
	})
	s := newExecutionPeersScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		1, nil, sampleAdminPeers(), nil, idx,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s = next.(executionPeersScreen)
	out := stripANSI(s.View())
	for _, want := range []string{
		"sequencer", "http://127.0.0.1:4000",
		"net_peerCount", "1",
		"admin_peers", "1",
		"fullnode",
		"reth/v2.0.0",
		"out", "172.17.0.7:30303",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("happy view missing %q:\n%s", want, out)
		}
	}
}

func TestExecutionPeersAdminDisabledShowsHint(t *testing.T) {
	rpcErr := &elnode.RPCError{Code: -32601, Message: "the method admin_peers does not exist/is not available"}
	s := newExecutionPeersScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		3, nil, nil, rpcErr,
		namespace.BuildIndex(nil),
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s = next.(executionPeersScreen)
	out := stripANSI(s.View())
	for _, want := range []string{
		"net_peerCount", "3",
		"admin_peers", "admin disabled",
		"admin_peers unavailable",
		"--http.api eth,net,web3,admin",
		"op-reth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin-disabled view missing %q:\n%s", want, out)
		}
	}
}

func TestExecutionPeersGenericErrorPropagates(t *testing.T) {
	rpcErr := &elnode.RPCError{Code: -32000, Message: "internal error"}
	s := newExecutionPeersScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		0, nil, nil, rpcErr,
		namespace.BuildIndex(nil),
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s = next.(executionPeersScreen)
	out := stripANSI(s.View())
	if !strings.Contains(out, "admin_peers failed") {
		t.Errorf("missing 'admin_peers failed' header:\n%s", out)
	}
	if !strings.Contains(out, "internal error") {
		t.Errorf("missing inner error message:\n%s", out)
	}
	if strings.Contains(out, "admin namespace") && strings.Contains(out, "Enable") {
		t.Errorf("should NOT show admin-disabled hint for non-32601 error:\n%s", out)
	}
}

func TestExecutionPeersEnterEmitsDetailMsg(t *testing.T) {
	idx := namespace.BuildIndex(nil)
	s := newExecutionPeersScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		1, nil, sampleAdminPeers(), nil, idx,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s = next.(executionPeersScreen)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should emit a detail msg")
	}
	got, ok := cmd().(executionPeerDetailMsg)
	if !ok {
		t.Fatalf("got %T, want executionPeerDetailMsg", cmd())
	}
	if got.peer.ID != "01afc118245a8d38f18cf2df1c2e3d8844b655079d4b372eb3150c0cf645dd9e" {
		t.Errorf("wrong peer ID: %q", got.peer.ID)
	}
}

func TestExecutionPeerDetailRendersAllFields(t *testing.T) {
	s := newExecutionPeerDetailScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		"fullnode", sampleAdminPeers()[0],
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
	s = next.(executionPeerDetailScreen)
	out := stripANSI(s.View())
	for _, want := range []string{
		"execution peer detail",
		"sequencer", "http://127.0.0.1:4000",
		"outbound",
		"namespace name", "fullnode",
		"id", "01afc118245a8d38f18cf2df1c2e3d8844b655079d4b372eb3150c0cf645dd9e",
		"name", "reth/v2.0.0",
		"enode", "566b7d943c3ed6519c4476812ac9e88c",
		"network",
		"localAddress", "172.17.0.2:33530",
		"remoteAddress", "172.17.0.7:30303",
		"inbound", "false",
		"caps (4)", "eth/69", "eth/66",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q", want)
		}
	}
}

func TestExecutionPeersSnapshot(t *testing.T) {
	idx := namespace.BuildIndex([]namespace.Entry{
		{Name: "fullnode", Execution: namespace.Execution{NodeID: "01afc118245a8d38f18cf2df1c2e3d8844b655079d4b372eb3150c0cf645dd9e"}},
	})
	s := newExecutionPeersScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		1, nil, sampleAdminPeers(), nil, idx,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 110, Height: 20})
	s = next.(executionPeersScreen)
	t.Logf("\n[admin enabled]\n%s", stripANSI(s.View()))

	rpcErr := &elnode.RPCError{Code: -32601, Message: "the method admin_peers does not exist/is not available"}
	s2 := newExecutionPeersScreen(
		config.Backend{Name: "sequencer", ExecutionRPCURL: "http://127.0.0.1:4000"},
		3, nil, nil, rpcErr, namespace.BuildIndex(nil),
	)
	next2, _ := s2.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	s2 = next2.(executionPeersScreen)
	t.Logf("\n[admin disabled]\n%s", stripANSI(s2.View()))
}
