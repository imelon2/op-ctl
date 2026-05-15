package elnode

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TxPoolStatus is the decoded `txpool_status` reply with both
// hex-encoded counters parsed into native uint64s.
type TxPoolStatus struct {
	Pending uint64
	Queued  uint64
}

// txPoolRaw mirrors the on-wire result envelope so encoding/json can
// fill it before we parse the hex strings into TxPoolStatus.
type txPoolRaw struct {
	Pending string `json:"pending"`
	Queued  string `json:"queued"`
}

// TxPool calls txpool_status on the given execution-layer RPC URL and
// returns the parsed mempool counters + wall-clock latency of the
// round trip. When the `txpool` namespace is disabled on the node,
// the call fails with RPCError{Code: -32601} — detectable via
// IsMethodNotFound.
func TxPool(ctx context.Context, hc *http.Client, url string) (*TxPoolStatus, time.Duration, error) {
	var raw txPoolRaw
	latency, err := callRPC(ctx, hc, url, "txpool_status", []any{}, &raw)
	if err != nil {
		return nil, latency, err
	}
	pending, err := parseHexUint64(raw.Pending)
	if err != nil {
		return nil, latency, fmt.Errorf("decode pending %q: %w", raw.Pending, err)
	}
	queued, err := parseHexUint64(raw.Queued)
	if err != nil {
		return nil, latency, fmt.Errorf("decode queued %q: %w", raw.Queued, err)
	}
	return &TxPoolStatus{Pending: pending, Queued: queued}, latency, nil
}

// parseHexUint64 parses an Ethereum-style hex-encoded uint ("0x10")
// into a uint64. Empty input decodes as 0 (op-geth occasionally
// returns "" for an empty pool on older client versions).
func parseHexUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 16, 64)
}

// parseHexBig parses an Ethereum-style hex-encoded integer of
// arbitrary width into a *big.Int. Returns big.NewInt(0) for the
// empty / "0x" / "0x0" cases.
func parseHexBig(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return big.NewInt(0), nil
	}
	n := new(big.Int)
	if _, ok := n.SetString(s, 16); !ok {
		return nil, fmt.Errorf("not a hex integer: %q", s)
	}
	return n, nil
}

// TxPoolTx is one tx object as returned by txpool_content. All hex
// fields are decoded once at this boundary. Pending is set during
// flattening (true for txs from the `pending` group, false for
// `queued`) so the screen layer can tag P/Q without re-walking the
// wire map.
type TxPoolTx struct {
	Hash, From, To   string
	Nonce, Gas       uint64
	Value            *big.Int
	GasPrice         *big.Int
	MaxFee           *big.Int
	MaxTip           *big.Int
	Type             uint64
	ChainID          uint64
	Input            []byte
	AccessList       json.RawMessage
	R, S, V, YParity string
	Pending          bool

	// In-pool flags are null while pending — kept as *string to
	// preserve "set" vs "explicitly null" semantics.
	BlockHash, BlockNumber, TxIndex *string
}

// txPoolTxRaw mirrors op-geth's wire shape for one tx inside
// txpool_content. Every numeric field arrives as a hex string; we
// decode at the elnode boundary so the screen layer never touches hex.
type txPoolTxRaw struct {
	Hash                 string          `json:"hash"`
	From                 string          `json:"from"`
	To                   string          `json:"to"`
	Nonce                string          `json:"nonce"`
	Gas                  string          `json:"gas"`
	Value                string          `json:"value"`
	GasPrice             string          `json:"gasPrice"`
	MaxFeePerGas         string          `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string          `json:"maxPriorityFeePerGas"`
	Type                 string          `json:"type"`
	ChainID              string          `json:"chainId"`
	Input                string          `json:"input"`
	AccessList           json.RawMessage `json:"accessList"`
	R                    string          `json:"r"`
	S                    string          `json:"s"`
	V                    string          `json:"v"`
	YParity              string          `json:"yParity"`
	BlockHash            *string         `json:"blockHash"`
	BlockNumber          *string         `json:"blockNumber"`
	TxIndex              *string         `json:"transactionIndex"`
}

func decodeTxRaw(raw txPoolTxRaw) (TxPoolTx, error) {
	nonce, err := parseHexUint64(raw.Nonce)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("nonce: %w", err)
	}
	gas, err := parseHexUint64(raw.Gas)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("gas: %w", err)
	}
	value, err := parseHexBig(raw.Value)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("value: %w", err)
	}
	gasPrice, err := parseHexBig(raw.GasPrice)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("gasPrice: %w", err)
	}
	maxFee, err := parseHexBig(raw.MaxFeePerGas)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("maxFeePerGas: %w", err)
	}
	maxTip, err := parseHexBig(raw.MaxPriorityFeePerGas)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("maxPriorityFeePerGas: %w", err)
	}
	txType, err := parseHexUint64(raw.Type)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("type: %w", err)
	}
	chainID, err := parseHexUint64(raw.ChainID)
	if err != nil {
		return TxPoolTx{}, fmt.Errorf("chainId: %w", err)
	}

	input := []byte{}
	if h := strings.TrimPrefix(raw.Input, "0x"); h != "" {
		b := make([]byte, len(h)/2)
		for i := 0; i < len(b); i++ {
			v, perr := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
			if perr != nil {
				return TxPoolTx{}, fmt.Errorf("input byte %d: %w", i, perr)
			}
			b[i] = byte(v)
		}
		input = b
	}

	return TxPoolTx{
		Hash:        raw.Hash,
		From:        raw.From,
		To:          raw.To,
		Nonce:       nonce,
		Gas:         gas,
		Value:       value,
		GasPrice:    gasPrice,
		MaxFee:      maxFee,
		MaxTip:      maxTip,
		Type:        txType,
		ChainID:     chainID,
		Input:       input,
		AccessList:  raw.AccessList,
		R:           raw.R,
		S:           raw.S,
		V:           raw.V,
		YParity:     raw.YParity,
		BlockHash:   raw.BlockHash,
		BlockNumber: raw.BlockNumber,
		TxIndex:     raw.TxIndex,
	}, nil
}

// TxPoolContent calls txpool_content and returns every pending +
// queued tx as a flat slice. Sort order: Pending desc, From asc,
// Nonce asc — pending first, then by sender, then by nonce within a
// sender. The map-of-maps wire shape is flattened at this boundary
// so callers render from a contiguous slice.
func TxPoolContent(ctx context.Context, hc *http.Client, url string) ([]TxPoolTx, time.Duration, error) {
	var raw struct {
		Pending map[string]map[string]txPoolTxRaw `json:"pending"`
		Queued  map[string]map[string]txPoolTxRaw `json:"queued"`
	}
	latency, err := callRPC(ctx, hc, url, "txpool_content", []any{}, &raw)
	if err != nil {
		return nil, latency, err
	}

	out := make([]TxPoolTx, 0)
	flatten := func(group map[string]map[string]txPoolTxRaw, pending bool) error {
		for _, byNonce := range group {
			for _, rawTx := range byNonce {
				tx, derr := decodeTxRaw(rawTx)
				if derr != nil {
					return derr
				}
				tx.Pending = pending
				out = append(out, tx)
			}
		}
		return nil
	}
	if err := flatten(raw.Pending, true); err != nil {
		return nil, latency, err
	}
	if err := flatten(raw.Queued, false); err != nil {
		return nil, latency, err
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pending != out[j].Pending {
			return out[i].Pending
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Nonce < out[j].Nonce
	})
	return out, latency, nil
}
