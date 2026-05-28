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

// GasPriceOracleAddr is the L2 predeploy address of GasPriceOracle.
// Source of truth: optimism/packages/contracts-bedrock/src/libraries/Predeploys.sol.
const GasPriceOracleAddr = "0x420000000000000000000000000000000000000f"

// FastLZ→Brotli compressed-size regression constants. In the
// GasPriceOracle source these are `private constant`, which Solidity
// inlines into bytecode as PUSH operands at each use site — there is
// no public getter and `eth_call`/`eth_getStorageAt` cannot read
// them. We pin the literal values here from the source-of-truth
// version and surface a drift warning via the contract's `version()`
// getter (which IS public and therefore readable).
//
// Update GasPriceOracleConstantsSourceVersion AND the three values
// together when the contract is rev'd.
const GasPriceOracleConstantsSourceVersion = "1.6.0"

const (
	GasPriceOracleCostIntercept      int32  = -42_585_600
	GasPriceOracleCostFastlzCoef     uint32 = 836_500
	GasPriceOracleMinTransactionSize uint64 = 100
)

// GasPriceOracleSnapshot reports the three compressed-size regression
// constants alongside the deployed contract's `version()`. The
// constants are compile-time literals (see source-version comment
// above); only Version is fetched at runtime. VersionMatches tells
// the caller whether the pinned constants line up with what's
// actually deployed — if false, the renderer should warn the
// operator that the displayed constants may not match bytecode.
type GasPriceOracleSnapshot struct {
	Address string

	// Live Ecotone+ fee inputs, all public getters readable via
	// eth_call. L1BaseFee / BlobBaseFee mirror the L1 attributes the
	// derivation pipeline writes each L2 block; the two scalars are the
	// governance-set multipliers SystemConfig pushes down. Per-field
	// errors land in Errors keyed by the getter name.
	BaseFee           *big.Int
	L1BaseFee         *big.Int
	BlobBaseFee       *big.Int
	BaseFeeScalar     uint32
	BlobBaseFeeScalar uint32
	Decimals          *big.Int

	CostIntercept      int32
	CostFastlzCoef     uint32
	MinTransactionSize *big.Int

	// ConstantsSourceVersion is the GasPriceOracle source version the
	// hardcoded constants were extracted from.
	ConstantsSourceVersion string

	// Version is the deployed contract's version() return — empty
	// when Errors["version"] is populated (call reverted or RPC
	// failed).
	Version string

	// VersionMatches is true when Version == ConstantsSourceVersion.
	// False when versions differ OR when the version() call failed.
	VersionMatches bool

	Errors  map[string]error
	Latency time.Duration
}

var versionSelector = l1.SelectorOf("version()")

// Selectors for the four live fee getters. Each is a public view
// method on GasPriceOracle (Ecotone onward) returning a single word.
var (
	baseFeeSelector           = l1.SelectorOf("baseFee()")
	l1BaseFeeSelector         = l1.SelectorOf("l1BaseFee()")
	blobBaseFeeSelector       = l1.SelectorOf("blobBaseFee()")
	baseFeeScalarSelector     = l1.SelectorOf("baseFeeScalar()")
	blobBaseFeeScalarSelector = l1.SelectorOf("blobBaseFeeScalar()")
	decimalsSelector          = l1.SelectorOf("decimals()")
)

