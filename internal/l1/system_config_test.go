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

// wordHex returns the 64-char lowercase hex of n as a 32-byte
// big-endian word — a tiny test helper that mirrors what a JSON-RPC
// node returns for a uint32/uint64/uint256 view method.
func wordHex(n uint64) string {
	s, _ := encodeUint256(new(big.Int).SetUint64(n))
	return s
}

// fakeRPC is a multi-call eth_call mock: it dispatches based on the
// "data" field of each batch entry to a pre-supplied response map.
// Used to exercise FetchSystemConfigSnapshot's branching without a
// live RPC.
type fakeRPC struct {
	byData map[string]string // selector calldata → 0x-prefixed result, "" means revert
}

func (f *fakeRPC) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var reqs []rpcRequest
	if err := json.Unmarshal(body, &reqs); err != nil {
		// Single (non-batched) eth_call path: treated as a 1-element batch.
		var single rpcRequest
		if err2 := json.Unmarshal(body, &single); err2 != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reqs = []rpcRequest{single}
	}
	envs := make([]rpcResponse, len(reqs))
	for i, req := range reqs {
		envs[i].JSONRPC = "2.0"
		envs[i].ID = req.ID
		params, _ := req.Params[0].(map[string]any)
		data, _ := params["data"].(string)
		got, ok := f.byData[data]
		if !ok || got == "" {
			envs[i].Error = &rpcError{Code: -32000, Message: "revert"}
			continue
		}
		raw, _ := json.Marshal(got)
		envs[i].Result = raw
	}
	w.Header().Set("Content-Type", "application/json")
	if len(envs) == 1 && reqs[0].ID == 1 && len(body) > 0 && body[0] != '[' {
		// Caller sent a single request; respond in kind.
		_ = json.NewEncoder(w).Encode(envs[0])
		return
	}
	_ = json.NewEncoder(w).Encode(envs)
}

