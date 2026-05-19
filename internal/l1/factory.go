package l1

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// GameListing is one row of `op-ctl read dispute-game`'s paginated
// list, sourced from DisputeGameFactory.gameAtIndex(uint256). Solidity
// declares the return as (GameType, Timestamp, IDisputeGame) which
// over the wire is (uint32, uint64, address) right-padded into three
// 32-byte words.
type GameListing struct {
	Index     uint64
	GameType  uint32
	Timestamp uint64 // unix seconds (Solidity Timestamp is uint64)
	Proxy     string // 0x-prefixed lowercase hex
}

// Version calls DisputeGameFactoryProxy.version() and returns the
// decoded string. The factory's version is a public constant ("1.4.0"
// at time of writing); we still fetch it dynamically so an upgrade
// won't stale-data the UI.
//
// Solidity: function version() public pure returns (string memory)
// Selector: keccak256("version()")[0:4] = 0x54fd4d50
func Version(ctx context.Context, hc *http.Client, l1RPCURL, factoryAddr string) (string, time.Duration, error) {
	raw, latency, err := EthCall(ctx, hc, l1RPCURL, factoryAddr, selectorOf("version()"))
	if err != nil {
		return "", latency, err
	}
	buf, err := decodeHexData(raw)
	if err != nil {
		return "", latency, fmt.Errorf("version: %w", err)
	}
	if len(buf) < 32 {
		return "", latency, fmt.Errorf("version: result too short (%d bytes)", len(buf))
	}
	// String return layout: head[0] = offset to data → tail with len + bytes.
	offW, err := wordAt(buf, 0)
	if err != nil {
		return "", latency, fmt.Errorf("version: %w", err)
	}
	off, err := readDynamicOffset(offW)
	if err != nil {
		return "", latency, fmt.Errorf("version: %w", err)
	}
	s, err := decodeStringAt(buf, off)
	if err != nil {
		return "", latency, fmt.Errorf("version: %w", err)
	}
	return s, latency, nil
}

// GameAtIndex calls DisputeGameFactoryProxy.gameAtIndex(_index) and
// decodes the (GameType, Timestamp, IDisputeGame) triple.
//
// Solidity:
//
//	function gameAtIndex(uint256 _index)
//	    external view
//	    returns (GameType gameType_, Timestamp timestamp_, IDisputeGame proxy_)
//
// Selector: keccak256("gameAtIndex(uint256)")[0:4] = 0xbb8aa1fc
func GameAtIndex(ctx context.Context, hc *http.Client, l1RPCURL, factoryAddr string, index uint64) (GameListing, time.Duration, error) {
	data := selectorOf("gameAtIndex(uint256)") + encodeUint64(index)
	raw, latency, err := EthCall(ctx, hc, l1RPCURL, factoryAddr, data)
	if err != nil {
		return GameListing{Index: index}, latency, err
	}
	gl, err := decodeGameAtIndex(raw, index)
	return gl, latency, err
}

// GameAtIndexBatch fans the per-index calls into a single JSON-RPC
// batch — used by the list screen when paginating. Per-call failures
// land in the parallel errs slot so the surviving rows still render.
// A transport-level batch failure (HTTP non-2xx, malformed envelope,
// etc.) is returned via the second return value (a single hard err)
// and listings/errs are nil.
func GameAtIndexBatch(ctx context.Context, hc *http.Client, l1RPCURL, factoryAddr string, indices []uint64) ([]GameListing, []error, time.Duration, error) {
	if len(indices) == 0 {
		return nil, nil, 0, nil
	}
	calls := make([]EthCallReq, len(indices))
	selector := selectorOf("gameAtIndex(uint256)")
	for i, idx := range indices {
		calls[i] = EthCallReq{To: factoryAddr, Data: selector + encodeUint64(idx)}
	}
	results, latency, err := EthCallBatch(ctx, hc, l1RPCURL, calls)
	if err != nil {
		return nil, nil, latency, err
	}
	listings := make([]GameListing, len(indices))
	errs := make([]error, len(indices))
	for i, r := range results {
		listings[i].Index = indices[i]
		if r.Err != nil {
			errs[i] = r.Err
			continue
		}
		gl, derr := decodeGameAtIndex(r.Result, indices[i])
		listings[i] = gl
		errs[i] = derr
	}
	return listings, errs, latency, nil
}

// VersionSelectorData is the calldata for DisputeGameFactoryProxy
// (or FaultDisputeGame).version(). Exposed so the TUI can batch
// version() alongside other reads without re-deriving the selector.
func VersionSelectorData() string { return selectorOf("version()") }

// GameCountSelectorData is the calldata for
// DisputeGameFactoryProxy.gameCount(). Exposed for the same reason
// as VersionSelectorData.
func GameCountSelectorData() string { return selectorOf("gameCount()") }

// DecodeVersionResult parses an eth_call result for a Solidity
// `string version()` view into a Go string. Returns an error on a
// malformed payload.
func DecodeVersionResult(hexResult string) (string, error) {
	buf, err := decodeHexData(hexResult)
	if err != nil {
		return "", err
	}
	if len(buf) < 32 {
		return "", fmt.Errorf("version: result too short (%d)", len(buf))
	}
	w0, _ := wordAt(buf, 0)
	off, err := readDynamicOffset(w0)
	if err != nil {
		return "", err
	}
	return decodeStringAt(buf, off)
}

// DecodeUint256Result parses an eth_call result for any uint256 view
// (e.g. gameCount()) into a *big.Int.
func DecodeUint256Result(hexResult string) (*big.Int, error) {
	buf, err := decodeHexData(hexResult)
	if err != nil {
		return nil, err
	}
	if len(buf) < 32 {
		return nil, fmt.Errorf("uint256: result too short (%d)", len(buf))
	}
	w0, _ := wordAt(buf, 0)
	return wordToUint256(w0), nil
}

// decodeGameAtIndex parses the (uint32, uint64, address) return into
// a GameListing. Solidity right-pads each value in its own 32-byte
// word — total return is always 3*32 = 96 bytes.
func decodeGameAtIndex(hexStr string, index uint64) (GameListing, error) {
	gl := GameListing{Index: index}
	buf, err := decodeHexData(hexStr)
	if err != nil {
		return gl, fmt.Errorf("gameAtIndex: %w", err)
	}
	if len(buf) < 96 {
		return gl, fmt.Errorf("gameAtIndex: result too short (%d bytes, want 96)", len(buf))
	}
	w0, _ := wordAt(buf, 0)
	w1, _ := wordAt(buf, 1)
	w2, _ := wordAt(buf, 2)
	gl.GameType = wordToUint32(w0)
	gl.Timestamp = wordToUint64(w1)
	gl.Proxy = wordToAddress(w2)
	return gl, nil
}
