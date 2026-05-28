package l1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// EthGetBalance issues a JSON-RPC eth_getBalance call against the
// supplied RPC URL at block tag "latest" and returns the balance in
// wei as a *big.Int. Used by `read network-fee` to inspect each L2
// FeeVault predeploy's accumulated wei balance, but URL-agnostic so
// any side of the bridge can reuse it.
//
// The shape mirrors EthCall: the latency is reported on both success
// and failure paths so callers can render "ERR (latency: 5.0s)" on
// timeout.
func EthGetBalance(ctx context.Context, hc *http.Client, url, addr string) (*big.Int, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if strings.TrimSpace(url) == "" {
		return nil, 0, fmt.Errorf("rpc url is empty")
	}
	if strings.TrimSpace(addr) == "" {
		return nil, 0, fmt.Errorf("address is empty")
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBalance",
		Params:  []any{addr, "latest"},
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
	var result string
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, latency, fmt.Errorf("decode result string: %w", err)
	}
	if result == "" || result == "0x" {
		return nil, latency, fmt.Errorf("rpc result is empty")
	}
	n, err := decodeUint256(result)
	if err != nil {
		return nil, latency, fmt.Errorf("decode balance: %w", err)
	}
	return n, latency, nil
}
