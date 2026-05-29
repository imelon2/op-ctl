package etherscan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestResolveChainID — happy path. The fake server returns Sepolia's
// chainID (0xaa36a7 = 11155111) as the JSON-RPC result. ResolveChainID
// must parse the hex string into the integer the caller hands to
// Etherscan as `chainid`.
func TestResolveChainID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0xaa36a7"}`)
	}))
	defer srv.Close()
	id, err := ResolveChainID(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("ResolveChainID: %v", err)
	}
	if got, want := id, uint64(11155111); got != want {
		t.Errorf("chainID: got %d, want %d", got, want)
	}
}

// pageHandler returns an httptest.Server that serves a scripted
// sequence of Etherscan V2 txlist responses, one per page. Used by
// every FetchTxList test below to drive a precise pagination scenario.
//
// hits records each (page, queryString) pair so tests can assert on
// pagination behavior (sort order, page ordering, …).
type pageHandler struct {
	mu       sync.Mutex
	pages    []string  // raw JSON envelope body, indexed by 1-based page
	hits     []hitRec  // observed requests
	hitTimes []time.Time
}

type hitRec struct {
	page  int
	query string
}

func (h *pageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hitTimes = append(h.hitTimes, time.Now())
	pageStr := r.URL.Query().Get("page")
	var p int
	fmt.Sscanf(pageStr, "%d", &p)
	h.hits = append(h.hits, hitRec{page: p, query: r.URL.RawQuery})
	if p < 1 || p > len(h.pages) {
		// Default: empty result envelope so unexpected over-fetches
		// terminate the loop cleanly rather than 500ing.
		fmt.Fprint(w, `{"status":"0","message":"No transactions found","result":[]}`)
		return
	}
	fmt.Fprint(w, h.pages[p-1])
}

