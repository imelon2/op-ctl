package l2

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// versionResultHex encodes s as an ABI `string` return value:
// head offset(=0x20) + length + right-padded UTF-8 bytes.
func versionResultHex(s string) string {
	payloadLen := ((len(s) + 31) / 32) * 32
	out := make([]byte, 32+32+payloadLen)
	out[31] = 0x20             // offset = 32
	out[63] = byte(len(s))     // length (assumes s < 256 chars — fine for semver)
	copy(out[64:], []byte(s))  // payload, right-padded
	return "0x" + hex.EncodeToString(out)
}

// gpoFakeRPC handles version() eth_call lookups by 4-byte selector.
// Missing or empty values revert; anything else returns the mapped
// hex string.
type gpoFakeRPC struct {
	bySelector map[string]string
}

func (f *gpoFakeRPC) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var batch []rpcReq
	if err := json.Unmarshal(body, &batch); err == nil && len(batch) > 0 {
		envs := make([]rpcResp, len(batch))
		for i, req := range batch {
			envs[i] = f.respondOne(req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envs)
		return
	}
	var single rpcReq
	if err := json.Unmarshal(body, &single); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.respondOne(single))
}

func (f *gpoFakeRPC) respondOne(req rpcReq) rpcResp {
	out := rpcResp{JSONRPC: "2.0", ID: req.ID}
	if req.Method != "eth_call" {
		out.Error = &rpcErr{Code: -32601, Message: "unknown method"}
		return out
	}
	params, _ := req.Params[0].(map[string]any)
	data, _ := params["data"].(string)
	key := strings.ToLower(data)
	if len(key) < 10 {
		out.Error = &rpcErr{Code: -32602, Message: "bad data"}
		return out
	}
	val, ok := f.bySelector[key[:10]]
	if !ok || val == "" {
		out.Error = &rpcErr{Code: -32000, Message: "revert"}
		return out
	}
	raw, _ := json.Marshal(val)
	out.Result = raw
	return out
}

// TestFetchGasPriceOracleSnapshot_VersionMatchesPinned verifies the
// happy path: hardcoded constants surface as-is, and the deployed
// contract reports the same semver as our pinned source version.
func TestFetchGasPriceOracleSnapshot_VersionMatchesPinned(t *testing.T) {
	f := &gpoFakeRPC{bySelector: map[string]string{
		baseFeeSelector:           "0x" + wordHex(7),
		l1BaseFeeSelector:         "0x" + wordHex(1_000_000_000),
		blobBaseFeeSelector:       "0x" + wordHex(1),
		baseFeeScalarSelector:     "0x" + wordHex(1368),
		blobBaseFeeScalarSelector: "0x" + wordHex(810949),
		decimalsSelector:          "0x" + wordHex(6),
		versionSelector:           versionResultHex(GasPriceOracleConstantsSourceVersion),
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snap, err := FetchGasPriceOracleSnapshot(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchGasPriceOracleSnapshot: %v", err)
	}
	if len(snap.Errors) != 0 {
		t.Fatalf("snap.Errors: expected empty, got %v", snap.Errors)
	}
	if snap.BaseFee == nil || snap.BaseFee.Cmp(big.NewInt(7)) != 0 {
		t.Errorf("BaseFee: got %v, want 7", snap.BaseFee)
	}
	if snap.L1BaseFee == nil || snap.L1BaseFee.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Errorf("L1BaseFee: got %v, want 1000000000", snap.L1BaseFee)
	}
	if snap.BlobBaseFee == nil || snap.BlobBaseFee.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("BlobBaseFee: got %v, want 1", snap.BlobBaseFee)
	}
	if snap.BaseFeeScalar != 1368 {
		t.Errorf("BaseFeeScalar: got %d, want 1368", snap.BaseFeeScalar)
	}
	if snap.BlobBaseFeeScalar != 810949 {
		t.Errorf("BlobBaseFeeScalar: got %d, want 810949", snap.BlobBaseFeeScalar)
	}
	if snap.Decimals == nil || snap.Decimals.Cmp(big.NewInt(6)) != 0 {
		t.Errorf("Decimals: got %v, want 6", snap.Decimals)
	}
	if snap.CostIntercept != GasPriceOracleCostIntercept {
		t.Errorf("CostIntercept: got %d, want %d", snap.CostIntercept, GasPriceOracleCostIntercept)
	}
	if snap.CostFastlzCoef != GasPriceOracleCostFastlzCoef {
		t.Errorf("CostFastlzCoef: got %d, want %d", snap.CostFastlzCoef, GasPriceOracleCostFastlzCoef)
	}
	if snap.MinTransactionSize == nil || snap.MinTransactionSize.Cmp(big.NewInt(int64(GasPriceOracleMinTransactionSize))) != 0 {
		t.Errorf("MinTransactionSize: got %v, want %d", snap.MinTransactionSize, GasPriceOracleMinTransactionSize)
	}
	if snap.Version != GasPriceOracleConstantsSourceVersion {
		t.Errorf("Version: got %q, want %q", snap.Version, GasPriceOracleConstantsSourceVersion)
	}
	if !snap.VersionMatches {
		t.Errorf("VersionMatches: got false, want true")
	}
}

