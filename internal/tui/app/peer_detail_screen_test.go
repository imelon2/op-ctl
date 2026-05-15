package app

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/opnode"
)

func sampleConnectedEntry() *opnode.PeerEntry {
	return &opnode.PeerEntry{
		PeerID:        "16Uiu2HAkv3EG8nng3EgUcuuJh8h3qLwdXZ3ymyNyxTeu9cf6gm1X",
		NodeID:        "94abe87a36d17603c50e0d19de3a0fa446988159ba41ccc2dec722cd9c78b94f",
		UserAgent:     "optimism",
		ENR:           "",
		Addresses:     []string{"/ip4/172.17.0.8/tcp/9222/p2p/16Uiu2HAkv3EG8nng3EgUcuuJh8h3qLwdXZ3ymyNyxTeu9cf6gm1X"},
		Protocols:     []string{"/ipfs/ping/1.0.0", "/meshsub/1.0.0", "/meshsub/1.1.0", "/meshsub/1.2.0", "/opstack/req/payload_by_number/9999/0", "/floodsub/1.0.0", "/ipfs/id/1.0.0", "/ipfs/id/push/1.0.0"},
		Connectedness: 1,
		Direction:     1,
		Protected:     true,
		ChainID:       0,
		Latency:       1395709,
		GossipBlocks:  true,
		Scores: json.RawMessage(`{
  "gossip": {
    "total": 0,
    "blocks": {
      "timeInMesh": 0,
      "firstMessageDeliveries": 0,
      "meshMessageDeliveries": 0,
      "invalidMessageDeliveries": 0
    },
    "IPColocationFactor": 0,
    "behavioralPenalty": 0
  },
  "reqResp": {
    "validResponses": 0,
    "errorResponses": 0,
    "rejectedPayloads": 0
  }
}`),
	}
}

func TestPeerDetailRendersAllFields(t *testing.T) {
	entry := sampleConnectedEntry()
	s := newPeerDetailScreen(
		config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004"},
		entry.PeerID, "fullnode", entry, false,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 110, Height: 50})
	s = next.(peerDetailScreen)

	out := stripANSI(s.View())
	for _, want := range []string{
		"peer detail",
		"sequencer",
		"connected",
		"namespace name", "fullnode",
		"peerID", "16Uiu2HAkv3EG8nng3EgUcuuJh8h3qLwdXZ3ymyNyxTeu9cf6gm1X",
		"nodeID", "94abe87a36d17603c50e0d19de3a0fa446988159ba41ccc2dec722cd9c78b94f",
		"userAgent", "optimism",
		"ENR", "(empty)",
		"connectedness", "Connected",
		"direction", "Inbound",
		"protected", "true",
		"latency", "1.4ms",
		"addresses (1)",
		"/ip4/172.17.0.8/tcp/9222",
		"protocols (8)",
		"/meshsub/1.2.0",
		"/opstack/req/payload_by_number/9999/0",
		"scores",
		"gossip",
		"behavioralPenalty",
		"reqResp",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail view missing %q", want)
		}
	}
}

func TestPeerDetailQEmitsPopMsg(t *testing.T) {
	s := newPeerDetailScreen(config.Backend{Name: "x"}, "id", "", nil, false)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should emit popMsg")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("got %T, want popMsg", cmd())
	}
}

func TestPeerDetailBannedDegradesGracefully(t *testing.T) {
	s := newPeerDetailScreen(
		config.Backend{Name: "sequencer", ConsensusRPCURL: "http://1:1"},
		"16Uiu2HAm-banned", "blocked", nil, true,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s = next.(peerDetailScreen)
	out := stripANSI(s.View())
	for _, want := range []string{"banned", "blocked", "16Uiu2HAm-banned", "no further attributes"} {
		if !strings.Contains(out, want) {
			t.Errorf("banned detail missing %q:\n%s", want, out)
		}
	}
}

func TestAppPeerDetailMsgPushesScreen(t *testing.T) {
	cfg := stubConfig(t, "sequencer")
	a := New(stubCobra(), cfg, nil, t.TempDir(), 5_000_000_000)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	entry := sampleConnectedEntry()
	next, _ := a.Update(peerDetailMsg{
		backend: config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004"},
		peerID:  entry.PeerID,
		name:    "fullnode",
		entry:   entry,
	})
	a = next.(App)
	if _, ok := a.stack[len(a.stack)-1].(peerDetailScreen); !ok {
		t.Fatalf("expected peerDetailScreen on top, got %T", a.stack[len(a.stack)-1])
	}
}

func TestPeerDetailSnapshot(t *testing.T) {
	entry := sampleConnectedEntry()
	s := newPeerDetailScreen(
		config.Backend{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004"},
		entry.PeerID, "fullnode", entry, false,
	)
	next, _ := s.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	s = next.(peerDetailScreen)
	t.Logf("\n%s", stripANSI(s.View()))
}
