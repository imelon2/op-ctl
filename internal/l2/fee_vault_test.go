package l2

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

// wordHex returns n encoded as a 32-byte big-endian word (no 0x).
// Tiny helper so test fixtures stay readable.
func wordHex(n uint64) string {
	out := make([]byte, 32)
	for i := range 8 {
		out[31-i] = byte(n >> (8 * i))
	}
	const hex = "0123456789abcdef"
	buf := make([]byte, 64)
	for i, b := range out {
		buf[2*i] = hex[b>>4]
		buf[2*i+1] = hex[b&0x0f]
	}
	return string(buf)
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

// vaultFakeRPC dispatches by method + first param:
//
//	eth_getBalance → balanceByAddr[addr]
//	eth_call       → callByAddr[addr]  (totalProcessed)
//
// Each map entry is the 0x-prefixed hex string the node should
// return; empty string forces an RPC revert.
type vaultFakeRPC struct {
	balanceByAddr map[string]string
	callByAddr    map[string]string
}

func (f *vaultFakeRPC) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	// Try batch first.
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

func (f *vaultFakeRPC) respondOne(req rpcReq) rpcResp {
	out := rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "eth_getBalance":
		addr, _ := req.Params[0].(string)
		val, ok := f.balanceByAddr[strings.ToLower(addr)]
		if !ok || val == "" {
			out.Error = &rpcErr{Code: -32000, Message: "revert"}
			return out
		}
		raw, _ := json.Marshal(val)
		out.Result = raw
	case "eth_call":
		params, _ := req.Params[0].(map[string]any)
		to, _ := params["to"].(string)
		val, ok := f.callByAddr[strings.ToLower(to)]
		if !ok || val == "" {
			out.Error = &rpcErr{Code: -32000, Message: "revert"}
			return out
		}
		raw, _ := json.Marshal(val)
		out.Result = raw
	default:
		out.Error = &rpcErr{Code: -32601, Message: "unknown method"}
	}
	return out
}

func TestFetchAllVaultSnapshots_AllFieldsPopulated(t *testing.T) {
	balanceWei := func(n uint64) string { return "0x" + wordHex(n) }
	totalProcessed := func(n uint64) string { return "0x" + wordHex(n) }
	f := &vaultFakeRPC{
		balanceByAddr: map[string]string{
			BaseFeeVaultAddr:      balanceWei(100),
			L1FeeVaultAddr:        balanceWei(200),
			SequencerFeeVaultAddr: balanceWei(300),
			OperatorFeeVaultAddr:  balanceWei(0),
		},
		callByAddr: map[string]string{
			BaseFeeVaultAddr:      totalProcessed(1000),
			L1FeeVaultAddr:        totalProcessed(2000),
			SequencerFeeVaultAddr: totalProcessed(3000),
			OperatorFeeVaultAddr:  totalProcessed(0),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snaps, _, err := FetchAllVaultSnapshots(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchAllVaultSnapshots: %v", err)
	}
	if len(snaps) != 4 {
		t.Fatalf("len(snaps): got %d, want 4", len(snaps))
	}
	want := []struct {
		name    string
		bal, tp uint64
	}{
		{NameBaseFeeVault, 100, 1000},
		{NameL1FeeVault, 200, 2000},
		{NameSequencerFeeVault, 300, 3000},
		{NameOperatorFeeVault, 0, 0},
	}
	for i, w := range want {
		s := snaps[i]
		if s.Name != w.name {
			t.Errorf("snap[%d].Name: got %q, want %q", i, s.Name, w.name)
		}
		if len(s.Errors) != 0 {
			t.Errorf("snap[%d].Errors: expected empty, got %v", i, s.Errors)
		}
		if s.Balance == nil || s.Balance.Cmp(big.NewInt(int64(w.bal))) != 0 {
			t.Errorf("snap[%d].Balance: got %v, want %d", i, s.Balance, w.bal)
		}
		if s.TotalProcessed == nil || s.TotalProcessed.Cmp(big.NewInt(int64(w.tp))) != 0 {
			t.Errorf("snap[%d].TotalProcessed: got %v, want %d", i, s.TotalProcessed, w.tp)
		}
	}
}

func TestFetchAllVaultSnapshots_PartialRevertRecorded(t *testing.T) {
	// OperatorFeeVault has a balance but totalProcessed reverts —
	// a realistic pre-Isthmus scenario. Other vaults still populate.
	f := &vaultFakeRPC{
		balanceByAddr: map[string]string{
			BaseFeeVaultAddr:      "0x" + wordHex(50),
			L1FeeVaultAddr:        "0x" + wordHex(60),
			SequencerFeeVaultAddr: "0x" + wordHex(70),
			OperatorFeeVaultAddr:  "0x" + wordHex(0),
		},
		callByAddr: map[string]string{
			BaseFeeVaultAddr:      "0x" + wordHex(500),
			L1FeeVaultAddr:        "0x" + wordHex(600),
			SequencerFeeVaultAddr: "0x" + wordHex(700),
			OperatorFeeVaultAddr:  "", // revert
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	snaps, _, err := FetchAllVaultSnapshots(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchAllVaultSnapshots: %v", err)
	}
	op := snaps[3]
	if op.Name != NameOperatorFeeVault {
		t.Fatalf("snap[3].Name: got %q, want %q", op.Name, NameOperatorFeeVault)
	}
	if op.Balance == nil || op.Balance.Sign() != 0 {
		t.Errorf("OperatorFeeVault.Balance: got %v, want 0", op.Balance)
	}
	if _, ok := op.Errors["totalProcessed"]; !ok {
		t.Errorf("OperatorFeeVault should record totalProcessed revert, got %v", op.Errors)
	}
	if snaps[0].Balance.Cmp(big.NewInt(50)) != 0 || snaps[0].TotalProcessed.Cmp(big.NewInt(500)) != 0 {
		t.Errorf("BaseFeeVault should be fully populated: %+v", snaps[0])
	}
}

func TestFetchAllVaultSnapshots_EmptyL2URL(t *testing.T) {
	snaps, _, err := FetchAllVaultSnapshots(context.Background(), nil, "")
	if err == nil {
		t.Fatal("expected error on empty l2_rpc_url")
	}
	if len(snaps) != 4 {
		t.Fatalf("len(snaps): got %d, want 4", len(snaps))
	}
	for i, s := range snaps {
		if _, ok := s.Errors["balance"]; !ok {
			t.Errorf("snap[%d] should record balance error, got %v", i, s.Errors)
		}
	}
}

func TestAllVaults_OrderAndAddresses(t *testing.T) {
	v := AllVaults()
	want := []struct{ name, addr string }{
		{NameBaseFeeVault, BaseFeeVaultAddr},
		{NameL1FeeVault, L1FeeVaultAddr},
		{NameSequencerFeeVault, SequencerFeeVaultAddr},
		{NameOperatorFeeVault, OperatorFeeVaultAddr},
	}
	if len(v) != len(want) {
		t.Fatalf("len(AllVaults()): got %d, want %d", len(v), len(want))
	}
	for i, w := range want {
		if v[i].Name != w.name || v[i].Address != w.addr {
			t.Errorf("AllVaults()[%d]: got {%s, %s}, want {%s, %s}", i, v[i].Name, v[i].Address, w.name, w.addr)
		}
	}
}