// TestFetchGasPriceOracleSnapshot_VersionDrift exercises the drift
// case: the deployed contract reports a different semver, constants
// are still returned, VersionMatches flips false so the renderer can
// warn the operator.
func TestFetchGasPriceOracleSnapshot_VersionDrift(t *testing.T) {
	f := &gpoFakeRPC{bySelector: map[string]string{
		versionSelector: versionResultHex("2.0.0"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snap, err := FetchGasPriceOracleSnapshot(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchGasPriceOracleSnapshot: %v", err)
	}
	if snap.Version != "2.0.0" {
		t.Errorf("Version: got %q, want %q", snap.Version, "2.0.0")
	}
	if snap.VersionMatches {
		t.Errorf("VersionMatches: got true, want false on drift")
	}
	// Constants still populated.
	if snap.CostIntercept != GasPriceOracleCostIntercept {
		t.Errorf("CostIntercept should remain pinned: got %d", snap.CostIntercept)
	}
}

// TestFetchGasPriceOracleSnapshot_VersionRevert checks the graceful
// path when the deployed contract has no version() getter (or RPC
// reverts for any other reason). Constants must still surface and
// VersionMatches must be false so the UI shows "drift undetectable".
func TestFetchGasPriceOracleSnapshot_VersionRevert(t *testing.T) {
	f := &gpoFakeRPC{bySelector: map[string]string{
		versionSelector: "", // forces revert
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snap, err := FetchGasPriceOracleSnapshot(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchGasPriceOracleSnapshot: %v (snapshot=%+v)", err, snap)
	}
	if _, ok := snap.Errors["version"]; !ok {
		t.Errorf("expected version error, got %v", snap.Errors)
	}
	if snap.VersionMatches {
		t.Errorf("VersionMatches: got true, want false on revert")
	}
	if snap.CostIntercept != GasPriceOracleCostIntercept {
		t.Errorf("CostIntercept should remain pinned: got %d", snap.CostIntercept)
	}
}

// TestFetchGasPriceOracleSnapshot_EmptyL2URL ensures empty config
// surfaces a clear error and still returns the hardcoded constants.
func TestFetchGasPriceOracleSnapshot_EmptyL2URL(t *testing.T) {
	snap, err := FetchGasPriceOracleSnapshot(context.Background(), nil, "")
	if err == nil {
		t.Fatal("expected error on empty l2_rpc_url")
	}
	if _, ok := snap.Errors["version"]; !ok {
		t.Errorf("expected version error, got %v", snap.Errors)
	}
	if snap.CostIntercept != GasPriceOracleCostIntercept {
		t.Errorf("CostIntercept should remain pinned even with empty URL: got %d", snap.CostIntercept)
	}
}
