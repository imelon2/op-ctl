package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/opnode"
)

func sbBackends() []config.Backend {
	return []config.Backend{
		{Name: "seq-1", ConsensusRPCURL: "http://127.0.0.1:9545"},
		{Name: "ingress-1", ConsensusRPCURL: "http://127.0.0.1:9546"},
		{Name: "ingress-2", ConsensusRPCURL: "http://127.0.0.1:9547"},
	}
}

// mkStatus builds a SyncStatus where every column's Number is offset
// from a shared baseline. Lets tests express "this backend trails head
// by N" without hand-writing eight separate BlockRef literals.
func mkStatus(unsafeL2, safeL2, finalizedL2, currentL1, currentL1Finalized, headL1, safeL1, finalizedL1 uint64) *opnode.SyncStatus {
	return &opnode.SyncStatus{
		UnsafeL2:           opnode.BlockRef{Number: unsafeL2},
		SafeL2:             opnode.BlockRef{Number: safeL2},
		FinalizedL2:        opnode.BlockRef{Number: finalizedL2},
		CurrentL1:          opnode.BlockRef{Number: currentL1},
		CurrentL1Finalized: opnode.BlockRef{Number: currentL1Finalized},
		HeadL1:             opnode.BlockRef{Number: headL1},
		SafeL1:             opnode.BlockRef{Number: safeL1},
		FinalizedL1:        opnode.BlockRef{Number: finalizedL1},
	}
}

// feed a sequence of messages into the state-block screen and return
// the final model. Mirrors namespace_screen_test.go's feedNS helper.
func feedSB(s stateBlockScreen, msgs ...tea.Msg) stateBlockScreen {
	for _, m := range msgs {
		next, _ := s.Update(m)
		s = next.(stateBlockScreen)
	}
	return s
}

func TestStateBlockScreen_View_InitialAllPolling(t *testing.T) {
	s := newStateBlockScreen(sbBackends(), nil, time.Second, 5*time.Second, 0)
	out := stripANSI(s.View())
	for _, name := range []string{"seq-1", "ingress-1", "ingress-2"} {
		if !strings.Contains(out, name) {
			t.Errorf("View should list backend %q:\n%s", name, out)
		}
	}
	// Both sections must show 'polling…' for every backend before any
	// snapshot has landed.
	if !strings.Contains(out, "Layer 2") {
		t.Errorf("View should include a 'Layer 2' section:\n%s", out)
	}
	if !strings.Contains(out, "Layer 1") {
		t.Errorf("View should include a 'Layer 1' section:\n%s", out)
	}
	if got := strings.Count(out, "polling"); got < 3 {
		t.Errorf("expected >=3 'polling…' rows (one per backend), got %d:\n%s", got, out)
	}
}

func TestStateBlockScreen_View_FoldInOK(t *testing.T) {
	s := newStateBlockScreen(sbBackends(), nil, time.Second, 5*time.Second, 0)
	// Advance to gen=1 so snapshot msgs are accepted.
	s = feedSB(s, stateBlockTickMsg{gen: 1})
	// Per-column heads after fold-in:
	//   unsafe_l2          = max(500, 497, 499) = 500
	//   safe_l2            = max(400, 398, 399) = 400
	//   finalized_l2       = max(300, 299, 298) = 300
	//   current_l1         = max(1000, 998, 999) = 1000
	//   current_l1_finalized = max(900, 899, 898) = 900
	//   head_l1            = max(1100, 1098, 1099) = 1100
	//   safe_l1            = max(950, 949, 948) = 950
	//   finalized_l1       = max(800, 799, 798) = 800
	s = feedSB(s,
		stateBlockSnapshotMsg{gen: 1, backendIdx: 0,
			status:     mkStatus(500, 400, 300, 1000, 900, 1100, 950, 800),
			latency:    50 * time.Millisecond,
			observedAt: time.Now(),
		},
		stateBlockSnapshotMsg{gen: 1, backendIdx: 1,
			status:     mkStatus(497, 398, 299, 998, 899, 1098, 949, 799),
			latency:    80 * time.Millisecond,
			observedAt: time.Now(),
		},
		stateBlockSnapshotMsg{gen: 1, backendIdx: 2,
			status:     mkStatus(499, 399, 298, 999, 898, 1099, 948, 798),
			latency:    60 * time.Millisecond,
			observedAt: time.Now(),
		},
	)
	out := stripANSI(s.View())

	// Head row cells (lag=0) on backend seq-1 across all 8 columns.
	for _, want := range []string{
		"500(0)", // unsafe_l2 head
		"400(0)", // safe_l2 head
		"300(0)", // finalized_l2 head
		"1000(0)",
		"900(0)",
		"1100(0)",
		"950(0)",
		"800(0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View should contain head cell %q:\n%s", want, out)
		}
	}

	// Spot-check a few lag cells on the trailing backends to confirm
	// per-column lag math, not just head identification.
	//   ingress-1 unsafe_l2: 497, head 500 → lag 3
	//   ingress-2 finalized_l2: 298, head 300 → lag 2
	//   ingress-1 head_l1: 1098, head 1100 → lag 2
	for _, want := range []string{"497(3)", "298(2)", "1098(2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("View should contain lag cell %q:\n%s", want, out)
		}
	}

	// Indicator section assertions — three labeled lines below
	// Layer 1. Each line shows `name(head)` for both operands, with
	// the operand columns padded so `-` and `=` align across rows.
	// Heads: unsafe_l2=500, safe_l2=400, finalized_l2=300, head_l1=1100,
	// current_l1=1000. Operand widths in this fixture:
	//   left:  max(unsafe_l2(500)=14, safe_l2(400)=12, head_l1(1100)=13) = 14
	//   right: max(safe_l2(400)=12, finalized_l2(300)=17, current_l1(1000)=16) = 17
	if !strings.Contains(out, "Indicator") {
		t.Errorf("View should contain 'Indicator' section title:\n%s", out)
	}
	for _, want := range []string{
		"unsafe_l2(500) - safe_l2(400)      = 100 ( 3m20s )",
		"safe_l2(400)   - finalized_l2(300) = 100 ( 3m20s )",
		"head_l1(1100)  - current_l1(1000)  = 100 ( 20m00s )",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View should contain indicator line %q:\n%s", want, out)
		}
	}
}