// FetchGasPriceOracleSnapshot returns a snapshot with the hardcoded
// constants populated unconditionally, then RPC-fetches `version()`
// to detect drift between the pinned constants and the deployed
// contract. Errors["version"] is populated when the call fails;
// constants remain valid regardless and callers should still render
// them with a "version unknown — drift undetectable" caveat.
func FetchGasPriceOracleSnapshot(ctx context.Context, hc *http.Client, l2RPCURL string) (*GasPriceOracleSnapshot, error) {
	s := &GasPriceOracleSnapshot{
		Address:                GasPriceOracleAddr,
		CostIntercept:          GasPriceOracleCostIntercept,
		CostFastlzCoef:         GasPriceOracleCostFastlzCoef,
		MinTransactionSize:     new(big.Int).SetUint64(GasPriceOracleMinTransactionSize),
		ConstantsSourceVersion: GasPriceOracleConstantsSourceVersion,
		Errors:                 map[string]error{},
	}
	fields := []string{"baseFee", "l1BaseFee", "blobBaseFee", "baseFeeScalar", "blobBaseFeeScalar", "decimals", "version"}
	if strings.TrimSpace(l2RPCURL) == "" {
		err := fmt.Errorf("l2_rpc_url is empty (set [rpc].l2_rpc_url in config.toml)")
		for _, f := range fields {
			s.Errors[f] = err
		}
		return s, err
	}

	// One batched POST: the four live fee getters plus the version()
	// drift probe. Order here is mirrored by the result-decode block
	// below.
	calls := []l1.EthCallReq{
		{To: GasPriceOracleAddr, Data: baseFeeSelector},
		{To: GasPriceOracleAddr, Data: l1BaseFeeSelector},
		{To: GasPriceOracleAddr, Data: blobBaseFeeSelector},
		{To: GasPriceOracleAddr, Data: baseFeeScalarSelector},
		{To: GasPriceOracleAddr, Data: blobBaseFeeScalarSelector},
		{To: GasPriceOracleAddr, Data: decimalsSelector},
		{To: GasPriceOracleAddr, Data: versionSelector},
	}
	results, lat, err := l1.EthCallBatch(ctx, hc, l2RPCURL, calls)
	s.Latency = lat
	if err != nil {
		for _, f := range fields {
			s.Errors[f] = err
		}
		return s, nil
	}

	if r := results[0]; r.Err != nil {
		s.Errors["baseFee"] = r.Err
	} else if n, derr := decodeUint256(r.Result); derr != nil {
		s.Errors["baseFee"] = fmt.Errorf("decode baseFee: %w", derr)
	} else {
		s.BaseFee = n
	}

	if r := results[1]; r.Err != nil {
		s.Errors["l1BaseFee"] = r.Err
	} else if n, derr := decodeUint256(r.Result); derr != nil {
		s.Errors["l1BaseFee"] = fmt.Errorf("decode l1BaseFee: %w", derr)
	} else {
		s.L1BaseFee = n
	}

	if r := results[2]; r.Err != nil {
		s.Errors["blobBaseFee"] = r.Err
	} else if n, derr := decodeUint256(r.Result); derr != nil {
		s.Errors["blobBaseFee"] = fmt.Errorf("decode blobBaseFee: %w", derr)
	} else {
		s.BlobBaseFee = n
	}

	if r := results[3]; r.Err != nil {
		s.Errors["baseFeeScalar"] = r.Err
	} else if v, derr := decodeUint32(r.Result); derr != nil {
		s.Errors["baseFeeScalar"] = fmt.Errorf("decode baseFeeScalar: %w", derr)
	} else {
		s.BaseFeeScalar = v
	}

	if r := results[4]; r.Err != nil {
		s.Errors["blobBaseFeeScalar"] = r.Err
	} else if v, derr := decodeUint32(r.Result); derr != nil {
		s.Errors["blobBaseFeeScalar"] = fmt.Errorf("decode blobBaseFeeScalar: %w", derr)
	} else {
		s.BlobBaseFeeScalar = v
	}

	if r := results[5]; r.Err != nil {
		s.Errors["decimals"] = r.Err
	} else if n, derr := decodeUint256(r.Result); derr != nil {
		s.Errors["decimals"] = fmt.Errorf("decode decimals: %w", derr)
	} else {
		s.Decimals = n
	}

	if r := results[6]; r.Err != nil {
		s.Errors["version"] = r.Err
	} else if ver, derr := decodeVersionString(r.Result); derr != nil {
		s.Errors["version"] = derr
	} else {
		s.Version = ver
		s.VersionMatches = ver == GasPriceOracleConstantsSourceVersion
	}
	return s, nil
}

