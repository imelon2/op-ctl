package l2

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// blockExtraDataFakeRPC answers eth_getBlockByNumber with a fixed
// number + extraData; any other method errors.
func blockFakeServer(t *testing.T, number, extraData string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rpcReq
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := rpcResp{JSONRPC: "2.0", ID: req.ID}
		if req.Method != "eth_getBlockByNumber" {
			out.Error = &rpcErr{Code: -32601, Message: "unknown method"}
		} else {
			raw, _ := json.Marshal(map[string]string{"number": number, "extraData": extraData})
			out.Result = raw
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// TestFetchLatestBlockEIP1559_Jovian decodes the exact example from the
// spec: 0x01000000fa000000060000000000000064 →
// version 1, denominator 250, elasticity 6, minBaseFee 100.
func TestFetchLatestBlockEIP1559_Jovian(t *testing.T) {
	srv := blockFakeServer(t, "0x1a4", "0x01000000fa000000060000000000000064")
	defer srv.Close()

	s, err := FetchLatestBlockEIP1559(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchLatestBlockEIP1559: %v", err)
	}
	if s.Err != nil {
		t.Fatalf("s.Err: %v", s.Err)
	}
	if s.BlockNumber == nil || s.BlockNumber.Int64() != 0x1a4 {
		t.Errorf("BlockNumber: got %v, want 420", s.BlockNumber)
	}
	if s.Version != 1 {
		t.Errorf("Version: got %d, want 1", s.Version)
	}
	if s.Denominator != 250 {
		t.Errorf("Denominator: got %d, want 250", s.Denominator)
	}
	if s.Elasticity != 6 {
		t.Errorf("Elasticity: got %d, want 6", s.Elasticity)
	}
	if s.MinBaseFee != 100 {
		t.Errorf("MinBaseFee: got %d, want 100", s.MinBaseFee)
	}
}

// TestDecodeHolocene decodes a 9-byte version-0 extraData: no
// minBaseFee, fork name "Holocene".
func TestDecodeHolocene(t *testing.T) {
	// version 0, denominator 250, elasticity 6
	d := []byte{0x00, 0x00, 0x00, 0x00, 0xfa, 0x00, 0x00, 0x00, 0x06}
	s := &BlockEIP1559Snapshot{ExtraData: d}
	if err := s.decode(); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Version != 0 || s.ForkName() != "Holocene" {
		t.Errorf("fork: got version=%d name=%q, want 0/Holocene", s.Version, s.ForkName())
	}
	if s.Denominator != 250 || s.Elasticity != 6 {
		t.Errorf("params: got denom=%d elas=%d, want 250/6", s.Denominator, s.Elasticity)
	}
	if s.HasMinBaseFee {
		t.Error("HasMinBaseFee should be false for Holocene")
	}
}

// TestDecodeJovian_ForkName confirms the version-1 path labels the fork
// "Jovian" and surfaces minBaseFee.
func TestDecodeJovian_ForkName(t *testing.T) {
	d := []byte{0x01, 0x00, 0x00, 0x00, 0xfa, 0x00, 0x00, 0x00, 0x06,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64}
	s := &BlockEIP1559Snapshot{ExtraData: d}
	if err := s.decode(); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ForkName() != "Jovian" {
		t.Errorf("ForkName: got %q, want Jovian", s.ForkName())
	}
	if !s.HasMinBaseFee || s.MinBaseFee != 100 {
		t.Errorf("minBaseFee: got has=%v val=%d, want true/100", s.HasMinBaseFee, s.MinBaseFee)
	}
}

// TestDecode_WrongLength rejects a version byte whose length doesn't
// match that version's mandated layout.
func TestDecode_WrongLength(t *testing.T) {
	// version 1 (Jovian) but only 3 bytes.
	s := &BlockEIP1559Snapshot{ExtraData: []byte{0x01, 0x00, 0x00}}
	if err := s.decode(); err == nil {
		t.Fatal("expected length error, got nil")
	}
	if s.Denominator != 0 {
		t.Errorf("Denominator should stay 0 on failure, got %d", s.Denominator)
	}
}

// TestDecode_UnknownVersion rejects a version byte that's neither
// Holocene (0) nor Jovian (1).
func TestDecode_UnknownVersion(t *testing.T) {
	d := make([]byte, JovianExtraDataLen)
	d[0] = 0x02
	s := &BlockEIP1559Snapshot{ExtraData: d}
	if err := s.decode(); err == nil {
		t.Fatal("expected version error, got nil")
	}
}

// TestFetchLatestBlockEIP1559_EmptyURL surfaces a clear error and
// returns a non-nil snapshot.
func TestFetchLatestBlockEIP1559_EmptyURL(t *testing.T) {
	s, err := FetchLatestBlockEIP1559(context.Background(), nil, "")
	if err == nil {
		t.Fatal("expected error on empty l2_rpc_url")
	}
	if s == nil || s.Err == nil {
		t.Fatalf("expected snapshot with Err set, got %+v", s)
	}
}
