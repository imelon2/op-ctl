package l1

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureRequest is a tiny test server that records the JSON-RPC body
// of the most recent POST and returns a caller-supplied response.
type captureRequest struct {
	body []byte
	resp string
	code int
}

func newServer(t *testing.T, cap *captureRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.body = body
		if cap.code == 0 {
			cap.code = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cap.code)
		_, _ = w.Write([]byte(cap.resp))
	}))
}

func TestGameCount_Success(t *testing.T) {
	// 0x...05 = 5 games
	cap := &captureRequest{
		resp: `{"jsonrpc":"2.0","id":1,"result":"0x0000000000000000000000000000000000000000000000000000000000000005"}`,
	}
	srv := newServer(t, cap)
	defer srv.Close()

	n, _, err := GameCount(context.Background(), srv.Client(), srv.URL, "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239")
	if err != nil {
		t.Fatalf("GameCount: %v", err)
	}
	if n.Cmp(big.NewInt(5)) != 0 {
		t.Errorf("count: got %v, want 5", n)
	}

	// Verify the on-wire request shape.
	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	if err := json.Unmarshal(cap.body, &req); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	if req.Method != "eth_call" {
		t.Errorf("method: got %q, want eth_call", req.Method)
	}
	if len(req.Params) != 2 {
		t.Fatalf("params length: got %d, want 2", len(req.Params))
	}
	call, _ := req.Params[0].(map[string]any)
	if call["data"] != gameCountSelector {
		t.Errorf("data selector: got %v, want %s", call["data"], gameCountSelector)
	}
	if !strings.EqualFold(call["to"].(string), "0x9b6709999e8fd16cae9e27bd0e7cf4b747097239") {
		t.Errorf("to: got %v", call["to"])
	}
	if req.Params[1] != "latest" {
		t.Errorf("block tag: got %v, want latest", req.Params[1])
	}
}

func TestGameCount_LargeNumber(t *testing.T) {
	// 0xff...ff (max uint256) — verify big.Int handles full width.
	cap := &captureRequest{
		resp: `{"jsonrpc":"2.0","id":1,"result":"0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`,
	}
	srv := newServer(t, cap)
	defer srv.Close()

	n, _, err := GameCount(context.Background(), srv.Client(), srv.URL, "0xabc")
	if err != nil {
		t.Fatalf("GameCount: %v", err)
	}
	want := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	if n.Cmp(want) != 0 {
		t.Errorf("count: got %v, want %v", n, want)
	}
}

func TestGameCount_EmptyURL(t *testing.T) {
	_, _, err := GameCount(context.Background(), nil, "", "0xabc")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "l1_rpc_url is empty") {
		t.Errorf("error: %v", err)
	}
}

func TestGameCount_EmptyAddress(t *testing.T) {
	_, _, err := GameCount(context.Background(), nil, "http://x", "")
	if err == nil {
		t.Fatal("expected error for empty address")
	}
	if !strings.Contains(err.Error(), "DisputeGameFactoryProxy address is empty") {
		t.Errorf("error: %v", err)
	}
}

func TestGameCount_RPCError(t *testing.T) {
	cap := &captureRequest{
		resp: `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"execution reverted"}}`,
	}
	srv := newServer(t, cap)
	defer srv.Close()
	_, _, err := GameCount(context.Background(), srv.Client(), srv.URL, "0xabc")
	if err == nil {
		t.Fatal("expected RPC error")
	}
	if !strings.Contains(err.Error(), "execution reverted") {
		t.Errorf("error: %v", err)
	}
}

func TestGameCount_EmptyResultRejected(t *testing.T) {
	cap := &captureRequest{
		resp: `{"jsonrpc":"2.0","id":1,"result":"0x"}`,
	}
	srv := newServer(t, cap)
	defer srv.Close()
	_, _, err := GameCount(context.Background(), srv.Client(), srv.URL, "0xabc")
	if err == nil {
		t.Fatal("expected error for empty 0x result")
	}
	if !strings.Contains(err.Error(), "contract returned no data") {
		t.Errorf("error: %v", err)
	}
}

func TestGameCount_HTTPError(t *testing.T) {
	cap := &captureRequest{
		resp: `<html>oops</html>`,
		code: 500,
	}
	srv := newServer(t, cap)
	defer srv.Close()
	_, _, err := GameCount(context.Background(), srv.Client(), srv.URL, "0xabc")
	if err == nil {
		t.Fatal("expected error for non-2xx HTTP")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Errorf("error: %v", err)
	}
}

func TestDecodeUint256_Padding(t *testing.T) {
	// Short input — well-formed eth_call always returns 32 bytes,
	// but the decoder should tolerate odd-length and short payloads.
	tests := []struct {
		in   string
		want int64
	}{
		{"0x0", 0},
		{"0x1", 1},
		{"0xff", 255},
		{"0x0000000000000000000000000000000000000000000000000000000000000005", 5},
	}
	for _, tc := range tests {
		got, err := decodeUint256(tc.in)
		if err != nil {
			t.Errorf("decode %q: %v", tc.in, err)
			continue
		}
		if got.Int64() != tc.want {
			t.Errorf("decode %q: got %v, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDecodeUint256_TooLong(t *testing.T) {
	// 65 hex chars (over 32 bytes) — must be rejected.
	_, err := decodeUint256("0x" + strings.Repeat("f", 65))
	if err == nil {
		t.Fatal("expected error for over-width hex")
	}
}
