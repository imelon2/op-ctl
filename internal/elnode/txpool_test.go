package elnode

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTxPool_OK(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"result":{"pending":"0x10","queued":"0x2"}
	}`)
	defer srv.Close()

	s, latency, err := TxPool(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("TxPool: %v", err)
	}
	if s == nil {
		t.Fatal("TxPool returned nil status with nil error")
	}
	if got, want := s.Pending, uint64(16); got != want {
		t.Errorf("Pending: got %d, want %d", got, want)
	}
	if got, want := s.Queued, uint64(2); got != want {
		t.Errorf("Queued: got %d, want %d", got, want)
	}
	if latency <= 0 {
		t.Errorf("latency: got %v, want > 0", latency)
	}
}

func TestTxPool_EmptyHexDecodesAsZero(t *testing.T) {
	// Some op-geth versions return "" instead of "0x0" for an empty
	// pool. The parser must accept it without erroring.
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"result":{"pending":"","queued":"0x0"}
	}`)
	defer srv.Close()

	s, _, err := TxPool(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("TxPool: %v", err)
	}
	if s.Pending != 0 || s.Queued != 0 {
		t.Errorf("expected both counters 0, got pending=%d queued=%d", s.Pending, s.Queued)
	}
}

func TestTxPool_RPCError_MethodNotFound(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"error":{"code":-32601,"message":"the method txpool_status does not exist"}
	}`)
	defer srv.Close()

	_, _, err := TxPool(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected RPC error")
	}
	if !IsMethodNotFound(err) {
		t.Errorf("err should match IsMethodNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "txpool_status") {
		t.Errorf("error should mention method name: %v", err)
	}
}

func TestTxPool_HTTP500(t *testing.T) {
	srv := newJSONServer(t, 500, `internal error`)
	defer srv.Close()

	_, _, err := TxPool(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Errorf("error should mention http status: %v", err)
	}
}

func TestTxPool_CtxDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := TxPool(ctx, nil, srv.URL)
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestTxPool_MalformedHex(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"result":{"pending":"0xZZZ","queued":"0x0"}
	}`)
	defer srv.Close()

	_, _, err := TxPool(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected decode error for malformed hex")
	}
	if !strings.Contains(err.Error(), "decode pending") {
		t.Errorf("error should mention 'decode pending': %v", err)
	}
}

// ---------- TxPoolContent ----------

// rawTxBody builds a single-tx wire payload inline so the multi-tx
// fixtures stay readable. Only the fields the parser cares about are
// set; defaults for the rest cover the "every field decodes" path.
func rawTxBody(hash, from, to, nonce, gas, value, gasPrice string) string {
	return `{"hash":"` + hash + `","from":"` + from + `","to":"` + to + `","nonce":"` + nonce + `","gas":"` + gas +
		`","value":"` + value + `","gasPrice":"` + gasPrice + `","maxFeePerGas":"` + gasPrice + `","maxPriorityFeePerGas":"0x1","type":"0x2","chainId":"0xa5e8","input":"0x","accessList":[],"r":"0x0","s":"0x0","v":"0x1","yParity":"0x1","blockHash":null,"blockNumber":null,"transactionIndex":null}`
}

func TestTxPoolContent_OK(t *testing.T) {
	// 2 senders × 2 nonces with mixed pending + queued. Sort
	// contract: Pending desc → From asc → Nonce asc.
	body := `{"jsonrpc":"2.0","id":1,"result":{
		"pending":{
			"0xaaaa":{
				"1":` + rawTxBody("0xh1", "0xaaaa", "0xrecv", "0x1", "0x5208", "0x10", "0x12a05f200") + `,
				"3":` + rawTxBody("0xh3", "0xaaaa", "0xrecv", "0x3", "0x5208", "0x20", "0x12a05f200") + `
			},
			"0xbbbb":{
				"7":` + rawTxBody("0xh7", "0xbbbb", "0xrecv", "0x7", "0x5208", "0x40", "0x12a05f200") + `
			}
		},
		"queued":{
			"0xcccc":{
				"99":` + rawTxBody("0xh99", "0xcccc", "0xrecv", "0x63", "0x5208", "0x80", "0x12a05f200") + `
			}
		}
	}}`
	srv := newJSONServer(t, 200, body)
	defer srv.Close()

	txs, latency, err := TxPoolContent(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("TxPoolContent: %v", err)
	}
	if got, want := len(txs), 4; got != want {
		t.Fatalf("len(txs): got %d, want %d", got, want)
	}
	// Pending desc: indices 0..2 must be pending, 3 queued.
	for i := 0; i < 3; i++ {
		if !txs[i].Pending {
			t.Errorf("txs[%d].Pending: got false, want true", i)
		}
	}
	if txs[3].Pending {
		t.Errorf("txs[3].Pending: got true, want false (queued sender)")
	}
	// Within pending: From asc → 0xaaaa rows before 0xbbbb.
	if txs[0].From != "0xaaaa" || txs[0].Nonce != 1 {
		t.Errorf("txs[0]: got from=%q nonce=%d, want 0xaaaa/1", txs[0].From, txs[0].Nonce)
	}
	if txs[1].From != "0xaaaa" || txs[1].Nonce != 3 {
		t.Errorf("txs[1]: got from=%q nonce=%d, want 0xaaaa/3", txs[1].From, txs[1].Nonce)
	}
	if txs[2].From != "0xbbbb" || txs[2].Nonce != 7 {
		t.Errorf("txs[2]: got from=%q nonce=%d, want 0xbbbb/7", txs[2].From, txs[2].Nonce)
	}
	// Queued row.
	if txs[3].From != "0xcccc" || txs[3].Nonce != 99 {
		t.Errorf("txs[3]: got from=%q nonce=%d, want 0xcccc/99", txs[3].From, txs[3].Nonce)
	}
	// Spot-check that hex fields decoded correctly.
	if txs[2].Value.Cmp(big.NewInt(0x40)) != 0 {
		t.Errorf("txs[2].Value: got %s, want 64 (0x40)", txs[2].Value)
	}
	if latency <= 0 {
		t.Errorf("latency: got %v, want > 0", latency)
	}
}

func TestTxPoolContent_Empty(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"pending":{},"queued":{}}}`
	srv := newJSONServer(t, 200, body)
	defer srv.Close()

	txs, _, err := TxPoolContent(context.Background(), nil, srv.URL)
	if err != nil {
		t.Fatalf("TxPoolContent: %v", err)
	}
	if len(txs) != 0 {
		t.Errorf("empty pool should return len=0, got %d", len(txs))
	}
}

func TestTxPoolContent_MethodNotFound(t *testing.T) {
	srv := newJSONServer(t, 200, `{
		"jsonrpc":"2.0","id":1,
		"error":{"code":-32601,"message":"the method txpool_content does not exist"}
	}`)
	defer srv.Close()

	_, _, err := TxPoolContent(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected RPC error")
	}
	if !IsMethodNotFound(err) {
		t.Errorf("err should match IsMethodNotFound, got %v", err)
	}
}

func TestTxPoolContent_HTTP500(t *testing.T) {
	srv := newJSONServer(t, 500, `internal error`)
	defer srv.Close()

	_, _, err := TxPoolContent(context.Background(), nil, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Errorf("error should mention http status: %v", err)
	}
}

func TestTxPoolContent_CtxDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := TxPoolContent(ctx, nil, srv.URL)
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}
