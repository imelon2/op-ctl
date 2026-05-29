package etherscan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BaseURL is the Etherscan V2 multi-chain endpoint. V2 routes the
// per-chain dispatch via the `chainid` query parameter, so a single
// constant covers mainnet, all testnets, and L2s — the caller passes
// the chainid in via FetchTxList.
const BaseURL = "https://api.etherscan.io/v2/api"

// throttleInterval is the inter-request sleep enforced by FetchTxList
// to stay under Etherscan's free-tier 5 calls/sec limit (200ms = 5/s).
// Hard-coded for now; if an operator on a paid tier wants to raise the
// ceiling, expose it via config in a follow-up change rather than
// adding flags here.
const throttleInterval = 200 * time.Millisecond

// pageSize is the txlist offset (page size) Etherscan V2 caps at 1000
// per call. The caller cannot meaningfully change this — it is the API
// contract, not a tunable.
const pageSize = 1000

// ResolveChainID returns the integer chainID exposed by l1RPCURL via
// JSON-RPC eth_chainId. `op-ctl read batch` passes the result as the
// `chainid` query parameter to FetchTxList; resolving dynamically
// (instead of hard-coding) lets the same code switch between Sepolia,
// mainnet, and any other chain reachable on the operator's L1.
func ResolveChainID(ctx context.Context, hc *http.Client, l1RPCURL string) (uint64, error) {
	if strings.TrimSpace(l1RPCURL) == "" {
		return 0, fmt.Errorf("etherscan: l1_rpc_url is empty")
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_chainId",
		"params":  []any{},
	})
	if err != nil {
		return 0, fmt.Errorf("etherscan: marshal eth_chainId request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l1RPCURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("etherscan: build eth_chainId request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("etherscan: eth_chainId: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("etherscan: read eth_chainId body: %w", err)
	}
	var env struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("etherscan: decode eth_chainId body %q: %w", string(raw), err)
	}
	if env.Error != nil {
		return 0, fmt.Errorf("etherscan: eth_chainId rpc error: %s", env.Error.Message)
	}
	hex := strings.TrimPrefix(strings.TrimSpace(env.Result), "0x")
	if hex == "" {
		return 0, fmt.Errorf("etherscan: empty eth_chainId result")
	}
	n, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("etherscan: parse eth_chainId %q: %w", env.Result, err)
	}
	return n, nil
}

// FetchTxList paginates Etherscan V2 `module=account&action=txlist`
// starting at fromBlock for the given address on chainID. Each
// successfully parsed page is handed to onPage; the cache layer uses
// that callback to commit per-page so a partial sync (network hiccup,
// daily rate-limit hit, ctx cancel) still saves whatever pages
// already arrived.
//
// Critical contract for the cache layer: `sort=asc` is hard-coded so
// every page's tx slice is monotone non-decreasing on block_number.
// internal/batchcache.UpsertPage relies on this to compute
// `last_synced_block = max(block in this page)` correctly.
//
// Termination conditions:
//   - status="0" message="No transactions found"   → success (no more pages)
//   - len(page) < pageSize                          → success (this was the tail)
//   - status="0" any other message                  → wrapped error (rate limit / auth fail)
//   - onPage returns non-nil error                  → fetcher halts, error wrapped
//   - ctx.Done                                      → ctx.Err() wrapped
//
// The 200ms inter-request throttle is the only knob; it stays under
// the public-tier 5 calls/sec quota with comfortable headroom.
func FetchTxList(
	ctx context.Context,
	hc *http.Client,
	apiKey string,
	chainID uint64,
	address string,
	fromBlock uint64,
	onPage func(page int, txs []Tx) error,
	progress io.Writer,
) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("etherscan: api key is empty (set [urls].etherscan_api_key)")
	}
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("etherscan: address is empty")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	if progress == nil {
		progress = io.Discard
	}
	for page := 1; ; page++ {
		// Sleep BETWEEN requests, not before the first. Honor ctx
		// cancellation while sleeping so a Ctrl+C is responsive
		// rather than parked for up to one full throttleInterval.
		if page > 1 {
			t := time.NewTimer(throttleInterval)
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("etherscan: ctx canceled during throttle: %w", ctx.Err())
			case <-t.C:
			}
		}
		body, status, message, txs, err := fetchPage(ctx, hc, apiKey, chainID, address, fromBlock, page)
		if err != nil {
			return fmt.Errorf("etherscan: page %d: %w", page, err)
		}
		// Terminal "no more rows" — Etherscan's idiomatic empty page.
		if status == "0" && strings.Contains(strings.ToLower(message), "no transactions found") {
			return nil
		}
		// Any other status="0" is an error (rate limit, bad key, …).
		if status == "0" {
			return fmt.Errorf("etherscan: page %d: api rejected (status=0, message=%q): %s", page, message, truncate(body, 200))
		}
		// Hand the parsed page to the caller (per-page commit). A
		// callback error halts the fetcher so partial state stays
		// usable but no further wasted HTTP calls happen.
		if err := onPage(page, txs); err != nil {
			return fmt.Errorf("etherscan: page %d: onPage callback: %w", page, err)
		}
		// Progress log AFTER the commit so a viewer of the stderr
		// stream knows the row count is durable.
		lastBlock := uint64(0)
		if n := len(txs); n > 0 {
			lastBlock = txs[n-1].BlockNumber
		}
		fmt.Fprintf(progress, "page %d (%d txs, last_block=%d)\n", page, len(txs), lastBlock)
		// Short page means we reached the tail of the chain — done.
		if len(txs) < pageSize {
			return nil
		}
	}
}

// fetchPage performs one HTTP GET against Etherscan V2 and returns
// the decoded envelope status, message, and tx slice. The raw body
// is returned alongside so the caller can include a truncated copy
// in the wrapped error when status=="0".
func fetchPage(
	ctx context.Context,
	hc *http.Client,
	apiKey string,
	chainID uint64,
	address string,
	fromBlock uint64,
	page int,
) (body []byte, status, message string, txs []Tx, err error) {
	url := fmt.Sprintf(
		"%s?chainid=%d&module=account&action=txlist&address=%s&startblock=%d&endblock=99999999&page=%d&offset=%d&sort=asc&apikey=%s",
		BaseURL, chainID, address, fromBlock, page, pageSize, apiKey,
	)
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if rerr != nil {
		err = fmt.Errorf("build request: %w", rerr)
		return
	}
	resp, herr := hc.Do(req)
	if herr != nil {
		err = fmt.Errorf("http: %w", herr)
		return
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("read body: %w", err)
		return
	}
	if resp.StatusCode/100 != 2 {
		err = fmt.Errorf("http status %d: %s", resp.StatusCode, truncate(body, 200))
		return
	}
	status, message, txs, err = decodeTxList(body)
	return
}

// truncate clips body to max bytes for error messages — keeps logs
// readable when Etherscan returns an HTML 503 page instead of JSON.
func truncate(body []byte, max int) string {
	s := strings.TrimSpace(string(body))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
