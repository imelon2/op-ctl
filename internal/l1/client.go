// Package l1 is a minimal JSON-RPC 2.0 client for read-only L1 view
// calls op-ctl uses to inspect settlement-layer contract state.
//
// Scope is intentionally narrow: eth_call (single + batched) + selector-
// based view methods, with a hand-rolled ABI codec in abi.go for the
// scalar and string/bytes types op-ctl actually consumes. No logs, no
// subscriptions, no signing. Pulling in go-ethereum/rpc + abi/bind
// would balloon the binary by several MB plus a CGO surface for a
// handful of read-only calls; the same rationale is documented in
// internal/opnode/client.go.
//
// Function selectors are computed at call time via abi.go's selectorOf
// (keccak256 over the canonical Solidity signature). Each call site
// pins the literal signature string so a mistype changes the selector
// and the next test run catches it.
package l1

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// gameCountSelector is the precomputed 4-byte function selector for
//
//	function gameCount() external view returns (uint256)
//
// in DisputeGameFactory.sol — keccak256("gameCount()")[0:4] = 0x4d1975b4.
//
// Kept as a literal constant after the migration to selectorOf so the
// abi_test.go `TestSelectorOf_MatchesPhase1Constant` test acts as a
// canary: if the keccak implementation, the canonical signature, or
// the encoding ever drifts, that test fails. GameCount() reuses the
// constant directly to avoid recomputing the selector on every call.
const gameCountSelector = "0x4d1975b4"

const maxBodySize = 1 << 20

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

type ethCallParams struct {
	To   string `json:"to"`
	Data string `json:"data"`
}

// GameCount calls DisputeGameFactoryProxy.gameCount() via eth_call on
// the supplied L1 RPC URL and returns the decoded uint256 alongside
// the wall-clock latency of the HTTP round trip. latency is reported
// for both success and failure paths so the UI can render
// "ERR (latency: 5.0s)" on timeout.
func GameCount(ctx context.Context, hc *http.Client, l1RPCURL, gameFactoryAddr string) (*big.Int, time.Duration, error) {
	if strings.TrimSpace(l1RPCURL) == "" {
		return nil, 0, fmt.Errorf("l1_rpc_url is empty (set [rpc].l1_rpc_url in config.toml)")
	}
	if strings.TrimSpace(gameFactoryAddr) == "" {
		return nil, 0, fmt.Errorf("DisputeGameFactoryProxy address is empty")
	}
	raw, latency, err := EthCall(ctx, hc, l1RPCURL, gameFactoryAddr, gameCountSelector)
	if err != nil {
		return nil, latency, err
	}
	n, err := decodeUint256(raw)
	if err != nil {
		return nil, latency, fmt.Errorf("decode gameCount: %w", err)
	}
	return n, latency, nil
}

// EthCall issues a JSON-RPC eth_call with block tag "latest" against
// `to` with `data` (selector + ABI-encoded args, 0x-prefixed) and
// returns the hex-encoded result string from the node.
//
// Exported so other selector-specific helpers in this package (and
// future read commands) can share the envelope handling.
func EthCall(ctx context.Context, hc *http.Client, url, to, data string) (string, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_call",
		Params:  []any{ethCallParams{To: to, Data: data}, "latest"},
	})
	if err != nil {
		return "", 0, fmt.Errorf("marshal rpc request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := hc.Do(req)
	latency := time.Since(start)
	if err != nil {
		return "", latency, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", latency, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return "", latency, fmt.Errorf("http %d: %s", resp.StatusCode, string(snippet))
	}
	var env rpcResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", latency, fmt.Errorf("decode rpc envelope: %w", err)
	}
	if env.Error != nil {
		return "", latency, fmt.Errorf("rpc error %d: %s", env.Error.Code, env.Error.Message)
	}
	var result string
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return "", latency, fmt.Errorf("decode result string: %w", err)
	}
	// "0x" with no payload means the call executed but the contract
	// returned no data — typically a wrong address or a contract that
	// doesn't implement the selector (proxied to an unrelated impl).
	if result == "" || result == "0x" {
		return "", latency, fmt.Errorf("rpc result is empty (contract returned no data — check the address and L1 chain)")
	}
	return result, latency, nil
}

