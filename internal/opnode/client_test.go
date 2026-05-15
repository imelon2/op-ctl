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

func newJSONServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: got %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestSelf_OK(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"result":{
			"peerID":"16Uiu2HAm",
			"chainID":71235,
			"userAgent":"optimism/v1.7.0",
			"addresses":["/ip4/10.0.0.5/tcp/9003"],
			"protocols":["/eth2/x"]
		}
	}`)
	defer srv.Close()

	peer, latency, err := Self(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	if peer.PeerID != "16Uiu2HAm" {
		t.Errorf("PeerID: got %q", peer.PeerID)
	}
	if peer.ChainID != 71235 {
		t.Errorf("ChainID: got %d", peer.ChainID)
	}
	if latency <= 0 {
		t.Errorf("latency should be > 0, got %v", latency)
	}
}

func TestSelf_HTTPError(t *testing.T) {
	srv := newJSONServer(t, 500, `internal error`)
	defer srv.Close()

	_, _, err := Self(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Errorf("error: %v", err)
	}
}

func TestSelf_RPCError(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"error":{"code":-32601,"message":"method not found"}
	}`)
	defer srv.Close()

	_, _, err := Self(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rpc error -32601") {
		t.Errorf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "method not found") {
		t.Errorf("error: %v", err)
	}
}

func TestSelf_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := Self(ctx, nil, srv.URL)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestSelf_NullResult(t *testing.T) {
	srv := newJSONServer(t, 200, `{"jsonrpc":"2.0","id":1,"result":null}`)
	defer srv.Close()

	_, _, err := Self(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error for null result")
	}
	if !strings.Contains(err.Error(), "rpc result missing") {
		t.Errorf("error: %v", err)
	}
}
