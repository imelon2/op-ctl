package batchprefetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"op-ctl/internal/batchcache"
	"op-ctl/internal/config"
	"op-ctl/internal/etherscan"
)

// writeTempFile creates path inside dir and writes content.
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// loadFixtureConfig builds a config.toml + state.json fixture pair
// under a fresh temp dir and returns the loaded Config. The Etherscan
// API key + batch addresses are placeholders the caller can override
// via the apiKey/inbox args; passing apiKey="" lets the test exercise
// the empty-key early-fail path.
func loadFixtureConfig(t *testing.T, apiKey, l1URL string, ttl string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	writeTempFile(t, dir, "state.json", `{
  "opChainDeployments": [
    { "id": "0xa5e8", "DisputeGameFactoryProxy": "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239" }
  ]
}`)
	cfgPath := writeTempFile(t, dir, "config.toml", fmt.Sprintf(`
[urls]
l1_rpc_url = %q
l2_rpc_url = "http://127.0.0.1:8545"
etherscan_api_key = %q

[batch]
batcher_from_address = "0xdf05E8C9C0Ef7b85d2536182fa1E911622622542"
batch_inbox_to_address = "0x00B607c67e6662aC51C747961b657659BB47FD95"
start_block = 0
cache_ttl = %q

[contracts]
state_root = "state.json"

[backends.sequencer]
consensus_rpc_url = "http://127.0.0.1:9545"
`, l1URL, apiKey, ttl))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// withTestTransport temporarily overrides HTTPClient with one that
// rewrites Etherscan-bound requests to the httptest server URL. The
// L1 RPC requests are NOT rewritten (cfg.URLs.L1RPCURL already points
// at the test server). On cleanup the original client is restored.
func withTestTransport(t *testing.T, etherscanURL string) {
	t.Helper()
	prev := HTTPClient
	t.Cleanup(func() { HTTPClient = prev })
	HTTPClient = &http.Client{Transport: rewriteEtherscan(etherscanURL), Timeout: 5 * time.Second}
}

// rewriteEtherscan returns a RoundTripper that redirects only the
// Etherscan V2 host to the test server while preserving the path +
// query string. Requests to other hosts (L1 RPC) pass through
// unchanged.
func rewriteEtherscan(etherscanURL string) http.RoundTripper {
	target, _ := url.Parse(etherscanURL)
	return roundTripFn(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Host, "etherscan.io") {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = "/"
		}
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestPrepare_EmptyAPIKeyEarlyFail asserts the cheapest gate fires
// first: an empty etherscan_api_key returns the clear-message error
// BEFORE any DB file is created or any HTTP call is made.
func TestPrepare_EmptyAPIKeyEarlyFail(t *testing.T) {
	cfg := loadFixtureConfig(t, "", "http://unused.invalid", "10m")
	store, err := Prepare(context.Background(), cfg, io.Discard)
	if err == nil {
		_ = store.Close()
		t.Fatal("expected error for empty api key")
	}
	if !strings.Contains(err.Error(), "etherscan_api_key required") {
		t.Errorf("error should mention etherscan_api_key: %v", err)
	}
	// No DB should have been created — the cache dir lookup hasn't
	// happened yet, so the chain-id partition dir should not exist.
	dbPath := filepath.Join(filepath.Dir(cfg.Path()), "0xa5e8", "batcher.db")
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Errorf("DB should not be created on early-fail, but found: %s", dbPath)
	}
}

// TestPrepare_TTLHitSkipsEtherscan pre-seeds the cache with a fresh
// last_synced_at and verifies Prepare returns the store without
// hitting either the L1 RPC or Etherscan.
func TestPrepare_TTLHitSkipsEtherscan(t *testing.T) {
	var l1Hits, esHits atomic.Int32
	l1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		l1Hits.Add(1)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0xaa36a7"}`)
	}))
	defer l1.Close()
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		esHits.Add(1)
		fmt.Fprint(w, `{"status":"0","message":"No transactions found","result":[]}`)
	}))
	defer es.Close()

	cfg := loadFixtureConfig(t, "KEY", l1.URL, "10m")
	withTestTransport(t, es.URL)

	// Pre-seed: open the store and commit a fresh page so
	// last_synced_at is well within the 10m TTL.
	preStore, err := batchcache.Open(filepath.Dir(cfg.Path()), "0xa5e8")
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if err := preStore.UpsertPage(1, []etherscan.Tx{{
		BlockNumber: 42, TimeStamp: 1_700_000_000, Hash: "0xabc",
		From: "0x00", To: "0x00", Value: "0", GasUsed: 100, MethodID: "0x00",
		Input: "0x", Status: 1,
	}}); err != nil {
		t.Fatalf("seed UpsertPage: %v", err)
	}
	_ = preStore.Close()

	store, err := Prepare(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer store.Close()
	if l1Hits.Load() != 0 {
		t.Errorf("L1 RPC should NOT be hit on cache HIT, got %d", l1Hits.Load())
	}
	if esHits.Load() != 0 {
		t.Errorf("Etherscan should NOT be hit on cache HIT, got %d", esHits.Load())
	}
}

// TestPrepare_TTLExpiredFetches asserts the opposite path: with a
// tiny TTL and a stale (or empty) cache, Prepare resolves chainID +
// pages Etherscan, and the resulting rows land in the store.
func TestPrepare_TTLExpiredFetches(t *testing.T) {
	var l1Hits, esHits atomic.Int32
	l1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		l1Hits.Add(1)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0xaa36a7"}`)
	}))
	defer l1.Close()
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		esHits.Add(1)
		// Single small page → fetcher terminates after one onPage call.
		fmt.Fprintf(w, `{
		  "status": "1", "message": "OK",
		  "result": [{
		    "blockNumber":"100","timeStamp":"1700000000",
		    "hash":"0x%064x","from":"0xdf","to":"0x00b6",
		    "value":"0","gasUsed":"188432","methodId":"0x6a",
		    "input":"0xdead","txreceipt_status":"1"
		  }]
		}`, 100)
		_ = r
	}))
	defer es.Close()

	cfg := loadFixtureConfig(t, "KEY", l1.URL, "1s")
	withTestTransport(t, es.URL)

	store, err := Prepare(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer store.Close()
	if l1Hits.Load() == 0 {
		t.Error("L1 RPC should be hit on TTL expired")
	}
	if esHits.Load() == 0 {
		t.Error("Etherscan should be hit on TTL expired")
	}
	// And the row actually landed.
	n, err := store.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("Count after fetch: got %d, want 1", n)
	}
}
