package opnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PeerEntry is one row in op-node's opp2p_peers `peers` map. Field names
// mirror op-node's PeerInfo struct exactly (op-service/p2p). Connectedness
// and Direction are libp2p enums marshaled as plain integers — see
// connectednessLabel / directionLabel callers for the symbolic mapping.
//
// Scores is intentionally json.RawMessage rather than a typed nested
// struct: the gossip / reqResp shape evolves with op-node versions and
// the detail view re-marshals it for human display, so a typed decode
// would just be schema drift waiting to bite us.
type PeerEntry struct {
	PeerID          string          `json:"peerID"`
	NodeID          string          `json:"nodeID"`
	UserAgent       string          `json:"userAgent"`
	ProtocolVersion string          `json:"protocolVersion"`
	ENR             string          `json:"ENR"`
	Addresses       []string        `json:"addresses"`
	Protocols       []string        `json:"protocols"`
	Connectedness   int             `json:"connectedness"`
	Direction       int             `json:"direction"`
	Protected       bool            `json:"protected"`
	ChainID         uint64          `json:"chainID"`
	Latency         uint64          `json:"latency"`
	GossipBlocks    bool            `json:"gossipBlocks"`
	Scores          json.RawMessage `json:"scores,omitempty"`
}

// PeerDump is the response shape of opp2p_peers — a snapshot of every
// peer op-node currently knows about (connected + cached + banned),
// keyed by libp2p peer ID.
type PeerDump struct {
	TotalConnected uint                 `json:"totalConnected"`
	Peers          map[string]PeerEntry `json:"peers"`
	BannedPeers    []string             `json:"bannedPeers"`
	BannedSubnets  []string             `json:"bannedSubnets"`
	BannedIPs      []string             `json:"bannedIPs"`
}

// Peers calls opp2p_peers(connected) on the given URL, honoring ctx
// deadline. Pass connected=false to include disconnected/cached peers.
//
// Error precedence mirrors Self() so callers can use the two
// interchangeably for fan-out work.
func Peers(ctx context.Context, hc *http.Client, url string, connected bool) (*PeerDump, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "opp2p_peers",
		Params:  []any{connected},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := hc.Do(req)
	latency := time.Since(start)
	if err != nil {
		return nil, latency, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, latency, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, latency, fmt.Errorf("http %d: %s", resp.StatusCode, string(snippet))
	}

	var env rpcResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, latency, fmt.Errorf("decode rpc envelope: %w", err)
	}
	if env.Error != nil {
		return nil, latency, fmt.Errorf("rpc error %d: %s", env.Error.Code, env.Error.Message)
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return nil, latency, fmt.Errorf("rpc result missing")
	}

	var dump PeerDump
	if err := json.Unmarshal(env.Result, &dump); err != nil {
		return nil, latency, fmt.Errorf("decode PeerDump: %w", err)
	}
	return &dump, latency, nil
}
