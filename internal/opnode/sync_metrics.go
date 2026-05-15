package opnode

import (
	"fmt"
	"time"
)

// L2BlockSeconds is the nominal L2 block time used to translate L2
// block-count gaps into human-readable durations. Optimism / OP-stack
// chains target 2s per L2 block.
func L2BlockSeconds() time.Duration { return 2 * time.Second }

// L1BlockSeconds is the nominal L1 block time used to translate L1
// block-count gaps. Ethereum mainnet post-merge targets ~12s per slot.
func L1BlockSeconds() time.Duration { return 12 * time.Second }

// BlockGapSeconds returns the wall-clock duration of `blocks` blocks
// at the given per-block cadence.
func BlockGapSeconds(blocks uint64, perBlock time.Duration) time.Duration {
	return time.Duration(blocks) * perBlock
}

// HumanizeGap returns just the time portion of a block-count gap —
// e.g., "3m20s" for 100 L2 blocks. The pipeline diagram prints the
// block count inside the connector and uses this for the time arrow
// label, so the two pieces are composed at the call site.
//
// The human portion rolls up units at natural boundaries:
//
//	<60s   → "Ns"
//	<1h    → "MmSSs"
//	>=1h   → "HhMMm"
func HumanizeGap(blocks uint64, perBlock time.Duration) string {
	if blocks == 0 {
		return "0s"
	}
	return humanizeDuration(BlockGapSeconds(blocks, perBlock))
}

func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%02dm", h, m)
}
