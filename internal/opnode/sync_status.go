package opnode

import (
	"context"
	"net/http"
	"time"
)

// SyncStatus mirrors op-node's optimism_syncStatus reply. The TUI only
// consumes the eight Number fields exposed here; the wire reply also
// contains pending_safe_l2, cross_unsafe_l2, and local_safe_l2, which
// this struct intentionally omits.
type SyncStatus struct {
	CurrentL1          BlockRef `json:"current_l1"`
	CurrentL1Finalized BlockRef `json:"current_l1_finalized"`
	HeadL1             BlockRef `json:"head_l1"`
	SafeL1             BlockRef `json:"safe_l1"`
	FinalizedL1        BlockRef `json:"finalized_l1"`
	UnsafeL2           BlockRef `json:"unsafe_l2"`
	SafeL2             BlockRef `json:"safe_l2"`
	FinalizedL2        BlockRef `json:"finalized_l2"`
}

// BlockRef is the common shape for both L1 and L2 block references in
// optimism_syncStatus. L2 references carry extra fields (`l1origin`,
// `sequenceNumber`) that this struct does not surface; they decode
// away silently.
type BlockRef struct {
	Hash       string `json:"hash"`
	Number     uint64 `json:"number"`
	ParentHash string `json:"parentHash"`
	Timestamp  uint64 `json:"timestamp"`
}

// Sync calls optimism_syncStatus on the given consensus RPC URL and
// returns the parsed reply + wall-clock latency of the round trip.
// Errors follow callRPC's typed precedence (RPCError surfaces -32601
// "method not found" detectable via errors.As, transport / decode
// errors as plain errors).
func Sync(ctx context.Context, hc *http.Client, url string) (*SyncStatus, time.Duration, error) {
	var s SyncStatus
	latency, err := callRPC(ctx, hc, url, "optimism_syncStatus", []any{}, &s)
	if err != nil {
		return nil, latency, err
	}
	return &s, latency, nil
}
