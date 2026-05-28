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

// BlockHeader carries the subset of an eth_getBlockByNumber result
// op-ctl actually reads. The full block object is large (logs bloom,
// tx list, …); only Number and ExtraData are decoded here.
type BlockHeader struct {
	Number    *big.Int
	ExtraData []byte
}

// EthGetBlockByNumber fetches the block at tag ("latest", "0x..") with
// full transactions omitted and returns its number + raw extraData.
// Envelope handling and latency reporting mirror EthGetBalance. Empty
// extraData ("0x") decodes to a zero-length slice rather than an error
// — pre-Holocene blocks legitimately carry no extraData.
func EthGetBlockByNumber(ctx context.Context, hc *http.Client, url, tag string) (*BlockHeader, time.Duration, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if strings.TrimSpace(url) == "" {
		return nil, 0, fmt.Errorf("rpc url is empty")
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBlockByNumber",
		Params:  []any{tag, false},
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
	var blk struct {
		Number    string `json:"number"`
		ExtraData string `json:"extraData"`
	}
	if err := json.Unmarshal(env.Result, &blk); err != nil {
		return nil, latency, fmt.Errorf("decode block result: %w", err)
	}
	if blk.Number == "" {
		return nil, latency, fmt.Errorf("block not found (tag %q)", tag)
	}
	num, err := decodeUint256(blk.Number)
	if err != nil {
		return nil, latency, fmt.Errorf("decode block number: %w", err)
	}
	es := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(blk.ExtraData)), "0x")
	extra, err := hex.DecodeString(es)
	if err != nil {
		return nil, latency, fmt.Errorf("decode extraData: %w", err)
	}
	return &BlockHeader{Number: num, ExtraData: extra}, latency, nil
}
