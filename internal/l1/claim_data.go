package l1

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// ClaimData mirrors one element of FaultDisputeGame.claimData[i],
// fetched via the auto-generated getter:
//
//	function claimData(uint256 _index)
//	  external view
//	  returns (uint32 parentIndex,
//	           address counteredBy,
//	           address claimant,
//	           uint128 bond,
//	           Claim claim,        // bytes32
//	           Position position,  // uint128
//	           Clock clock)        // uint128
//
// Bond, Position and Clock are uint128 on-chain so we model them as
// *big.Int — they comfortably exceed uint64 in pathological cases and
// the value is presentational, not arithmetic.
type ClaimData struct {
	Index       uint64
	ParentIndex uint32
	CounteredBy string // 0x... (zero address = "uncountered yet")
	Claimant    string
	Bond        *big.Int
	Claim       string // 0x...bytes32
	Position    *big.Int
	Clock       *big.Int

	// ChallengerDuration is the seconds remaining on the chess clock
	// for the *opposing* side of this claim — sourced from
	// FaultDisputeGame.getChallengerDuration(uint256) which performs
	// the clock arithmetic against block.timestamp. Zero means the
	// clock has already expired. Duration is uint64 on-chain.
	ChallengerDuration uint64
}

// claimDataNoParent is Solidity's "no parent" sentinel — the root
// claim is initialized with parentIndex == type(uint32).max so the
// game's tree walking knows where the chain terminates.
const claimDataNoParent uint32 = 0xFFFFFFFF

// HasParent returns true when this claim has a real parent index
// (i.e. it's not the root claim).
func (c ClaimData) HasParent() bool { return c.ParentIndex != claimDataNoParent }

// IsCountered returns true when CounteredBy is set to a non-zero
// address. Solidity zero-initializes the slot, so the zero address
// stands in for "uncountered" without needing a separate flag.
func (c ClaimData) IsCountered() bool {
	return c.CounteredBy != "" && c.CounteredBy != "0x0000000000000000000000000000000000000000"
}

// FetchClaimData issues a single batched JSON-RPC POST containing N
// claimData(i) calls and decodes each return into a ClaimData. The
// per-call errs slot carries the failure for any individual call so
// a single revert (e.g. index racing the contract's growth) does
// not abort the whole fetch. The hard err return is reserved for
// transport-level failures (HTTP 5xx, malformed envelope).
//
// n is typically GameSnapshot.ClaimDataLen.Uint64() — callers should
// short-circuit when n == 0 instead of hitting this with an empty
// indices slice.
// FetchClaimData issues a single batched JSON-RPC POST that, per index,
// makes BOTH claimData(i) and getChallengerDuration(i) calls and
// decodes the pair into one ClaimData entry. Total batch size = 2N.
//
// Errors-handling layout: errs[i] reports the claimData(i) decode error
// (the primary read). A failed getChallengerDuration(i) leaves
// ChallengerDuration at zero but does NOT populate errs[i] — the row
// still renders the rest of the data with a "—" duration. This keeps
// the display tolerant of older game contracts that pre-date the
// helper.
func FetchClaimData(ctx context.Context, hc *http.Client, l1RPCURL, gameAddr string, n uint64) ([]ClaimData, []error, time.Duration, error) {
	if n == 0 {
		return nil, nil, 0, nil
	}
	calls := make([]EthCallReq, 0, 2*n)
	cdSel := selectorOf("claimData(uint256)")
	durSel := selectorOf("getChallengerDuration(uint256)")
	for i := uint64(0); i < n; i++ {
		idxHex := encodeUint64(i)
		calls = append(calls,
			EthCallReq{To: gameAddr, Data: cdSel + idxHex},
			EthCallReq{To: gameAddr, Data: durSel + idxHex},
		)
	}
	results, latency, err := EthCallBatch(ctx, hc, l1RPCURL, calls)
	if err != nil {
		return nil, nil, latency, err
	}
	out := make([]ClaimData, n)
	errs := make([]error, n)
	for i := uint64(0); i < n; i++ {
		// Each claim's two results sit at offsets 2i and 2i+1, in
		// the order pushed above.
		cdRes := results[2*i]
		durRes := results[2*i+1]
		out[i].Index = i
		if cdRes.Err != nil {
			errs[i] = cdRes.Err
		} else {
			cd, derr := decodeClaimData(cdRes.Result, i)
			out[i] = cd
			errs[i] = derr
		}
		if durRes.Err == nil {
			buf, derr := decodeHexData(durRes.Result)
			if derr == nil && len(buf) >= 32 {
				w, _ := wordAt(buf, 0)
				out[i].ChallengerDuration = wordToUint64(w)
			}
		}
	}
	return out, errs, latency, nil
}

// decodeClaimData parses the 7-tuple return of claimData(uint256)
// into a ClaimData. All seven returns are static-size so the layout
// is 7 contiguous 32-byte words (224 bytes total).
func decodeClaimData(hexResult string, index uint64) (ClaimData, error) {
	cd := ClaimData{Index: index}
	buf, err := decodeHexData(hexResult)
	if err != nil {
		return cd, fmt.Errorf("claimData[%d]: %w", index, err)
	}
	if len(buf) < 7*32 {
		return cd, fmt.Errorf("claimData[%d]: result too short (%d, want 224)", index, len(buf))
	}
	w0, _ := wordAt(buf, 0)
	w1, _ := wordAt(buf, 1)
	w2, _ := wordAt(buf, 2)
	w3, _ := wordAt(buf, 3)
	w4, _ := wordAt(buf, 4)
	w5, _ := wordAt(buf, 5)
	w6, _ := wordAt(buf, 6)
	cd.ParentIndex = wordToUint32(w0)
	cd.CounteredBy = wordToAddress(w1)
	cd.Claimant = wordToAddress(w2)
	cd.Bond = wordToUint256(w3)
	cd.Claim = wordToBytes32(w4)
	cd.Position = wordToUint256(w5)
	cd.Clock = wordToUint256(w6)
	return cd, nil
}