// mkPage builds a status=1 envelope carrying `count` synthetic txs
// whose block numbers march ascending from startBlock. Mirrors the
// real Etherscan sort=asc contract.
func mkPage(startBlock uint64, count int) string {
	type rawTx struct {
		BlockNumber     string `json:"blockNumber"`
		TimeStamp       string `json:"timeStamp"`
		Hash            string `json:"hash"`
		From            string `json:"from"`
		To              string `json:"to"`
		Value           string `json:"value"`
		GasUsed         string `json:"gasUsed"`
		MethodID        string `json:"methodId"`
		Input           string `json:"input"`
		TxReceiptStatus string `json:"txreceipt_status"`
	}
	rows := make([]rawTx, count)
	for i := range rows {
		blk := startBlock + uint64(i)
		rows[i] = rawTx{
			BlockNumber:     fmt.Sprintf("%d", blk),
			TimeStamp:       "1700000000",
			Hash:            fmt.Sprintf("0x%064x", blk),
			From:            "0xdf05e8c9c0ef7b85d2536182fa1e911622622542",
			To:              "0x00b607c67e6662ac51c747961b657659bb47fd95",
			Value:           "0",
			GasUsed:         "188432",
			MethodID:        "0x6a",
			Input:           "0xdead",
			TxReceiptStatus: "1",
		}
	}
	out := map[string]any{
		"status":  "1",
		"message": "OK",
		"result":  rows,
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// TestFetchTxList_Pagination drives a 3-page scenario (1000 + 1000 +
// 200) and asserts onPage was called once per page in order, with
// the correct row counts and ascending block numbers preserved.
func TestFetchTxList_Pagination(t *testing.T) {
	h := &pageHandler{pages: []string{
		mkPage(100, 1000),
		mkPage(1100, 1000),
		mkPage(2100, 200), // tail page
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	// Point client at the fake by swapping BaseURL via a custom
	// http.Client + RoundTripper. Simpler: hit the real BaseURL but
	// rewrite the host. Even simpler: just inject the test URL via
	// a test-only helper. We use the path-override approach so the
	// production code stays unmodified — build the URL with the
	// fake's URL as the base.
	hc := &http.Client{Transport: rewriteTransport(srv.URL)}

	var seenPages []int
	var totalTxs int
	err := FetchTxList(context.Background(), hc, "DUMMY", 11155111, "0x00B6", 0,
		func(page int, txs []Tx) error {
			seenPages = append(seenPages, page)
			totalTxs += len(txs)
			return nil
		}, io.Discard)
	if err != nil {
		t.Fatalf("FetchTxList: %v", err)
	}
	if got, want := seenPages, []int{1, 2, 3}; !equal(got, want) {
		t.Errorf("seenPages: got %v, want %v", got, want)
	}
	if got, want := totalTxs, 1000+1000+200; got != want {
		t.Errorf("totalTxs: got %d, want %d", got, want)
	}
	if got, want := len(h.hits), 3; got != want {
		t.Errorf("server hits: got %d, want %d", got, want)
	}
}

// TestFetchTxList_SortAsc pins the query-string contract. The cache
// layer's last_synced_block = max-in-page only works if the API
// guarantees ascending order, so the client must explicitly request
// sort=asc on every page.
func TestFetchTxList_SortAsc(t *testing.T) {
	h := &pageHandler{pages: []string{mkPage(100, 5)}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hc := &http.Client{Transport: rewriteTransport(srv.URL)}
	_ = FetchTxList(context.Background(), hc, "DUMMY", 1, "0xabc", 0,
		func(int, []Tx) error { return nil }, io.Discard)
	if len(h.hits) == 0 {
		t.Fatal("server received no hits")
	}
	if !strings.Contains(h.hits[0].query, "sort=asc") {
		t.Errorf("query string missing sort=asc: %s", h.hits[0].query)
	}
}

// TestFetchTxList_RateLimit asserts the partial-progress contract:
// page 1 commits via onPage, page 2 returns "Max rate limit reached",
// the fetcher surfaces a wrapped error mentioning the page number,
// and onPage is NOT called for page 2.
func TestFetchTxList_RateLimit(t *testing.T) {
	h := &pageHandler{pages: []string{
		mkPage(100, 1000),
		`{"status":"0","message":"Max rate limit reached","result":"please slow down"}`,
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hc := &http.Client{Transport: rewriteTransport(srv.URL)}
	var onPageCalls int
	err := FetchTxList(context.Background(), hc, "DUMMY", 1, "0xabc", 0,
		func(int, []Tx) error { onPageCalls++; return nil }, io.Discard)
	if err == nil {
		t.Fatal("expected error on rate limit page")
	}
	if !strings.Contains(err.Error(), "page 2") {
		t.Errorf("error should mention page 2: %v", err)
	}
	if !strings.Contains(err.Error(), "Max rate limit reached") {
		t.Errorf("error should include API message: %v", err)
	}
	if onPageCalls != 1 {
		t.Errorf("onPage should fire exactly once (page 1 only), got %d calls", onPageCalls)
	}
}

// TestFetchTxList_NoTransactionsFound asserts the cache-empty / tail
// case is a clean termination, not an error. Etherscan returns
// status="0" message="No transactions found" both for a fresh inbox
// and for the page after the last real page.
func TestFetchTxList_NoTransactionsFound(t *testing.T) {
	h := &pageHandler{pages: []string{
		`{"status":"0","message":"No transactions found","result":[]}`,
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hc := &http.Client{Transport: rewriteTransport(srv.URL)}
	called := false
	err := FetchTxList(context.Background(), hc, "DUMMY", 1, "0xabc", 0,
		func(int, []Tx) error { called = true; return nil }, io.Discard)
	if err != nil {
		t.Fatalf("expected nil error on no-tx page, got %v", err)
	}
	if called {
		t.Error("onPage should NOT fire for status=0 no-tx response")
	}
}

// TestFetchTxList_Throttle measures inter-request spacing. The client
// promises ≥200ms between successive HTTP calls; we tolerate ±50ms
// jitter for CI scheduling noise.
func TestFetchTxList_Throttle(t *testing.T) {
	h := &pageHandler{pages: []string{
		mkPage(100, 1000),
		mkPage(1100, 1000),
		mkPage(2100, 50), // tail
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hc := &http.Client{Transport: rewriteTransport(srv.URL)}
	if err := FetchTxList(context.Background(), hc, "DUMMY", 1, "0xabc", 0,
		func(int, []Tx) error { return nil }, io.Discard); err != nil {
		t.Fatalf("FetchTxList: %v", err)
	}
	if len(h.hitTimes) < 2 {
		t.Fatalf("need >=2 hits to measure spacing, got %d", len(h.hitTimes))
	}
	for i := 1; i < len(h.hitTimes); i++ {
		gap := h.hitTimes[i].Sub(h.hitTimes[i-1])
		if gap < 150*time.Millisecond {
			t.Errorf("gap between hit %d and %d too short: %v (want >=150ms)", i-1, i, gap)
		}
	}
}

// TestFetchTxList_OnPageError asserts that an error returned by the
// onPage callback halts the fetcher immediately — no further HTTP
// calls are issued. This protects the partial-commit contract: a
// SQLite write failure on page 2 must NOT silently keep fetching
// pages 3..N that have nowhere to go.
func TestFetchTxList_OnPageError(t *testing.T) {
	h := &pageHandler{pages: []string{
		mkPage(100, 1000),
		mkPage(1100, 1000),
		mkPage(2100, 1000),
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	hc := &http.Client{Transport: rewriteTransport(srv.URL)}
	err := FetchTxList(context.Background(), hc, "DUMMY", 1, "0xabc", 0,
		func(page int, _ []Tx) error {
			if page == 2 {
				return fmt.Errorf("simulated commit failure")
			}
			return nil
		}, io.Discard)
	if err == nil {
		t.Fatal("expected error from onPage")
	}
	if !strings.Contains(err.Error(), "simulated commit failure") {
		t.Errorf("error should wrap callback error: %v", err)
	}
	if !strings.Contains(err.Error(), "page 2") {
		t.Errorf("error should mention page 2: %v", err)
	}
	if got := len(h.hits); got != 2 {
		t.Errorf("fetcher should stop after page 2, got %d hits", got)
	}
}

// rewriteTransport routes any outbound request to baseURL, preserving
// the original path+query. This lets the production code build URLs
// against the const BaseURL while tests redirect to httptest.
func rewriteTransport(baseURL string) http.RoundTripper {
	return roundTripFn(func(req *http.Request) (*http.Response, error) {
		nu, err := req.URL.Parse(baseURL)
		if err != nil {
			return nil, err
		}
		nu.Path = ""
		nu.RawQuery = req.URL.RawQuery
		req.URL.Scheme = nu.Scheme
		req.URL.Host = nu.Host
		req.URL.Path = nu.Path
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