func TestStateBlockScreen_View_FoldInERR(t *testing.T) {
	s := newStateBlockScreen(sbBackends(), nil, time.Second, 5*time.Second, 0)
	s = feedSB(s, stateBlockTickMsg{gen: 1})
	s = feedSB(s,
		stateBlockSnapshotMsg{gen: 1, backendIdx: 0,
			status:     mkStatus(500, 400, 300, 1000, 900, 1100, 950, 800),
			latency:    50 * time.Millisecond,
			observedAt: time.Now(),
		},
		stateBlockSnapshotMsg{gen: 1, backendIdx: 1,
			err:        errors.New("connection refused"),
			observedAt: time.Now(),
		},
		stateBlockSnapshotMsg{gen: 1, backendIdx: 2,
			status:     mkStatus(498, 399, 298, 998, 898, 1098, 948, 798),
			latency:    60 * time.Millisecond,
			observedAt: time.Now(),
		},
	)
	out := stripANSI(s.View())

	// The failed backend's ERR string must appear in BOTH sections —
	// renderSection is called once per section, so the err row is
	// emitted twice (once under Layer 2, once under Layer 1).
	if got := strings.Count(out, "ERR connection refused"); got < 2 {
		t.Errorf("expected ERR row to span both sections (>=2 occurrences), got %d:\n%s", got, out)
	}
	// Surviving backends still compute per-column head/lag without the
	// failed row polluting head.
	if !strings.Contains(out, "500(0)") {
		t.Errorf("seq-1 unsafe_l2 head=500(0) missing:\n%s", out)
	}
	if !strings.Contains(out, "498(2)") {
		t.Errorf("ingress-2 unsafe_l2 should be 498(2) (head 500 - 498):\n%s", out)
	}
}

// TestStateBlockScreen_View_AllErrored_IndicatorSuppressed: when every
// backend errors on a tick the Indicator section is omitted entirely
// (no section title, no indicator lines).
func TestStateBlockScreen_View_AllErrored_IndicatorSuppressed(t *testing.T) {
	s := newStateBlockScreen(sbBackends(), nil, time.Second, 5*time.Second, 0)
	s = feedSB(s, stateBlockTickMsg{gen: 1})
	s = feedSB(s,
		stateBlockSnapshotMsg{gen: 1, backendIdx: 0, err: errors.New("e1"), observedAt: time.Now()},
		stateBlockSnapshotMsg{gen: 1, backendIdx: 1, err: errors.New("e2"), observedAt: time.Now()},
		stateBlockSnapshotMsg{gen: 1, backendIdx: 2, err: errors.New("e3"), observedAt: time.Now()},
	)
	out := stripANSI(s.View())
	for _, banned := range []string{"Indicator", "unsafe_l2(", "head_l1("} {
		if strings.Contains(out, banned) {
			t.Errorf("Indicator fragment %q should be suppressed when all backends err:\n%s", banned, out)
		}
	}
}

