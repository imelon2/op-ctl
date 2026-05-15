package elnode

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
			"id":"a4de274d3a159e10c2c9a68c326511236381b84c9ec52e72ad6b3d4d4d1d6f7e",
			"name":"op-geth/v1.101315.3-stable",
			"enode":"enode://abc@10.0.0.5:30303",
			"enr":"enr:-Ku4QHqV...",
			"ip":"10.0.0.5",
			"ports":{"discovery":30303,"listener":30303},
			"listenAddr":"[::]:30303",
			"protocols":{"eth":{"network":71235}}
		}
	}`)
	defer srv.Close()

	info, latency, err := Self(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	if info.ID != "a4de274d3a159e10c2c9a68c326511236381b84c9ec52e72ad6b3d4d4d1d6f7e" {
		t.Errorf("ID: got %q", info.ID)
	}
	if !strings.HasPrefix(info.Enode, "enode://") {
		t.Errorf("Enode: got %q", info.Enode)
	}
	if !strings.HasPrefix(info.ENR, "enr:") {
		t.Errorf("ENR: got %q", info.ENR)
	}
	if info.Ports.Listener != 30303 {
		t.Errorf("Ports.Listener: got %d", info.Ports.Listener)
	}
	if len(info.Protocols) == 0 {
		t.Errorf("Protocols should preserve raw payload, got empty")
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
		"error":{"code":-32601,"message":"the method admin_nodeInfo does not exist"}
	}`)
	defer srv.Close()

	_, _, err := Self(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rpc error -32601") {
		t.Errorf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "admin_nodeInfo") {
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
