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

// EthGasPrice issues a JSON-RPC eth_gasPrice call and returns the
// node's suggested gas price in wei. On an OP-Stack L2 this is
// baseFee + suggested priority fee.
func EthGasPrice(ctx context.Context, hc *http.Client, url string) (*big.Int, time.Duration, error) {
	return ethQuantityCall(ctx, hc, url, "eth_gasPrice")
}

// EthMaxPriorityFeePerGas issues eth_maxPriorityFeePerGas and returns
// the node's suggested priority fee (tip) in wei.
func EthMaxPriorityFeePerGas(ctx context.Context, hc *http.Client, url string) (*big.Int, time.Duration, error) {
	return ethQuantityCall(ctx, hc, url, "eth_maxPriorityFeePerGas")
}

// ethQuantityCall runs a no-argument JSON-RPC method that returns a
// single hex QUANTITY (e.g. eth_gasPrice, eth_maxPriorityFeePerGas)
// and decodes it into wei. Envelope handling and latency reporting
// mirror EthGetBalance.
func ethQuantityCall(ctx context.Context, hc *http.Client, url, method string) (*big.Int, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if strings.TrimSpace(url) == "" {
		return nil, 0, fmt.Errorf("rpc url is empty")
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
	var result string
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, latency, fmt.Errorf("decode result string: %w", err)
	}
	if result == "" || result == "0x" {
		return nil, latency, fmt.Errorf("rpc result is empty")
	}
	n, err := decodeUint256(result)
	if err != nil {
		return nil, latency, fmt.Errorf("decode %s: %w", method, err)
	}
	return n, latency, nil
}
