package opnode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RPCError is a JSON-RPC `error` object surfaced as a typed Go error.
// Unlike the inline fmt.Errorf path used by Self() / Peers() (which
// existing tests pin by exact substring), the discovery flow needs a
// typed error so the TUI can detect the "discovery disabled" case
// (op-node code -32000 with a "discovery disabled" message) and
// render a tailored UX. Self()/Peers() are intentionally left alone
// to avoid breaking their tests.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// IsDiscoveryDisabled reports whether err is op-node's typed
// "discovery disabled" RPC error. We string-match on the message
// (case-insensitive, "discovery" + "disabled" both present) instead
// of hard-coding -32000 because the code itself is the generic
// server-error space, but the message is consistent across op-node
// versions.
func IsDiscoveryDisabled(err error) bool {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		m := strings.ToLower(rpcErr.Message)
		return strings.Contains(m, "discovery") && strings.Contains(m, "disabled")
	}
	return false
}

// DiscoveryTable calls opp2p_discoveryTable and returns the slice of
// ENR strings op-node currently knows about via discv5. Returns an
// RPCError when discovery is disabled — see IsDiscoveryDisabled for
// the canonical detection.
func DiscoveryTable(ctx context.Context, hc *http.Client, url string) ([]string, time.Duration, error) {
	var enrs []string
	latency, err := callRPC(ctx, hc, url, "opp2p_discoveryTable", []any{}, &enrs)
	if err != nil {
		return nil, latency, err
	}
	return enrs, latency, nil
}

// callRPC is a generic JSON-RPC 2.0 helper used by DiscoveryTable
// (and any future opp2p_* method that doesn't need bespoke wiring).
// Mirrors the inline plumbing in client.go but returns RPCError as a
// typed value so callers can errors.As-detect specific codes /
// messages.
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
