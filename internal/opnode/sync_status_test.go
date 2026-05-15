package opnode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// syncStatusSampleResult is a real optimism_syncStatus reply. It
// retains the pending_safe_l2 / cross_unsafe_l2 / local_safe_l2 keys
// the struct intentionally ignores so the test also exercises the
// "extra fields decode away" path.
const syncStatusSampleResult = `{
	"current_l1": {
		"hash": "0xaaed59bc0661bb999c4ac3c6f52a8bfd75211a615a4fc0cf337294cce49cf385",
		"number": 10847760,
		"parentHash": "0x5f1e28b3eb84fe65a34985cdcb621a227731c3a2efee3ba4839d9e9f87735df9",
		"timestamp": 1778713716
	},
	"current_l1_finalized": {
		"hash": "0xcec0695240fd2e7724f127677d775196d1aef362223b2c34c44d25d7e59b82dc",
		"number": 10847679,
		"parentHash": "0xfafaedfdaa25c0b56db59f234101a7fb2dddf7ba21794b9486058501ba0a470b",
		"timestamp": 1778712672
	},
	"head_l1": {
		"hash": "0xd76e05d74ac226f0fe94aa9cc1da7bc99c1f5f41fa7f30c26d1204622d3e84a1",
		"number": 10847764,
		"parentHash": "0xab32087e716a07d3dbdbd5a1d291c33faf72acc8af9d620341d0c473d64ff09a",
		"timestamp": 1778713776
	},
	"safe_l1": {
		"hash": "0x186a460798dc5e0ea631024d20452aae34bf8aff225825dd1272cf6185d89215",
		"number": 10847708,
		"parentHash": "0x85b6f9be3a361037813b849a2b8807fc5a71cb7ac40b18cfb94a2b9d715728f7",
		"timestamp": 1778713044
	},
	"finalized_l1": {
		"hash": "0xcec0695240fd2e7724f127677d775196d1aef362223b2c34c44d25d7e59b82dc",
		"number": 10847679,
		"parentHash": "0xfafaedfdaa25c0b56db59f234101a7fb2dddf7ba21794b9486058501ba0a470b",
		"timestamp": 1778712672
	},
	"unsafe_l2": {
		"hash": "0x881196b438e9017450ae884120e1d6719fbf8d19e00706225cdc0e45d0852edb",
		"number": 497197,
		"parentHash": "0x42f473d35106284b26ffc045d352dd64dfff543e5898270b36e7d6e7e3f2fca8",
		"timestamp": 1778713789,
		"l1origin": {"hash":"0xd76e05d74ac226f0fe94aa9cc1da7bc99c1f5f41fa7f30c26d1204622d3e84a1","number":10847764},
		"sequenceNumber": 11
	},
	"safe_l2": {
		"hash": "0x43729a8657d7ece18ed3b4769bc4536def4f334c5c34f3fc69a8a023857175d1",
		"number": 497063,
		"parentHash": "0x83d6216773c78dd869fe83e2f1defb3986e996d917fda903d6725c5183294dbc",
		"timestamp": 1778713655,
		"l1origin": {"hash":"0x2325208d7a94ff88d8435407a864699c3a929d4a50c7e1245f986a0e0d9d81f6","number":10847754},
		"sequenceNumber": 21
	},
	"finalized_l2": {
		"hash": "0xe7544f57be376ea23b433b8c7dee162cddb97821818271926c93c32cd2c9128e",
		"number": 496034,
		"parentHash": "0x5eb4c2760313be60bd9405a10648d8b0210b5521c3375ec0d97707fab85728ab",
		"timestamp": 1778712626,
		"l1origin": {"hash":"0x5684d84f6c68a7017515923dc3310014426ab5b39de04b8e7679293574757f33","number":10847675},
		"sequenceNumber": 0
	},
	"pending_safe_l2": {"hash":"0x43729a8657d7ece18ed3b4769bc4536def4f334c5c34f3fc69a8a023857175d1","number":497063,"parentHash":"","timestamp":0},
	"cross_unsafe_l2": {"hash":"0x881196b438e9017450ae884120e1d6719fbf8d19e00706225cdc0e45d0852edb","number":497197,"parentHash":"","timestamp":0},
	"local_safe_l2":   {"hash":"0x43729a8657d7ece18ed3b4769bc4536def4f334c5c34f3fc69a8a023857175d1","number":497063,"parentHash":"","timestamp":0}
}`

func TestSync_OK(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":` + syncStatusSampleResult + `}`
	srv := newJSONServer(t, 200, body)
	defer srv.Close()

	s, latency, err := Sync(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if s == nil {
		t.Fatal("Sync returned nil status with nil error")
	}
	// Cross-check every field the user explicitly asked the TUI to
	// surface; if any future op-node version renames a JSON key the
	// test will fail loudly here rather than silently elsewhere.
	if got, want := s.UnsafeL2.Number, uint64(497197); got != want {
		t.Errorf("UnsafeL2.Number: got %d, want %d", got, want)
	}
	if got, want := s.SafeL2.Number, uint64(497063); got != want {
		t.Errorf("SafeL2.Number: got %d, want %d", got, want)
	}
	if got, want := s.FinalizedL2.Number, uint64(496034); got != want {
		t.Errorf("FinalizedL2.Number: got %d, want %d", got, want)
	}
	if got, want := s.CurrentL1.Number, uint64(10847760); got != want {
		t.Errorf("CurrentL1.Number: got %d, want %d", got, want)
	}
	if got, want := s.CurrentL1Finalized.Number, uint64(10847679); got != want {
		t.Errorf("CurrentL1Finalized.Number: got %d, want %d", got, want)
	}
	if got, want := s.HeadL1.Number, uint64(10847764); got != want {
		t.Errorf("HeadL1.Number: got %d, want %d", got, want)
	}
	if got, want := s.SafeL1.Number, uint64(10847708); got != want {
		t.Errorf("SafeL1.Number: got %d, want %d", got, want)
	}
	if got, want := s.FinalizedL1.Number, uint64(10847679); got != want {
		t.Errorf("FinalizedL1.Number: got %d, want %d", got, want)
	}
	if latency <= 0 {
		t.Errorf("latency: got %v, want > 0", latency)
	}
}

func TestSync_RPCError_MethodNotFound(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"error":{"code":-32601,"message":"the method optimism_syncStatus does not exist"}
	}`)
	defer srv.Close()

	_, _, err := Sync(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected RPC error")
	}
	if !strings.Contains(err.Error(), "rpc error -32601") {
		t.Errorf("error should mention rpc error code: %v", err)
	}
	if !strings.Contains(err.Error(), "optimism_syncStatus") {
		t.Errorf("error should mention method name: %v", err)
	}
}

func TestSync_HTTP500(t *testing.T) {
	srv := newJSONServer(t, 500, `internal error`)
	defer srv.Close()

	_, _, err := Sync(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Errorf("error should mention http status: %v", err)
	}
}

func TestSync_CtxDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := Sync(ctx, nil, srv.URL)
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}
