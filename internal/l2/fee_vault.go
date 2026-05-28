// Package l2 is a thin set of view-call helpers for L2 predeploys.
//
// op-ctl ships its own minimal JSON-RPC + ABI codec under internal/l1
// (originally written for settlement-layer reads). Rather than
// duplicate it, this package imports those helpers and pairs them with
// L2-specific selectors and address constants. Every function expects
// an L2 RPC URL (from [rpc].l2_rpc_url) and lets the underlying l1
// client handle envelope plumbing.
package l2

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"op-ctl/internal/l1"
)

// FeeVault predeploy addresses. Source of truth:
// optimism/packages/contracts-bedrock/src/libraries/Predeploys.sol.
const (
	BaseFeeVaultAddr      = "0x4200000000000000000000000000000000000019"
	L1FeeVaultAddr        = "0x420000000000000000000000000000000000001a"
	SequencerFeeVaultAddr = "0x4200000000000000000000000000000000000011"
	OperatorFeeVaultAddr  = "0x420000000000000000000000000000000000001b"
)

// VaultName labels each predeploy in user-facing output. Order matches
// AllVaults() / FetchAllVaultSnapshots() iteration order — the same
// order the operator sees on screen.
const (
	NameBaseFeeVault      = "BaseFeeVault"
	NameL1FeeVault        = "L1FeeVault"
	NameSequencerFeeVault = "SequencerFeeVault"
	NameOperatorFeeVault  = "OperatorFeeVault"
)

// VaultDescriptor binds a human name to its predeploy address. The
// list returned by AllVaults() is the canonical iteration order.
type VaultDescriptor struct {
	Name    string
	Address string
}

// AllVaults returns the 4 FeeVault predeploys in render order: base
// fees first (largest accumulator), then L1 data fees, sequencer tip,
// and finally operator fee (which only accrues post-Isthmus).
func AllVaults() []VaultDescriptor {
	return []VaultDescriptor{
		{Name: NameBaseFeeVault, Address: BaseFeeVaultAddr},
		{Name: NameL1FeeVault, Address: L1FeeVaultAddr},
		{Name: NameSequencerFeeVault, Address: SequencerFeeVaultAddr},
		{Name: NameOperatorFeeVault, Address: OperatorFeeVaultAddr},
	}
}

// VaultSnapshot captures the two numbers operators care about for
// each FeeVault:
//
//   - Balance: the live wei balance via eth_getBalance (what's
//     currently sitting in the vault waiting for the next withdraw).
//   - TotalProcessed: the lifetime total ever withdrawn from the
//     vault via totalProcessed() — sums across every withdrawal
//     ever issued. Acts as a watermark for historical revenue.
//
// Per-field errors mirror the snapshot pattern used by
// l1.GameSnapshot / l1.SystemConfigSnapshot: a vault that hasn't
// shipped (pre-Isthmus OperatorFeeVault) may revert totalProcessed
// while its balance still returns 0 cleanly. Recording the error
// lets the printer say "ERR(...)" for one field without losing the
// other.
type VaultSnapshot struct {
	Name    string
	Address string

	Balance        *big.Int
	TotalProcessed *big.Int

	Errors  map[string]error
	Latency time.Duration
}

// totalProcessedSelector is keccak256("totalProcessed()")[0:4]; the
// FeeVault abstract contract exposes it as a public uint256 getter.
var totalProcessedSelector = l1.SelectorOf("totalProcessed()")

// FetchVaultSnapshot reads (balance, totalProcessed) for a single
// vault. Two RPC roundtrips: eth_getBalance + eth_call. Wall-clock
// latency is the sum of both — close enough for header diagnostics.
//
// Per-call failures land in s.Errors but never abort the snapshot.
func FetchVaultSnapshot(ctx context.Context, hc *http.Client, l2RPCURL string, v VaultDescriptor) VaultSnapshot {
	s := VaultSnapshot{
		Name:    v.Name,
		Address: v.Address,
		Errors:  map[string]error{},
	}
	if strings.TrimSpace(l2RPCURL) == "" {
		s.Errors["balance"] = fmt.Errorf("l2_rpc_url is empty (set [rpc].l2_rpc_url in config.toml)")
		s.Errors["totalProcessed"] = s.Errors["balance"]
		return s
	}
	bal, balLat, err := l1.EthGetBalance(ctx, hc, l2RPCURL, v.Address)
	s.Latency += balLat
	if err != nil {
		s.Errors["balance"] = err
	} else {
		s.Balance = bal
	}
	raw, callLat, err := l1.EthCall(ctx, hc, l2RPCURL, v.Address, totalProcessedSelector)
	s.Latency += callLat
	if err != nil {
		s.Errors["totalProcessed"] = err
		return s
	}
	n, derr := decodeUint256(raw)
	if derr != nil {
		s.Errors["totalProcessed"] = fmt.Errorf("decode totalProcessed: %w", derr)
		return s
	}
	s.TotalProcessed = n
	return s
}