// L1DataFeeSnapshot holds just the live L1 data-fee inputs the
// GasPriceOracle exposes — the two values that move every L1 block.
// It is the live subset of GasPriceOracleSnapshot, fetched on the
// screen's 1s tick while the static oracle parameters (scalars,
// decimals, pinned constants, version) are fetched once. Errors are
// keyed by "l1BaseFee" / "blobBaseFee".
type L1DataFeeSnapshot struct {
	L1BaseFee   *big.Int
	BlobBaseFee *big.Int
	Errors      map[string]error
	Latency     time.Duration
}

// FetchL1DataFeeSnapshot batches l1BaseFee() + blobBaseFee() against the
// GasPriceOracle predeploy in a single POST — cheap enough to poll each
// second. The snapshot is always non-nil so partial rendering works.
func FetchL1DataFeeSnapshot(ctx context.Context, hc *http.Client, l2RPCURL string) *L1DataFeeSnapshot {
	s := &L1DataFeeSnapshot{Errors: map[string]error{}}
	if strings.TrimSpace(l2RPCURL) == "" {
		err := fmt.Errorf("l2_rpc_url is empty (set [rpc].l2_rpc_url in config.toml)")
		s.Errors["l1BaseFee"] = err
		s.Errors["blobBaseFee"] = err
		return s
	}
	calls := []l1.EthCallReq{
		{To: GasPriceOracleAddr, Data: l1BaseFeeSelector},
		{To: GasPriceOracleAddr, Data: blobBaseFeeSelector},
	}
	results, lat, err := l1.EthCallBatch(ctx, hc, l2RPCURL, calls)
	s.Latency = lat
	if err != nil {
		s.Errors["l1BaseFee"] = err
		s.Errors["blobBaseFee"] = err
		return s
	}
	if r := results[0]; r.Err != nil {
		s.Errors["l1BaseFee"] = r.Err
	} else if n, derr := decodeUint256(r.Result); derr != nil {
		s.Errors["l1BaseFee"] = fmt.Errorf("decode l1BaseFee: %w", derr)
	} else {
		s.L1BaseFee = n
	}
	if r := results[1]; r.Err != nil {
		s.Errors["blobBaseFee"] = r.Err
	} else if n, derr := decodeUint256(r.Result); derr != nil {
		s.Errors["blobBaseFee"] = fmt.Errorf("decode blobBaseFee: %w", derr)
	} else {
		s.BlobBaseFee = n
	}
	return s
}

// decodeUint32 parses a single-word eth_call result into a uint32 —
// the scalar getters (baseFeeScalar / blobBaseFeeScalar) return a
// uint32 right-aligned in one ABI word.
func decodeUint32(hexResult string) (uint32, error) {
	buf, err := l1.DecodeHexData(hexResult)
	if err != nil {
		return 0, err
	}
	if len(buf) < 32 {
		return 0, fmt.Errorf("uint32: result too short (%d)", len(buf))
	}
	w, _ := l1.WordAt(buf, 0)
	return l1.WordToUint32(w), nil
}

// decodeVersionString parses an ABI-encoded `string` eth_call result:
// one head word (offset to the tail), followed at that offset by a
// length word and right-padded UTF-8 payload.
func decodeVersionString(hexResult string) (string, error) {
	buf, err := l1.DecodeHexData(hexResult)
	if err != nil {
		return "", err
	}
	if len(buf) < 32 {
		return "", fmt.Errorf("version: result too short (%d)", len(buf))
	}
	offW, _ := l1.WordAt(buf, 0)
	off, err := l1.ReadDynamicOffset(offW)
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	return l1.DecodeStringAt(buf, off)
}
