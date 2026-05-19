package l1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// batchServer accepts EITHER a single rpcRequest object OR an array
// of them and returns a handler-controlled response. The test sets
// `respondToBatch` to a slice of pre-encoded responses (one per id)
// — handler interleaves them in the order received.
type batchServer struct {
	t              *testing.T
	singleResponse string
	batchResponses map[int]string // id → JSON envelope (excluding outer brackets)
}

func (b *batchServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		body = []byte(strings.TrimSpace(string(body)))
		if len(body) > 0 && body[0] == '[' {
			// Batch — parse to find ids and write matching responses.
			var reqs []struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(body, &reqs); err != nil {
				b.t.Fatalf("batch unmarshal: %v", err)
			}
			out := "["
			for i, rq := range reqs {
				if i > 0 {
					out += ","
				}
				if resp, ok := b.batchResponses[rq.ID]; ok {
					out += resp
				} else {
					out += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"no handler"}}`, rq.ID)
				}
			}
			out += "]"
			_, _ = w.Write([]byte(out))
			return
		}
		_, _ = w.Write([]byte(b.singleResponse))
	}
}

func TestVersion(t *testing.T) {
	// version() returns "1.4.0" — encoded as offset (0x20) + length (5) + "1.4.0" padded to 32.
	versionResult := "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" + // offset
		"0000000000000000000000000000000000000000000000000000000000000005" + // length
		"312e342e30000000000000000000000000000000000000000000000000000000" // "1.4.0"
	bs := &batchServer{
		t:              t,
		singleResponse: fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"%s"}`, versionResult),
	}
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	got, _, err := Version(context.Background(), srv.Client(), srv.URL, "0xfeed")
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "1.4.0" {
		t.Errorf("Version: got %q, want %q", got, "1.4.0")
	}
}

func TestGameAtIndex(t *testing.T) {
	// Return: gameType=1, timestamp=1730000000, proxy=0xabcd...1234
	w0 := strings.Repeat("0", 56) + "00000001"                                 // uint32=1 in last 4 bytes
	w1 := strings.Repeat("0", 48) + "0000000067218000"                         // uint64 ≈ 1730000000
	w2 := strings.Repeat("0", 24) + "abcdabcdabcdabcdabcdabcdabcdabcdabcd1234" // 20-byte addr right-padded
	bs := &batchServer{
		t:              t,
		singleResponse: fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%s%s%s"}`, w0, w1, w2),
	}
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	gl, _, err := GameAtIndex(context.Background(), srv.Client(), srv.URL, "0xfeed", 7)
	if err != nil {
		t.Fatalf("GameAtIndex: %v", err)
	}
	if gl.Index != 7 {
		t.Errorf("Index: got %d, want 7", gl.Index)
	}
	if gl.GameType != 1 {
		t.Errorf("GameType: got %d, want 1", gl.GameType)
	}
	if gl.Timestamp != 0x67218000 {
		t.Errorf("Timestamp: got %d, want %d", gl.Timestamp, 0x67218000)
	}
	if gl.Proxy != "0xabcdabcdabcdabcdabcdabcdabcdabcdabcd1234" {
		t.Errorf("Proxy: got %s", gl.Proxy)
	}
}

func TestGameAtIndexBatch_MixedSuccessAndRevert(t *testing.T) {
	// Three calls: indices 0, 1, 2.
	// id=1 (idx 0): success — gameType=1, timestamp=100, proxy=0x...01
	// id=2 (idx 1): revert (RPC error)
	// id=3 (idx 2): success — gameType=2, timestamp=200, proxy=0x...02
	gen := func(gt int, ts int, lastByte byte) string {
		w0 := fmt.Sprintf("%064x", gt)
		w1 := fmt.Sprintf("%064x", ts)
		w2 := strings.Repeat("00", 31) + fmt.Sprintf("%02x", lastByte)
		// Pad address to 20 bytes (12 leading zero bytes already there)
		return "0x" + w0 + w1 + w2
	}
	bs := &batchServer{
		t: t,
		batchResponses: map[int]string{
			1: fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"%s"}`, gen(1, 100, 0x01)),
			2: `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"execution reverted"}}`,
			3: fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"result":"%s"}`, gen(2, 200, 0x02)),
		},
	}
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	listings, errs, _, err := GameAtIndexBatch(context.Background(), srv.Client(), srv.URL, "0xfeed", []uint64{0, 1, 2})
	if err != nil {
		t.Fatalf("GameAtIndexBatch: %v", err)
	}
	if len(listings) != 3 || len(errs) != 3 {
		t.Fatalf("expected 3 listings + 3 errs, got %d/%d", len(listings), len(errs))
	}
	if errs[0] != nil || errs[2] != nil {
		t.Errorf("expected nil errs for [0] and [2], got %v / %v", errs[0], errs[2])
	}
	if errs[1] == nil || !strings.Contains(errs[1].Error(), "execution reverted") {
		t.Errorf("expected revert error at [1], got %v", errs[1])
	}
	if listings[0].GameType != 1 || listings[0].Timestamp != 100 {
		t.Errorf("[0] decode: got gt=%d ts=%d", listings[0].GameType, listings[0].Timestamp)
	}
	if listings[2].GameType != 2 || listings[2].Timestamp != 200 {
		t.Errorf("[2] decode: got gt=%d ts=%d", listings[2].GameType, listings[2].Timestamp)
	}
	if listings[0].Index != 0 || listings[1].Index != 1 || listings[2].Index != 2 {
		t.Errorf("indices not preserved: %d/%d/%d", listings[0].Index, listings[1].Index, listings[2].Index)
	}
}

func TestGameAtIndexBatch_Empty(t *testing.T) {
	listings, errs, _, err := GameAtIndexBatch(context.Background(), nil, "http://x", "0xfeed", nil)
	if err != nil || listings != nil || errs != nil {
		t.Errorf("empty indices should return nil/nil/0/nil, got %v/%v/err=%v", listings, errs, err)
	}
}

func TestEthCallBatch_ReorderedResponses(t *testing.T) {
	// Node responds in reverse order — out slice should still be in
	// request order via id matching.
	bs := &batchServer{
		t: t,
		batchResponses: map[int]string{
			1: `{"jsonrpc":"2.0","id":1,"result":"0x01"}`,
			2: `{"jsonrpc":"2.0","id":2,"result":"0x02"}`,
			3: `{"jsonrpc":"2.0","id":3,"result":"0x03"}`,
		},
	}
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	calls := []EthCallReq{
		{To: "0xa", Data: "0xa"},
		{To: "0xb", Data: "0xb"},
		{To: "0xc", Data: "0xc"},
	}
	got, _, err := EthCallBatch(context.Background(), srv.Client(), srv.URL, calls)
	if err != nil {
		t.Fatalf("EthCallBatch: %v", err)
	}
	if got[0].Result != "0x01" || got[1].Result != "0x02" || got[2].Result != "0x03" {
		t.Errorf("ordering broken: %+v", got)
	}
}
