package l1

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// ResourceConfig mirrors SystemConfig.sol's ResourceMetering.ResourceConfig
// struct, returned as a single tuple from SystemConfig.resourceConfig().
// All fields are static-sized so the on-wire shape is six 32-byte
// words in declaration order — no dynamic-offset machinery needed.
type ResourceConfig struct {
	MaxResourceLimit            uint32
	ElasticityMultiplier        uint8
	BaseFeeMaxChangeDenominator uint8
	MinimumBaseFee              uint32
	SystemTxMaxGas              uint32
	MaximumBaseFee              *big.Int // uint128
}

// SystemConfigSnapshot is the union of every view-method on the L1
// SystemConfig proxy that op-ctl renders for `read network-fee`.
//
// Per-field errors: a SystemConfig deployed before a given fork
// (Isthmus, Jovian, Ecotone) will revert on selectors the impl
// doesn't carry. Rather than failing the whole snapshot we record the
// failure in Errors[fieldName] and leave the field at its zero value
// — the UI/printer then surfaces "ERR <message>" for that line while
// fork-current fields still render.
type SystemConfigSnapshot struct {
	Address string

	// Ecotone scalars (uint32).
	BasefeeScalar     uint32
	BlobBasefeeScalar uint32

	// Legacy Bedrock scalars (uint256; Ecotone packs both new scalars
	// into the same word, so reading both legacy + new is useful for
	// post-Ecotone L2s).
	Scalar   *big.Int
	Overhead *big.Int

	// L2 gas / EIP-1559 knobs.
	GasLimit           uint64
	EIP1559Denominator uint32
	EIP1559Elasticity  uint32

	// Isthmus+ operator fee.
	OperatorFeeScalar   uint32
	OperatorFeeConstant uint64

	// Jovian+ DA footprint gas scalar (uint16).
	DAFootprintGasScalar uint16

	// MinBaseFee floor (Jovian+).
	MinBaseFee uint64

	// Deposit resource fee market.
	ResourceConfig ResourceConfig

	// Errors maps field name → call/decode error. Absent key means the
	// field is populated.
	Errors map[string]error

	// Latency is the wall-clock duration of the batched RPC POST.
	Latency time.Duration
}

// systemConfigCall mirrors snapshotCall in game.go: pairs a logical
// field name with calldata + a decoder that writes into the snapshot.
type systemConfigCall struct {
	field  string
	data   string
	decode func(raw string, s *SystemConfigSnapshot) error
}

// FetchSystemConfigSnapshot issues one batched JSON-RPC POST for every
// selector in systemConfigCalls() and decodes the results into a
// SystemConfigSnapshot. Per-call reverts populate s.Errors but never
// abort the snapshot — a partial picture (e.g. Isthmus fields missing
// on a pre-Isthmus deployment) is more useful than nothing.
//
// A transport-level failure (HTTP non-2xx, malformed envelope) is
// returned as the function's err return; the partial snapshot is also
// returned so callers can still display Address + breadcrumb.
func FetchSystemConfigSnapshot(ctx context.Context, hc *http.Client, l1RPCURL, systemConfigAddr string) (*SystemConfigSnapshot, error) {
	s := &SystemConfigSnapshot{
		Address: systemConfigAddr,
		Errors:  map[string]error{},
	}
	calls := systemConfigCalls()
	reqs := make([]EthCallReq, len(calls))
	for i, c := range calls {
		reqs[i] = EthCallReq{To: systemConfigAddr, Data: c.data}
	}
	results, latency, err := EthCallBatch(ctx, hc, l1RPCURL, reqs)
	s.Latency = latency
	if err != nil {
		return s, err
	}
	for i, c := range calls {
		r := results[i]
		if r.Err != nil {
			s.Errors[c.field] = r.Err
			continue
		}
		if derr := c.decode(r.Result, s); derr != nil {
			s.Errors[c.field] = derr
		}
	}
	return s, nil
}

