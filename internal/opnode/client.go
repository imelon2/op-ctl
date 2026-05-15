// Package opnode is a thin JSON-RPC 2.0 client for op-node's opp2p namespace.
//
// Why not go-ethereum/rpc: v1 calls one method (opp2p_self) over plain HTTP
// POST. go-ethereum/rpc would pull in subscription/IPC/secp256k1, several
// MB and a CGO build surface, for zero benefit.
//
// Method name: "opp2p_self" is verified against op-node:
//   prefixP2PRPC("self") in op-service/sources/p2p_client.go,
//   namespace const P2PNamespaceRPC = "opp2p".
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

const (
	method     = "opp2p_self"
	maxBodySize = 1 << 20 // 1 MiB
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Self calls opp2p_self on the given URL, honoring ctx deadline.
//
// Returns parsed PeerInfo and the wall-clock duration of the HTTP round
// trip. The duration is measured for both success and failure so the UI
// can display "ERR (latency: 5.0s)" on timeout.
//
// Error precedence:
//  1. Marshal request          → (nil, 0, err)
//  2. Build http.Request       → (nil, 0, err)
//  3. http.Client.Do error     → (nil, latency, err)  // includes ctx.DeadlineExceeded
//  4. Read body error          → (nil, latency, err)
//  5. Non-2xx HTTP status      → (nil, latency, err)
//  6. Decode envelope error    → (nil, latency, err)
//  7. RPC error field          → (nil, latency, err)
//  8. Decode result error      → (nil, latency, err)
//  9. Success                  → (&peer, latency, nil)
func Self(ctx context.Context, hc *http.Client, url string) (*PeerInfo, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  []any{},
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

	var peer PeerInfo
	if err := json.Unmarshal(env.Result, &peer); err != nil {
		return nil, latency, fmt.Errorf("decode PeerInfo: %w", err)
	}
	return &peer, latency, nil
}