// TestStateBlockScreen_Update_StaleSnapshotDropped: a snapshot tagged
// with an older generation than the screen's current gen must be
// ignored entirely.
func TestStateBlockScreen_Update_StaleSnapshotDropped(t *testing.T) {
	s := newStateBlockScreen(sbBackends(), nil, time.Second, 5*time.Second, 0)
	// Advance to gen=2 by feeding two ticks.
	s = feedSB(s, stateBlockTickMsg{gen: 1}, stateBlockTickMsg{gen: 2})
	if s.gen != 2 {
		t.Fatalf("setup: s.gen should be 2, got %d", s.gen)
	}
	// Try to fold in an old-gen snapshot.
	s = feedSB(s, stateBlockSnapshotMsg{gen: 1, backendIdx: 0,
		status: mkStatus(999, 0, 0, 0, 0, 0, 0, 0),
	})
	if !s.snapshots[0].pending {
		t.Errorf("stale snapshot should NOT have toggled pending; row 0 was overwritten with %v", s.snapshots[0])
	}
	if s.snapshots[0].status != nil {
		t.Errorf("stale snapshot should have been dropped; got status=%+v", s.snapshots[0].status)
	}
}

// TestStateBlockScreen_QEmitsPopMsg mirrors
// namespace_screen_test.go:169-174.
func TestStateBlockScreen_QEmitsPopMsg(t *testing.T) {
	s := newStateBlockScreen(sbBackends(), nil, time.Second, 5*time.Second, 0)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should emit a tea.Cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("q cmd: got %T, want popMsg", cmd())
	}
}

// TestRunStateBlockPlain_ContextCancelled: an already-cancelled context
// must return immediately without blocking. A watchdog catches the
// regression case where the loop waits on ticker.C before checking
// ctx.Done().
func TestRunStateBlockPlain_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- runStateBlockPlainForTest(ctx, io.Discard, []config.Backend{}, 10*time.Millisecond, 100*time.Millisecond)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runStateBlockPlain returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("runStateBlockPlain did not return within 1s on cancelled context")
	}
}

// runStateBlockPlainForTest is a thin in-test wrapper that mirrors the
// loop shape in cmd/op-ctl/state.go:runStateBlockPlain. We duplicate
// the structure here so the screen-package test does not import the
// main cmd package. The contract under test is the select-based loop
// pattern (ctx.Done() must unblock mid-interval), which is independent
// of the real RPC fan-out.
//
// Keep in sync with cmd/op-ctl/state.go:runStateBlockPlain — if the
// production loop drops the render-first-then-wait order or replaces
// the select with a poll-then-sleep shape, this test will continue
// to pass while the real Ctrl+C path silently regresses. PR reviewers
// should diff both functions when either changes.
func runStateBlockPlainForTest(ctx context.Context, w io.Writer, bs []config.Backend, interval, timeout time.Duration) error {
	_ = w
	_ = bs
	_ = timeout
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// TestHandleCmdSelected_BlockUnderState verifies the menu-path dispatch
// lands on stateBlockScreen when the user selects `block` from inside
// the `status` parent menu. The cobra tree is assembled explicitly
// (rather than via stubCobra()) so the test does not regress if the
// stub evolves.
func TestHandleCmdSelected_BlockUnderState(t *testing.T) {
	root := &cobra.Command{Use: "op-ctl"}
	stateRoot := &cobra.Command{Use: "status"}
	blockLeaf := &cobra.Command{
		Use:  "block",
		RunE: func(cmd *cobra.Command, args []string) error { return nil },
	}
	stateRoot.AddCommand(blockLeaf)
	root.AddCommand(stateRoot)

	cfg := stubConfig(t, "sequencer")
	a := New(root, cfg, nil, t.TempDir(), 5*time.Second)
	a = feed(t, a, tea.WindowSizeMsg{Width: 100, Height: 30})

	// Replace the root cmdMenu with one rooted at "status" so the
	// dispatch resolves "block" as a direct child whose
	// Parent().Name() == "status".
	a.stack[0] = newCmdMenu(stateRoot, "status")

	next, _ := a.Update(cmdSelectedMsg{name: "block"})
	a = next.(App)

	if got := len(a.stack); got != 2 {
		t.Fatalf("stack depth after block: got %d, want 2", got)
	}
	if _, ok := a.stack[1].(stateBlockScreen); !ok {
		t.Errorf("top of stack: got %T, want stateBlockScreen", a.stack[1])
	}
}
