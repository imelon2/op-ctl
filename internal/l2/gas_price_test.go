package l2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gasFakeServer answers eth_gasPrice / eth_maxPriorityFeePerGas with
// the supplied wei values (as hex quantities). A value of -1 makes
// that method return an RPC error so the failure path is testable.
func gasFakeServer(t *testing.T, gasPrice, tip int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rpcReq
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := rpcResp{JSONRPC: "2.0", ID: req.ID}
		reply := func(v int64) {
			if v < 0 {
				out.Error = &rpcErr{Code: -32000, Message: "unavailable"}
				return
			}
			raw, _ := json.Marshal(fmt.Sprintf("0x%x", v))
			out.Result = raw
		}
		switch req.Method {
		case "eth_gasPrice":
			reply(gasPrice)
		case "eth_maxPriorityFeePerGas":
			reply(tip)
		default:
			out.Error = &rpcErr{Code: -32601, Message: "unknown method"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// TestFetchGasPriceSnapshot_BaseFeeDerived verifies baseFee = gasPrice
// - maxPriorityFeePerGas.
func TestFetchGasPriceSnapshot_BaseFeeDerived(t *testing.T) {
	srv := gasFakeServer(t, 1_000_000_007, 7)
	defer srv.Close()

	s := FetchGasPriceSnapshot(context.Background(), srv.Client(), srv.URL)
	if len(s.Errors) != 0 {
		t.Fatalf("Errors: expected empty, got %v", s.Errors)
	}
	if s.GasPrice.Int64() != 1_000_000_007 {
		t.Errorf("GasPrice: got %v, want 1000000007", s.GasPrice)
	}
	if s.MaxPriorityFee.Int64() != 7 {
		t.Errorf("MaxPriorityFee: got %v, want 7", s.MaxPriorityFee)
	}
	if s.BaseFee == nil || s.BaseFee.Int64() != 1_000_000_000 {
		t.Errorf("BaseFee: got %v, want 1000000000", s.BaseFee)
	}
}

// TestFetchGasPriceSnapshot_PartialError leaves BaseFee nil when one of
// the two inputs fails, and records the per-field error.
func TestFetchGasPriceSnapshot_PartialError(t *testing.T) {
	srv := gasFakeServer(t, 1_000_000_007, -1) // tip call errors
	defer srv.Close()

	s := FetchGasPriceSnapshot(context.Background(), srv.Client(), srv.URL)
	if s.Errors["maxPriorityFee"] == nil {
		t.Error("expected maxPriorityFee error")
	}
	if s.BaseFee != nil {
		t.Errorf("BaseFee should be nil when an input failed, got %v", s.BaseFee)
	}
	if s.GasPrice == nil || s.GasPrice.Int64() != 1_000_000_007 {
		t.Errorf("GasPrice should still be populated, got %v", s.GasPrice)
	}
}

// TestFetchGasPriceSnapshot_EmptyURL surfaces clear per-field errors.
func TestFetchGasPriceSnapshot_EmptyURL(t *testing.T) {
	s := FetchGasPriceSnapshot(context.Background(), nil, "")
	if s.Errors["gasPrice"] == nil || s.Errors["maxPriorityFee"] == nil {
		t.Errorf("expected both errors on empty URL, got %v", s.Errors)
	}
}
