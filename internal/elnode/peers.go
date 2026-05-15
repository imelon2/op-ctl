package elnode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AdminPeer is one row in the execution-layer `admin_peers` response.
// Field names mirror op-geth's p2p.PeerInfo so a raw JSON dump
// round-trips. Protocols stays as RawMessage because each EL fork
// (eth, snap, ...) reports its own per-protocol shape and a typed
// decode would just chase upstream schema drift.
type AdminPeer struct {
	Enode     string                     `json:"enode"`
	ID        string                     `json:"id"`
	Name      string                     `json:"name"`
	Caps      []string                   `json:"caps"`
	Network   AdminPeerNetwork           `json:"network"`
	Protocols map[string]json.RawMessage `json:"protocols"`
}

// AdminPeerNetwork is the nested `network` block inside admin_peers.
type AdminPeerNetwork struct {
	LocalAddress  string `json:"localAddress"`
	RemoteAddress string `json:"remoteAddress"`
	Inbound       bool   `json:"inbound"`
	Trusted       bool   `json:"trusted"`
	Static        bool   `json:"static"`
}

// RPCError is a JSON-RPC `error` object returned in callRPC failures.
// Exposed as a typed error so callers can distinguish method-not-found
// (-32601 — typically "admin namespace disabled") from generic
// transport errors via errors.As.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// IsMethodNotFound reports whether err is a JSON-RPC -32601 (the
// canonical "method does not exist / is not available" code op-geth
// returns when the admin namespace is disabled). Used by the TUI to
// show a tailored "enable admin" hint instead of a raw error string.
func IsMethodNotFound(err error) bool {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == -32601
	}
	return false
}

// PeerCount calls net_peerCount and returns the count as uint64. The
// RPC returns a hex-encoded string ("0x1"); we parse it once here so
// callers don't have to. net_peerCount lives in the `net` namespace,
// which is universally enabled, so this rarely fails when admin_peers
// would also fail with "method not found".
func PeerCount(ctx context.Context, hc *http.Client, url string) (uint64, time.Duration, error) {
	var hexStr string
	latency, err := callRPC(ctx, hc, url, "net_peerCount", []any{}, &hexStr)
	if err != nil {
		return 0, latency, err
	}
	s := strings.TrimPrefix(hexStr, "0x")
	if s == "" {
		return 0, latency, nil
	}
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, latency, fmt.Errorf("decode net_peerCount %q: %w", hexStr, err)
	}
	return n, latency, nil
}

// AdminPeers calls admin_peers. On execution nodes that don't expose
// the `admin` namespace this fails with RPCError{Code: -32601}; the
// caller is expected to show a tailored UX (see IsMethodNotFound).
func AdminPeers(ctx context.Context, hc *http.Client, url string) ([]AdminPeer, time.Duration, error) {
	var peers []AdminPeer
	latency, err := callRPC(ctx, hc, url, "admin_peers", []any{}, &peers)
	if err != nil {
		return nil, latency, err
	}
	return peers, latency, nil
}

// callRPC is a generic JSON-RPC 2.0 helper. Mirrors Self()'s inline
// plumbing in client.go but with a method+result-pointer parameter so
// we don't grow a dedicated function per RPC method. Kept separate
// from Self() to avoid touching tests that match Self()'s exact error
// strings.
func callRPC(ctx context.Context, hc *http.Client, url, method string, params []any, result any) (time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: method, Params: params,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := hc.Do(req)
	latency := time.Since(start)
	if err != nil {
		return latency, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return latency, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return latency, fmt.Errorf("http %d: %s", resp.StatusCode, string(snippet))
	}
	var env rpcResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return latency, fmt.Errorf("decode rpc envelope: %w", err)
	}
	if env.Error != nil {
		return latency, &RPCError{Code: env.Error.Code, Message: env.Error.Message}
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return latency, fmt.Errorf("rpc result missing")
	}
	if err := json.Unmarshal(env.Result, result); err != nil {
		return latency, fmt.Errorf("decode result: %w", err)
	}
	return latency, nil
}
