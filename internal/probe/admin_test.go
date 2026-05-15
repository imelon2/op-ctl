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

func adminOKHandler(after time.Duration, started *int64, barrier *sync.WaitGroup) http.Handler {
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
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{
			"id":"abc","name":"op-geth","enode":"enode://x@1.2.3.4:30303",
			"enr":"enr:-Ku4Q","ip":"1.2.3.4",
			"ports":{"discovery":30303,"listener":30303},
			"listenAddr":"[::]:30303","protocols":{}
		}}`))
	})
}

func TestAdminProbeAll_PreservesInputOrder(t *testing.T) {
	srv := httptest.NewServer(adminOKHandler(0, new(int64), nil))
	defer srv.Close()

	bs := []config.Backend{
		{Name: "zeta", ExecutionRPCURL: srv.URL},
		{Name: "alpha", ExecutionRPCURL: srv.URL},
		{Name: "mid", ExecutionRPCURL: srv.URL},
	}
	probes := AdminProbeAll(context.Background(), time.Second, 0, nil, bs)
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

// Mirrors TestProbeAll_Parallel: a WaitGroup barrier forces all handlers
// to enter before any returns, so a serial fan-out would deadlock.
func TestAdminProbeAll_Parallel(t *testing.T) {
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
		s := httptest.NewServer(adminOKHandler(d, &started, &barrier))
		servers = append(servers, s)
		defer s.Close()
	}

	bs := make([]config.Backend, len(servers))
	for i, s := range servers {
		bs[i] = config.Backend{Name: string(rune('a' + i)), ExecutionRPCURL: s.URL}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	t0 := time.Now()
	probes := AdminProbeAll(ctx, time.Second, 0, nil, bs)
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

func TestAdminProbeAll_PartialFailure(t *testing.T) {
	okSrv := httptest.NewServer(adminOKHandler(0, new(int64), nil))
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
		{Name: "ok", ExecutionRPCURL: okSrv.URL},
		{Name: "err5xx", ExecutionRPCURL: errSrv.URL},
		{Name: "timeout", ExecutionRPCURL: slowSrv.URL},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	probes := AdminProbeAll(ctx, 50*time.Millisecond, 0, nil, bs)

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

// TestAdminProbeAll_RetryEventuallySucceeds: server fails 2x then
// succeeds; maxRetries=3 must surface OK=true with hits==3.
func TestAdminProbeAll_RetryEventuallySucceeds(t *testing.T) {
	withShortRetryBackoff(t)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if n <= 2 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{
			"id":"abc","name":"op-geth","enode":"enode://x@1.2.3.4:30303",
			"enr":"enr:-Ku4Q","ip":"1.2.3.4",
			"ports":{"discovery":30303,"listener":30303},
			"listenAddr":"[::]:30303","protocols":{}
		}}`))
	}))
	defer srv.Close()

	bs := []config.Backend{{Name: "flaky", ExecutionRPCURL: srv.URL}}
	probes := AdminProbeAll(context.Background(), time.Second, 3, nil, bs)

	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("server hits: got %d, want 3", got)
	}
	if !probes[0].OK {
		t.Errorf("probe should succeed after retries; got err=%v", probes[0].Err)
	}
}
