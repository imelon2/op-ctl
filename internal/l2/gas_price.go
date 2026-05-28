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

// GasPriceSnapshot holds the two live gas suggestions the L2 node
// reports plus the base fee derived from them:
//
//	GasPrice       = eth_gasPrice            (baseFee + tip)
//	MaxPriorityFee = eth_maxPriorityFeePerGas (tip)
//	BaseFee        = GasPrice - MaxPriorityFee
//
// All three are in wei. These change every block, so the screen reads
// them on the same tick as the FeeVault balances. Per-field errors
// land in Errors keyed by "gasPrice" / "maxPriorityFee"; BaseFee is
// only computed when both succeed.
type GasPriceSnapshot struct {
	GasPrice       *big.Int
	MaxPriorityFee *big.Int
	BaseFee        *big.Int

	Errors  map[string]error
	Latency time.Duration
}

// FetchGasPriceSnapshot issues eth_maxPriorityFeePerGas + eth_gasPrice
// (two roundtrips) against the L2 RPC and derives BaseFee = gasPrice -
// maxPriorityFee. A failed call records its error in Errors and leaves
// BaseFee nil; the snapshot is always non-nil so partial rendering
// works.
func FetchGasPriceSnapshot(ctx context.Context, hc *http.Client, l2RPCURL string) *GasPriceSnapshot {
	s := &GasPriceSnapshot{Errors: map[string]error{}}
	if strings.TrimSpace(l2RPCURL) == "" {
		err := fmt.Errorf("l2_rpc_url is empty (set [rpc].l2_rpc_url in config.toml)")
		s.Errors["gasPrice"] = err
		s.Errors["maxPriorityFee"] = err
		return s
	}
	tip, tipLat, err := l1.EthMaxPriorityFeePerGas(ctx, hc, l2RPCURL)
	s.Latency += tipLat
	if err != nil {
		s.Errors["maxPriorityFee"] = err
	} else {
		s.MaxPriorityFee = tip
	}
	gp, gpLat, err := l1.EthGasPrice(ctx, hc, l2RPCURL)
	s.Latency += gpLat
	if err != nil {
		s.Errors["gasPrice"] = err
	} else {
		s.GasPrice = gp
	}
	if s.GasPrice != nil && s.MaxPriorityFee != nil {
		s.BaseFee = new(big.Int).Sub(s.GasPrice, s.MaxPriorityFee)
	}
	return s
}
