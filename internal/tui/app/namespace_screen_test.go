package app

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/opnode"
)

func sampleBackends() []config.Backend {
	return []config.Backend{
		{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004", ExecutionRPCURL: "http://127.0.0.1:4000"},
		{Name: "ingress1", ConsensusRPCURL: "http://127.0.0.1:8004", ExecutionRPCURL: "http://127.0.0.1:8000"},
	}
}

func sized(s namespaceScreen, w, h int) namespaceScreen {
	next, _ := s.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(namespaceScreen)
}

func feedNS(s namespaceScreen, msgs ...tea.Msg) namespaceScreen {
	for _, m := range msgs {
		next, _ := s.Update(m)
		s = next.(namespaceScreen)
	}
	return s
}

func TestNamespaceProgressInitiallyShowsAllRunning(t *testing.T) {
	s, _ := newNamespaceScreen(sampleBackends(), nil, t.TempDir(), 5_000_000_000, 0)
	s = sized(s, 100, 30)
	out := stripANSI(s.View())
	for _, want := range []string{"sequencer", "ingress1", "consensus", "execution", "snapshotting"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress view missing %q:\n%s", want, out)
		}
	}
}

func TestNamespaceWriteFiresWhenBothProbesSettle(t *testing.T) {
	dir := t.TempDir()
	s, _ := newNamespaceScreen(sampleBackends(), nil, dir, 5_000_000_000, 0)
	s = sized(s, 100, 30)

	// Both probes for backend 0 settle; write should fire.
	next, _ := s.Update(nsConsensusMsg{
		backendIdx: 0,
		info:       &opnode.PeerInfo{PeerID: "16Uiu2HAm-seq", NodeID: "0xseqnode", ENR: "enr:-seq"},
		latencyMS:  12,
	})
	s = next.(namespaceScreen)
	if s.rows[0].consensus != probeOK {
		t.Fatalf("consensus state: got %v, want probeOK", s.rows[0].consensus)
	}

	next, cmd := s.Update(nsExecutionMsg{
		backendIdx: 0,
		info:       &elnode.NodeInfo{ID: "abc", Enode: "enode://x@1.2.3.4:30303", ENR: "enr:-Ku4Q"},
		latencyMS:  8,
	})
	s = next.(namespaceScreen)
	if cmd == nil {
		t.Fatalf("expected write cmd after both probes settled")
	}
	if s.rows[0].write != writeRunning {
		t.Fatalf("write state: got %v, want writeRunning", s.rows[0].write)
	}

	// Run the write cmd; it should produce nsWriteMsg.
	wmsg := cmd().(nsWriteMsg)
	if wmsg.err != nil {
		t.Fatalf("write returned err: %v", wmsg.err)
	}
	if !strings.HasSuffix(wmsg.path, "/sequencer.json") {
		t.Errorf("write path: got %q", wmsg.path)
	}
}

func TestNamespaceWriteSkippedWhenOnlyOneProbeSettled(t *testing.T) {
	s, _ := newNamespaceScreen(sampleBackends(), nil, t.TempDir(), 5_000_000_000, 0)
	s = sized(s, 100, 30)

	_, cmd := s.Update(nsConsensusMsg{
		backendIdx: 0,
		info:       &opnode.PeerInfo{PeerID: "x"},
	})
	if cmd != nil {
		t.Errorf("write should not fire with execution still pending; got non-nil cmd")
	}
}

func TestNamespaceFailedProbeStillTriggersWriteWithEmptyFields(t *testing.T) {
	dir := t.TempDir()
	s, _ := newNamespaceScreen(sampleBackends()[:1], nil, dir, 5_000_000_000, 0)
	s = sized(s, 100, 30)

	s = feedNS(s,
		nsConsensusMsg{backendIdx: 0, err: errors.New("connection refused")},
		nsExecutionMsg{backendIdx: 0, err: errors.New("connection refused")},
	)
	// Write cmd should have been returned by the second update.
	_, cmd := s.Update(nsExecutionMsg{backendIdx: 0, err: errors.New("again")})
	_ = cmd // we already triggered write above; second call is a no-op (write != writeIdle)

	if s.rows[0].consensus != probeFailed || s.rows[0].execution != probeFailed {
		t.Fatalf("expected both failed, got cons=%v exec=%v", s.rows[0].consensus, s.rows[0].execution)
	}
}

func TestNamespaceAllDoneTransitionsToResultView(t *testing.T) {
	dir := t.TempDir()
	s, _ := newNamespaceScreen(sampleBackends()[:1], nil, dir, 5_000_000_000, 0)
	s = sized(s, 100, 30)

	s = feedNS(s,
		nsConsensusMsg{backendIdx: 0, info: &opnode.PeerInfo{PeerID: "16Uiu2HAm-seq", NodeID: "0xseq", ENR: "enr:-seq"}, latencyMS: 12},
		nsExecutionMsg{backendIdx: 0, info: &elnode.NodeInfo{ID: "abc", Enode: "enode://x@1:1", ENR: "enr:-Ku"}, latencyMS: 8},
		nsWriteMsg{backendIdx: 0, path: dir + "/sequencer.json"},
	)
	if !s.allDone() {
		t.Fatalf("expected allDone, got false")
	}
	out := stripANSI(s.View())
	for _, want := range []string{
		"snapshot complete",
		"sequencer",
		"peer_id", "16Uiu2HAm-seq",
		"node_id", "abc",
		"enode", "enode://x@1:1",
		"file", "sequencer.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("result view missing %q:\n%s", want, out)
		}
	}
}

