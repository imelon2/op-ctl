package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"op-ctl/internal/config"
)

// flakyHandler returns a handler that fails (HTTP 500) on its first
// `failuresBefore` invocations and serves a valid opp2p_self body
// afterwards. The counter is incremented per request so callers can
// assert how many retries actually happened.
func flakyHandler(failuresBefore int, hits *int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(hits, 1)
		if int(n) <= failuresBefore {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"peerID":"p","chainID":1}}`))
	})
}

func withShortRetryBackoff(t *testing.T) {
	t.Helper()
	orig := retryBackoff
	retryBackoff = 5 * time.Millisecond
	t.Cleanup(func() { retryBackoff = orig })
}

func okResponseHandler(after time.Duration, started *int64, barrier *sync.WaitGroup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(started, 1)
		if barrier != nil {
			barrier.Done()
			barrier.Wait()
		}
		if after > 0 {
			time.Sleep(after)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"peerID":"p","chainID":1}}`))
	})
}

func TestProbeAll_PreservesInputOrder(t *testing.T) {
	srv := httptest.NewServer(okResponseHandler(0, new(int64), nil))
	defer srv.Close()

	bs := []config.Backend{
		{Name: "zeta", ConsensusRPCURL: srv.URL},
		{Name: "alpha", ConsensusRPCURL: srv.URL},
		{Name: "mid", ConsensusRPCURL: srv.URL},
	}
	probes := ProbeAll(context.Background(), time.Second, 0, nil, bs)
	if len(probes) != 3 {
		t.Fatalf("len: got %d, want 3", len(probes))
	}
	want := []string{"zeta", "alpha", "mid"}
	for i := range want {
		if probes[i].Backend != want[i] {
			t.Errorf("position %d: got %q, want %q", i, probes[i].Backend, want[i])
		}
	}
}

// TestProbeAll_Parallel uses a strict WaitGroup barrier: every handler
// blocks until all three have entered, then each sleeps for a different
// duration before responding. If ProbeAll runs serially the test deadlocks
// because handler #1 never returns until handler #2 enters, which can't
// happen until handler #1 returns.
//
// The 600ms wall-clock gate equals exactly the sum (100+200+300), so any
// serial execution must exceed it. We also assert all 3 started before any
// finished via the atomic counter.
func TestProbeAll_Parallel(t *testing.T) {
	durations := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
	}

	var started int64
	var barrier sync.WaitGroup
	barrier.Add(len(durations))

	var servers []*httptest.Server
	for _, d := range durations {
		s := httptest.NewServer(okResponseHandler(d, &started, &barrier))
		servers = append(servers, s)
		defer s.Close()
	}

	bs := make([]config.Backend, len(servers))
	for i, s := range servers {
		bs[i] = config.Backend{Name: string(rune('a' + i)), ConsensusRPCURL: s.URL}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	t0 := time.Now()
	probes := ProbeAll(ctx, time.Second, 0, nil, bs)
	elapsed := time.Since(t0)

	if got := atomic.LoadInt64(&started); got != int64(len(durations)) {
		t.Fatalf("started count: got %d, want %d", got, len(durations))
	}
	if elapsed >= 600*time.Millisecond {
		t.Errorf("elapsed %v >= 600ms sum — execution looks serial", elapsed)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("elapsed %v < 300ms — slowest handler skipped?", elapsed)
	}
	for _, p := range probes {
		if !p.OK {
			t.Errorf("probe %s: %v", p.Backend, p.Err)
		}
	}
}

func TestProbeAll_PartialFailure(t *testing.T) {
	okSrv := httptest.NewServer(okResponseHandler(0, new(int64), nil))
	defer okSrv.Close()

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer slowSrv.Close()

	bs := []config.Backend{
		{Name: "ok", ConsensusRPCURL: okSrv.URL},
		{Name: "err5xx", ConsensusRPCURL: errSrv.URL},
		{Name: "timeout", ConsensusRPCURL: slowSrv.URL},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	probes := ProbeAll(ctx, 50*time.Millisecond, 0, nil, bs)

	if len(probes) != 3 {
		t.Fatalf("len: got %d, want 3", len(probes))
	}
	if !probes[0].OK || probes[0].Result == nil {
		t.Errorf("first should be OK, got OK=%v err=%v", probes[0].OK, probes[0].Err)
	}
	if probes[1].OK || probes[1].Err == nil {
		t.Errorf("second should fail with HTTP error, got OK=%v err=%v", probes[1].OK, probes[1].Err)
	}
	if probes[2].OK || probes[2].Err == nil {
		t.Errorf("third should time out, got OK=%v err=%v", probes[2].OK, probes[2].Err)
	}
}

// TestProbeAll_RetryEventuallySucceeds: server returns HTTP 500 on
// the first 2 hits and a valid body on the 3rd. With maxRetries=3
// (so up to 4 attempts) the probe must succeed and the server must
// have been hit exactly 3 times.
func TestProbeAll_RetryEventuallySucceeds(t *testing.T) {
	withShortRetryBackoff(t)
	var hits int64
	srv := httptest.NewServer(flakyHandler(2, &hits))
	defer srv.Close()

	bs := []config.Backend{{Name: "flaky", ConsensusRPCURL: srv.URL}}
	probes := ProbeAll(context.Background(), time.Second, 3, nil, bs)

	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("server hits: got %d, want 3 (2 failures + 1 success)", got)
	}
	if !probes[0].OK {
		t.Errorf("probe should succeed after retries; got err=%v", probes[0].Err)
	}
}

// TestProbeAll_RetryExhausted: server always 500s; maxRetries=2 yields
// 3 total attempts and the probe must report OK=false.
func TestProbeAll_RetryExhausted(t *testing.T) {
	withShortRetryBackoff(t)
	var hits int64
	srv := httptest.NewServer(flakyHandler(1<<30, &hits))
	defer srv.Close()

	bs := []config.Backend{{Name: "always-bad", ConsensusRPCURL: srv.URL}}
	probes := ProbeAll(context.Background(), time.Second, 2, nil, bs)

	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("server hits: got %d, want 3 (maxRetries=2 → 3 attempts)", got)
	}
	if probes[0].OK {
		t.Errorf("probe should fail after exhausting retries; got OK=true")
	}
	if probes[0].Err == nil {
		t.Errorf("probe should carry last attempt's error")
	}
}