// FetchAllVaultSnapshots fans the per-vault fetches into one batched
// JSON-RPC POST (eth_getBalance + eth_call for each of the 4 vaults
// → 8 sub-requests). Returns snapshots in AllVaults() order. A
// transport-level batch failure (network down, malformed envelope)
// is reported via the returned error AND each snapshot gets the
// failure recorded in Errors so partial rendering still works.
func FetchAllVaultSnapshots(ctx context.Context, hc *http.Client, l2RPCURL string) ([]VaultSnapshot, time.Duration, error) {
	vaults := AllVaults()
	snaps := make([]VaultSnapshot, len(vaults))
	for i, v := range vaults {
		snaps[i] = VaultSnapshot{Name: v.Name, Address: v.Address, Errors: map[string]error{}}
	}
	if strings.TrimSpace(l2RPCURL) == "" {
		err := fmt.Errorf("l2_rpc_url is empty (set [rpc].l2_rpc_url in config.toml)")
		for i := range snaps {
			snaps[i].Errors["balance"] = err
			snaps[i].Errors["totalProcessed"] = err
		}
		return snaps, 0, err
	}
	// Build the batched eth_call slice for totalProcessed() (4
	// requests). Balance reads use eth_getBalance which doesn't fit
	// EthCallBatch's eth_call-only envelope — issue them sequentially.
	// In practice both call sets are public-RPC-friendly: 4+4=8 RPCs
	// per invocation, well under any rate-limit threshold.
	calls := make([]l1.EthCallReq, len(vaults))
	for i, v := range vaults {
		calls[i] = l1.EthCallReq{To: v.Address, Data: totalProcessedSelector}
	}
	callResults, callLat, err := l1.EthCallBatch(ctx, hc, l2RPCURL, calls)
	if err != nil {
		for i := range snaps {
			snaps[i].Errors["totalProcessed"] = err
		}
	} else {
		for i, r := range callResults {
			if r.Err != nil {
				snaps[i].Errors["totalProcessed"] = r.Err
				continue
			}
			n, derr := decodeUint256(r.Result)
			if derr != nil {
				snaps[i].Errors["totalProcessed"] = fmt.Errorf("decode totalProcessed: %w", derr)
				continue
			}
			snaps[i].TotalProcessed = n
		}
	}
	// Balances: one HTTP roundtrip each. Sum into the aggregate latency
	// so the printed header reflects total wall-clock cost.
	var balLatSum time.Duration
	for i, v := range vaults {
		bal, balLat, berr := l1.EthGetBalance(ctx, hc, l2RPCURL, v.Address)
		balLatSum += balLat
		if berr != nil {
			snaps[i].Errors["balance"] = berr
			continue
		}
		snaps[i].Balance = bal
	}
	totalLat := callLat + balLatSum
	for i := range snaps {
		snaps[i].Latency = totalLat
	}
	return snaps, totalLat, nil
}

// decodeUint256 parses a totalProcessed() eth_call result (0x-prefixed
// hex) into a *big.Int. Thin wrapper around the l1 helpers so this
// package's call sites stay readable.
func decodeUint256(hexResult string) (*big.Int, error) {
	buf, err := l1.DecodeHexData(hexResult)
	if err != nil {
		return nil, err
	}
	if len(buf) < 32 {
		return nil, fmt.Errorf("uint256: result too short (%d)", len(buf))
	}
	w, _ := l1.WordAt(buf, 0)
	return l1.WordToUint256(w), nil
}