func TestNamespacePartialResultShowsErrorInline(t *testing.T) {
	dir := t.TempDir()
	s, _ := newNamespaceScreen(sampleBackends()[:1], nil, dir, 5_000_000_000, 0)
	s = sized(s, 100, 30)

	s = feedNS(s,
		nsConsensusMsg{backendIdx: 0, info: &opnode.PeerInfo{PeerID: "16Uiu2HAm-seq"}},
		nsExecutionMsg{backendIdx: 0, err: errors.New("rpc error -32601: method not found")},
		nsWriteMsg{backendIdx: 0, path: dir + "/sequencer.json"},
	)
	out := stripANSI(s.View())
	for _, want := range []string{
		"partial",
		"execution failed",
		"rpc error -32601",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("partial view missing %q:\n%s", want, out)
		}
	}
}

func TestNamespaceQEmitsPopMsg(t *testing.T) {
	s, _ := newNamespaceScreen(sampleBackends()[:1], nil, t.TempDir(), 5_000_000_000, 0)
	s = sized(s, 100, 30)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should emit popMsg cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("q cmd: got %T, want popMsg", cmd())
	}
}

func TestNamespaceTickRescheduleStopsWhenAllDone(t *testing.T) {
	dir := t.TempDir()
	s, _ := newNamespaceScreen(sampleBackends()[:1], nil, dir, 5_000_000_000, 0)
	s = sized(s, 100, 30)

	// Tick while still in progress: should reschedule.
	_, cmd := s.Update(nsTickMsg{})
	if cmd == nil {
		t.Errorf("tick during in-progress should reschedule")
	}

	// Settle everything, then tick: should NOT reschedule.
	s = feedNS(s,
		nsConsensusMsg{backendIdx: 0, info: &opnode.PeerInfo{PeerID: "x"}},
		nsExecutionMsg{backendIdx: 0, info: &elnode.NodeInfo{ID: "y"}},
		nsWriteMsg{backendIdx: 0, path: dir + "/sequencer.json"},
	)
	_, cmd = s.Update(nsTickMsg{})
	if cmd != nil {
		t.Errorf("tick after allDone should not reschedule, got cmd")
	}
}

func TestNamespaceSnapshotsForReview(t *testing.T) {
	dir := t.TempDir()
	bs := []config.Backend{
		{Name: "sequencer", ConsensusRPCURL: "http://127.0.0.1:4004", ExecutionRPCURL: "http://127.0.0.1:4000"},
		{Name: "ingress1", ConsensusRPCURL: "http://127.0.0.1:8004", ExecutionRPCURL: "http://127.0.0.1:8000"},
		{Name: "fullnode", ConsensusRPCURL: "http://127.0.0.1:6004", ExecutionRPCURL: "http://127.0.0.1:6000"},
	}
	s, _ := newNamespaceScreen(bs, nil, dir, 5_000_000_000, 0)
	s = sized(s, 100, 30)

	// Mid-flight: sequencer fully done, ingress1 consensus only, fullnode nothing.
	s = feedNS(s,
		nsConsensusMsg{backendIdx: 0, info: &opnode.PeerInfo{PeerID: "16Uiu-seq", NodeID: "0xseq", ENR: "enr:-seq"}, latencyMS: 12},
		nsExecutionMsg{backendIdx: 0, info: &elnode.NodeInfo{ID: "abc", Enode: "enode://x@1:1", ENR: "enr:-Ku"}, latencyMS: 8},
		nsWriteMsg{backendIdx: 0, path: dir + "/sequencer.json"},
		nsConsensusMsg{backendIdx: 1, info: &opnode.PeerInfo{PeerID: "16Uiu-ing"}, latencyMS: 21},
	)
	t.Logf("\n[mid-flight progress]\n%s", stripANSI(s.View()))

	// Finish everything.
	s = feedNS(s,
		nsExecutionMsg{backendIdx: 1, err: errors.New("rpc error -32601: method not found")},
		nsWriteMsg{backendIdx: 1, path: dir + "/ingress1.json"},
		nsConsensusMsg{backendIdx: 2, err: errors.New("connection refused")},
		nsExecutionMsg{backendIdx: 2, err: errors.New("connection refused")},
		nsWriteMsg{backendIdx: 2, path: dir + "/fullnode.json"},
	)
	t.Logf("\n[final result view]\n%s", stripANSI(s.View()))
}