func TestFetchSystemConfigSnapshot_AllFieldsPopulated(t *testing.T) {
	// Build a fake response set: one result per selector. ResourceConfig
	// is the 6-word tuple; everything else is a single padded word.
	resCfgPayload := "0x" +
		wordHex(1_000_000) + // maxResourceLimit
		wordHex(10) + // elasticityMultiplier
		wordHex(8) + // baseFeeMaxChangeDenominator
		wordHex(1_000_000_000) + // minimumBaseFee
		wordHex(1_000_000) + // systemTxMaxGas
		wordHex(2_000_000_000) // maximumBaseFee

	f := &fakeRPC{byData: map[string]string{
		selectorOf("basefeeScalar()"):        "0x" + wordHex(11),
		selectorOf("blobbasefeeScalar()"):    "0x" + wordHex(22),
		selectorOf("scalar()"):               "0x" + wordHex(333),
		selectorOf("overhead()"):             "0x" + wordHex(444),
		selectorOf("gasLimit()"):             "0x" + wordHex(60_000_000),
		selectorOf("eip1559Denominator()"):   "0x" + wordHex(50),
		selectorOf("eip1559Elasticity()"):    "0x" + wordHex(6),
		selectorOf("operatorFeeScalar()"):    "0x" + wordHex(7),
		selectorOf("operatorFeeConstant()"):  "0x" + wordHex(8),
		selectorOf("daFootprintGasScalar()"): "0x" + wordHex(9),
		selectorOf("minBaseFee()"):           "0x" + wordHex(1_500_000_000),
		selectorOf("resourceConfig()"):       resCfgPayload,
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snap, err := FetchSystemConfigSnapshot(context.Background(), srv.Client(), srv.URL, "0x586fb5eac03e347a9ab109618296d9aad915a2ee")
	if err != nil {
		t.Fatalf("FetchSystemConfigSnapshot: %v", err)
	}
	if len(snap.Errors) != 0 {
		t.Fatalf("expected no per-field errors, got %v", snap.Errors)
	}
	if snap.BasefeeScalar != 11 {
		t.Errorf("BasefeeScalar: got %d, want 11", snap.BasefeeScalar)
	}
	if snap.BlobBasefeeScalar != 22 {
		t.Errorf("BlobBasefeeScalar: got %d, want 22", snap.BlobBasefeeScalar)
	}
	if snap.Scalar.Cmp(big.NewInt(333)) != 0 {
		t.Errorf("Scalar: got %v, want 333", snap.Scalar)
	}
	if snap.Overhead.Cmp(big.NewInt(444)) != 0 {
		t.Errorf("Overhead: got %v, want 444", snap.Overhead)
	}
	if snap.GasLimit != 60_000_000 {
		t.Errorf("GasLimit: got %d, want 60_000_000", snap.GasLimit)
	}
	if snap.EIP1559Denominator != 50 {
		t.Errorf("EIP1559Denominator: got %d, want 50", snap.EIP1559Denominator)
	}
	if snap.EIP1559Elasticity != 6 {
		t.Errorf("EIP1559Elasticity: got %d, want 6", snap.EIP1559Elasticity)
	}
	if snap.OperatorFeeScalar != 7 {
		t.Errorf("OperatorFeeScalar: got %d, want 7", snap.OperatorFeeScalar)
	}
	if snap.OperatorFeeConstant != 8 {
		t.Errorf("OperatorFeeConstant: got %d, want 8", snap.OperatorFeeConstant)
	}
	if snap.DAFootprintGasScalar != 9 {
		t.Errorf("DAFootprintGasScalar: got %d, want 9", snap.DAFootprintGasScalar)
	}
	if snap.MinBaseFee != 1_500_000_000 {
		t.Errorf("MinBaseFee: got %d, want 1_500_000_000", snap.MinBaseFee)
	}
	rc := snap.ResourceConfig
	if rc.MaxResourceLimit != 1_000_000 || rc.ElasticityMultiplier != 10 ||
		rc.BaseFeeMaxChangeDenominator != 8 || rc.MinimumBaseFee != 1_000_000_000 ||
		rc.SystemTxMaxGas != 1_000_000 || rc.MaximumBaseFee.Cmp(big.NewInt(2_000_000_000)) != 0 {
		t.Errorf("ResourceConfig mismatch: %+v", rc)
	}
}

func TestFetchSystemConfigSnapshot_PartialRevertsRecorded(t *testing.T) {
	// Pre-Isthmus deployment: operatorFeeScalar reverts but basefeeScalar
	// still returns a value. The snapshot should record the revert in
	// Errors and leave OperatorFeeScalar at zero, while preserving the
	// fields that succeeded.
	f := &fakeRPC{byData: map[string]string{
		selectorOf("basefeeScalar()"):        "0x" + wordHex(11),
		selectorOf("blobbasefeeScalar()"):    "0x" + wordHex(22),
		selectorOf("operatorFeeScalar()"):    "", // forces revert
		selectorOf("operatorFeeConstant()"):  "", // forces revert
		selectorOf("daFootprintGasScalar()"): "", // forces revert
		selectorOf("minBaseFee()"):           "", // forces revert
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snap, err := FetchSystemConfigSnapshot(context.Background(), srv.Client(), srv.URL, "0x0000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("FetchSystemConfigSnapshot: %v", err)
	}
	if snap.BasefeeScalar != 11 {
		t.Errorf("BasefeeScalar: got %d, want 11", snap.BasefeeScalar)
	}
	if snap.OperatorFeeScalar != 0 {
		t.Errorf("OperatorFeeScalar should be zero on revert, got %d", snap.OperatorFeeScalar)
	}
	if _, ok := snap.Errors["operatorFeeScalar"]; !ok {
		t.Errorf("Errors should record operatorFeeScalar revert; got %v", snap.Errors)
	}
	if _, ok := snap.Errors["minBaseFee"]; !ok {
		t.Errorf("Errors should record minBaseFee revert; got %v", snap.Errors)
	}
}

func TestDecodeResourceConfig_ShortPayload(t *testing.T) {
	if _, err := decodeResourceConfig("0x" + wordHex(1)); err == nil {
		t.Fatal("expected error on short payload")
	} else if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error should mention short result, got %v", err)
	}
}

func TestWordToUint16(t *testing.T) {
	// 0x000...01ff → 511
	w := make([]byte, 32)
	w[30] = 0x01
	w[31] = 0xff
	if got := wordToUint16(w); got != 511 {
		t.Errorf("wordToUint16: got %d, want 511", got)
	}
}