// EthCallReq is one entry in a JSON-RPC batch — a (to, data) pair
// targeted at the L1 RPC's eth_call method. Phase 2's list and detail
// screens issue many calls per fan-out, so batching them into one
// HTTP POST avoids public-RPC rate limiting and cuts the wall-clock
// to a single roundtrip.
type EthCallReq struct {
	To   string
	Data string
}

// EthCallResult is the per-call outcome from a batched eth_call. Err
// is non-nil only for the calls that returned an RPC envelope error
// or an empty/null result; other calls populate Result with the raw
// hex string (0x-prefixed) the node returned.
type EthCallResult struct {
	Result string
	Err    error
}

// EthCallBatch sends a single HTTP POST containing a JSON array of
// eth_call requests and returns one EthCallResult per input in the
// same order. Results are matched back by the per-request `id` field
// because the node is free to reorder its response array.
//
// A transport-level failure (build req error, http.Do error, body
// read error, non-2xx, malformed envelope) returns (nil, latency,
// err) — callers should treat that as "everything failed". Per-call
// RPC errors are surfaced inside the result slice instead so a single
// reverting view doesn't fail the whole fan-out.
func EthCallBatch(ctx context.Context, hc *http.Client, url string, calls []EthCallReq) ([]EthCallResult, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if strings.TrimSpace(url) == "" {
		return nil, 0, fmt.Errorf("l1_rpc_url is empty")
	}
	if len(calls) == 0 {
		return nil, 0, nil
	}
	reqs := make([]rpcRequest, len(calls))
	for i, c := range calls {
		reqs[i] = rpcRequest{
			JSONRPC: "2.0",
			ID:      i + 1, // 1-based so id==0 can flag "no match"
			Method:  "eth_call",
			Params:  []any{ethCallParams{To: c.To, Data: c.Data}, "latest"},
		}
	}
	body, err := json.Marshal(reqs)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal batch request: %w", err)
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

	// Batch responses can grow proportionally to len(calls); raise the
	// cap from the per-call default (1 MiB) to a flat 4 MiB ceiling
	// that still bounds memory usage but accommodates ~25 detail-screen
	// calls each with sizable return data (gameData() returns variable
	// extraData; in practice each result is well under 1 KiB).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, latency, fmt.Errorf("read batch response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, latency, fmt.Errorf("http %d: %s", resp.StatusCode, string(snippet))
	}
	var envs []rpcResponse
	if err := json.Unmarshal(raw, &envs); err != nil {
		return nil, latency, fmt.Errorf("decode batch envelope: %w", err)
	}
	if len(envs) != len(calls) {
		return nil, latency, fmt.Errorf("batch size mismatch: got %d responses, sent %d requests", len(envs), len(calls))
	}
	out := make([]EthCallResult, len(calls))
	// Initialize with a "missing response" error so an envelope with a
	// bogus id leaves a detectable per-call failure rather than a zero
	// value masquerading as success.
	for i := range out {
		out[i].Err = fmt.Errorf("no response with id=%d in batch", i+1)
	}
	for _, env := range envs {
		idx := env.ID - 1
		if idx < 0 || idx >= len(out) {
			continue
		}
		if env.Error != nil {
			out[idx] = EthCallResult{Err: fmt.Errorf("rpc error %d: %s", env.Error.Code, env.Error.Message)}
			continue
		}
		var result string
		if err := json.Unmarshal(env.Result, &result); err != nil {
			out[idx] = EthCallResult{Err: fmt.Errorf("decode result string: %w", err)}
			continue
		}
		if result == "" || result == "0x" {
			out[idx] = EthCallResult{Err: fmt.Errorf("contract returned no data")}
			continue
		}
		out[idx] = EthCallResult{Result: result}
	}
	return out, latency, nil
}

// decodeUint256 parses a 0x-prefixed hex string of at most 32 bytes
// into a non-negative big.Int. Well-formed eth_call output for a
// uint256-returning view is already padded to 64 hex chars; the
// shorter-input path is defensive against permissive RPC nodes.
func decodeUint256(s string) (*big.Int, error) {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if len(s) == 0 {
		return nil, fmt.Errorf("empty hex string")
	}
	if len(s) > 64 {
		return nil, fmt.Errorf("hex too long for uint256: %d chars", len(s))
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}
