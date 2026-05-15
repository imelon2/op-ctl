package opnode

import (
	"testing"
	"time"
)

func TestL2BlockSeconds(t *testing.T) {
	if got := L2BlockSeconds(); got != 2*time.Second {
		t.Errorf("L2BlockSeconds: got %v, want 2s", got)
	}
}

func TestL1BlockSeconds(t *testing.T) {
	if got := L1BlockSeconds(); got != 12*time.Second {
		t.Errorf("L1BlockSeconds: got %v, want 12s", got)
	}
}

func TestBlockGapSeconds(t *testing.T) {
	cases := []struct {
		blocks   uint64
		perBlock time.Duration
		want     time.Duration
	}{
		{0, L2BlockSeconds(), 0},
		{1, L2BlockSeconds(), 2 * time.Second},
		{30, L2BlockSeconds(), 60 * time.Second},
		{4, L1BlockSeconds(), 48 * time.Second},
	}
	for _, c := range cases {
		if got := BlockGapSeconds(c.blocks, c.perBlock); got != c.want {
			t.Errorf("BlockGapSeconds(%d, %v): got %v, want %v", c.blocks, c.perBlock, got, c.want)
		}
	}
}

func TestHumanizeGap(t *testing.T) {
	cases := []struct {
		blocks   uint64
		perBlock time.Duration
		want     string
	}{
		// 0-block is the special path that returns "0s" without
		// dividing.
		{0, L2BlockSeconds(), "0s"},
		// Minute-boundary: exactly 60s reads "1m00s" (not "60s").
		{30, L2BlockSeconds(), "1m00s"},
		{134, L2BlockSeconds(), "4m28s"},
		{1029, L2BlockSeconds(), "34m18s"},
		// Hour rollover at L2 cadence.
		{3600, L2BlockSeconds(), "2h00m"},
		// L1 cadence.
		{4, L1BlockSeconds(), "48s"},
		{300, L1BlockSeconds(), "1h00m"},
		// 59s on a 1s cadence keeps the seconds form.
		{59, time.Second, "59s"},
	}
	for _, c := range cases {
		if got := HumanizeGap(c.blocks, c.perBlock); got != c.want {
			t.Errorf("HumanizeGap(%d, %v) = %q, want %q", c.blocks, c.perBlock, got, c.want)
		}
	}
}