func systemConfigCalls() []systemConfigCall {
	decodeUint32Into := func(field string, set func(*SystemConfigSnapshot, uint32)) func(string, *SystemConfigSnapshot) error {
		return func(raw string, s *SystemConfigSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToUint32(w))
			return nil
		}
	}
	decodeUint64Into := func(field string, set func(*SystemConfigSnapshot, uint64)) func(string, *SystemConfigSnapshot) error {
		return func(raw string, s *SystemConfigSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToUint64(w))
			return nil
		}
	}
	decodeUint256Into := func(field string, set func(*SystemConfigSnapshot, *big.Int)) func(string, *SystemConfigSnapshot) error {
		return func(raw string, s *SystemConfigSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToUint256(w))
			return nil
		}
	}

	return []systemConfigCall{
		{
			field:  "basefeeScalar",
			data:   selectorOf("basefeeScalar()"),
			decode: decodeUint32Into("basefeeScalar", func(s *SystemConfigSnapshot, n uint32) { s.BasefeeScalar = n }),
		},
		{
			field:  "blobbasefeeScalar",
			data:   selectorOf("blobbasefeeScalar()"),
			decode: decodeUint32Into("blobbasefeeScalar", func(s *SystemConfigSnapshot, n uint32) { s.BlobBasefeeScalar = n }),
		},
		{
			field:  "scalar",
			data:   selectorOf("scalar()"),
			decode: decodeUint256Into("scalar", func(s *SystemConfigSnapshot, n *big.Int) { s.Scalar = n }),
		},
		{
			field:  "overhead",
			data:   selectorOf("overhead()"),
			decode: decodeUint256Into("overhead", func(s *SystemConfigSnapshot, n *big.Int) { s.Overhead = n }),
		},
		{
			field:  "gasLimit",
			data:   selectorOf("gasLimit()"),
			decode: decodeUint64Into("gasLimit", func(s *SystemConfigSnapshot, n uint64) { s.GasLimit = n }),
		},
		{
			field:  "eip1559Denominator",
			data:   selectorOf("eip1559Denominator()"),
			decode: decodeUint32Into("eip1559Denominator", func(s *SystemConfigSnapshot, n uint32) { s.EIP1559Denominator = n }),
		},
		{
			field:  "eip1559Elasticity",
			data:   selectorOf("eip1559Elasticity()"),
			decode: decodeUint32Into("eip1559Elasticity", func(s *SystemConfigSnapshot, n uint32) { s.EIP1559Elasticity = n }),
		},
		{
			field:  "operatorFeeScalar",
			data:   selectorOf("operatorFeeScalar()"),
			decode: decodeUint32Into("operatorFeeScalar", func(s *SystemConfigSnapshot, n uint32) { s.OperatorFeeScalar = n }),
		},
		{
			field:  "operatorFeeConstant",
			data:   selectorOf("operatorFeeConstant()"),
			decode: decodeUint64Into("operatorFeeConstant", func(s *SystemConfigSnapshot, n uint64) { s.OperatorFeeConstant = n }),
		},
		{
			field: "daFootprintGasScalar",
			data:  selectorOf("daFootprintGasScalar()"),
			decode: func(raw string, s *SystemConfigSnapshot) error {
				buf, err := decodeHexData(raw)
				if err != nil {
					return err
				}
				if len(buf) < 32 {
					return fmt.Errorf("daFootprintGasScalar: result too short (%d)", len(buf))
				}
				w, _ := wordAt(buf, 0)
				s.DAFootprintGasScalar = wordToUint16(w)
				return nil
			},
		},
		{
			field:  "minBaseFee",
			data:   selectorOf("minBaseFee()"),
			decode: decodeUint64Into("minBaseFee", func(s *SystemConfigSnapshot, n uint64) { s.MinBaseFee = n }),
		},
		{
			field: "resourceConfig",
			data:  selectorOf("resourceConfig()"),
			decode: func(raw string, s *SystemConfigSnapshot) error {
				rc, err := decodeResourceConfig(raw)
				if err != nil {
					return err
				}
				s.ResourceConfig = rc
				return nil
			},
		},
	}
}

// decodeResourceConfig parses the 6-word tuple return of
// SystemConfig.resourceConfig(). Each struct field gets its own
// 32-byte word, right-aligned per Solidity's static-tuple ABI rules:
//
//	[ uint32 maxResourceLimit            ]
//	[ uint8  elasticityMultiplier        ]
//	[ uint8  baseFeeMaxChangeDenominator ]
//	[ uint32 minimumBaseFee              ]
//	[ uint32 systemTxMaxGas              ]
//	[ uint128 maximumBaseFee             ]
func decodeResourceConfig(hexStr string) (ResourceConfig, error) {
	var rc ResourceConfig
	buf, err := decodeHexData(hexStr)
	if err != nil {
		return rc, fmt.Errorf("resourceConfig: %w", err)
	}
	if len(buf) < 6*32 {
		return rc, fmt.Errorf("resourceConfig: result too short (%d bytes, want 192)", len(buf))
	}
	w0, _ := wordAt(buf, 0)
	w1, _ := wordAt(buf, 1)
	w2, _ := wordAt(buf, 2)
	w3, _ := wordAt(buf, 3)
	w4, _ := wordAt(buf, 4)
	w5, _ := wordAt(buf, 5)
	rc.MaxResourceLimit = wordToUint32(w0)
	rc.ElasticityMultiplier = wordToUint8(w1)
	rc.BaseFeeMaxChangeDenominator = wordToUint8(w2)
	rc.MinimumBaseFee = wordToUint32(w3)
	rc.SystemTxMaxGas = wordToUint32(w4)
	rc.MaximumBaseFee = wordToUint256(w5)
	return rc, nil
}
