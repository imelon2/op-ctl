package main

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"op-ctl/internal/contracts"
	"op-ctl/internal/l1"
	"op-ctl/internal/l2"
)

var readNetworkFeeTimeout time.Duration

// readNetworkFeeCmd is the second leaf of `op-ctl read`: it surfaces
// every fee-related parameter and accumulator op-ctl can read for
// this chain.
//
// Two RPC endpoints are used:
//   - [rpc].l1_rpc_url → SystemConfig view methods (the L1 source of
//     truth for fee scalars / EIP-1559 / operator fee / DA scalar).
//   - [rpc].l2_rpc_url → FeeVault predeploy reads (balance +
//     totalProcessed for BaseFee, L1Fee, Sequencer, Operator vaults).
//
// The SystemConfigProxy address comes from the same state.json the
// dispute-game command already loads; the FeeVault addresses are
// hard-coded predeploys (Predeploys.sol).
var readNetworkFeeCmd = &cobra.Command{
	Use:   "network-fee",
	Short: "Show L1 SystemConfig fee params + L2 FeeVault balances",
	Long: "Reads every fee-related parameter from the L1 SystemConfig " +
		"(scalars, EIP-1559, operator/DA fees, resource config) plus " +
		"each L2 FeeVault predeploy's live balance and totalProcessed. " +
		"L1 calls go to [rpc].l1_rpc_url; L2 calls go to [rpc].l2_rpc_url. " +
		"SystemConfigProxy comes from the state.json named by [contracts].state_root.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}
		addrs, err := contracts.Load(cfg.Contracts.StateRoot)
		if err != nil {
			return err
		}

		timeoutEff := readNetworkFeeTimeout
		if timeoutEff <= 0 {
			timeoutEff = 10 * time.Second
		}

		return runReadNetworkFeePlain(
			ctx, cmd.OutOrStdout(),
			cfg.RPC.L1RPCURL, cfg.RPC.L2RPCURL,
			addrs.SystemConfigProxy,
			timeoutEff,
		)
	},
}

// runReadNetworkFeePlain fetches both snapshots and prints them as
// plain text. SystemConfig comes from L1; FeeVault balances come from
// L2. Each section reports its wall-clock latency so an operator can
// tell which endpoint is slow.
func runReadNetworkFeePlain(ctx context.Context, out io.Writer, l1RPCURL, l2RPCURL, systemConfigAddr string, timeout time.Duration) error {
	// --- L1: SystemConfig ---
	fmt.Fprintf(out, "l1=%s\n", l1RPCURL)
	if systemConfigAddr == "" {
		fmt.Fprintln(out, "SystemConfig: ERR(SystemConfigProxy not in state.json — add it to opChainDeployments[0])")
	} else {
		scCtx, cancel := context.WithTimeout(ctx, timeout)
		snap, err := l1.FetchSystemConfigSnapshot(scCtx, nil, l1RPCURL, systemConfigAddr)
		cancel()
		fmt.Fprintf(out, "SystemConfig=%s latency=%dms\n", systemConfigAddr, snap.Latency.Milliseconds())
		if err != nil {
			fmt.Fprintf(out, "  TRANSPORT_ERR %v\n", err)
		} else {
			printSystemConfig(out, snap)
		}
	}

	// --- L2: FeeVault predeploys ---
	fmt.Fprintf(out, "\nl2=%s\n", l2RPCURL)
	vaultCtx, cancel := context.WithTimeout(ctx, timeout)
	vaults, vaultLat, vaultErr := l2.FetchAllVaultSnapshots(vaultCtx, nil, l2RPCURL)
	cancel()
	fmt.Fprintf(out, "FeeVaults latency=%dms\n", vaultLat.Milliseconds())
	if vaultErr != nil {
		fmt.Fprintf(out, "  TRANSPORT_ERR %v\n", vaultErr)
	}
	for _, v := range vaults {
		printVault(out, v)
	}

	// --- L2: GasPriceOracle compressed-size regression constants ---
	gpoCtx, cancel := context.WithTimeout(ctx, timeout)
	gpo, gpoErr := l2.FetchGasPriceOracleSnapshot(gpoCtx, nil, l2RPCURL)
	cancel()
	fmt.Fprintf(out, "GasPriceOracle=%s latency=%dms\n", gpo.Address, gpo.Latency.Milliseconds())
	if gpoErr != nil {
		fmt.Fprintf(out, "  TRANSPORT_ERR %v\n", gpoErr)
	} else {
		printGasPriceOracle(out, gpo)
	}

	// --- L2: live gas price suggestions (eth_gasPrice / tip) ---
	gasCtx, cancel := context.WithTimeout(ctx, timeout)
	gas := l2.FetchGasPriceSnapshot(gasCtx, nil, l2RPCURL)
	cancel()
	fmt.Fprintf(out, "GasPrice latency=%dms\n", gas.Latency.Milliseconds())
	printGasPrice(out, gas)

	// --- L2: latest block EIP-1559 params (Jovian extraData) ---
	beCtx, cancel := context.WithTimeout(ctx, timeout)
	be, _ := l2.FetchLatestBlockEIP1559(beCtx, nil, l2RPCURL)
	cancel()
	fmt.Fprintf(out, "BlockEIP1559 latency=%dms\n", be.Latency.Milliseconds())
	printBlockEIP1559(out, be)
	return nil
}

func printGasPrice(out io.Writer, s *l2.GasPriceSnapshot) {
	emit := func(label string, v *big.Int, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%s\n", label, bigStrOrZero(v))
	}
	emit("gasPrice", s.GasPrice, "gasPrice")
	emit("maxPriorityFeePerGas", s.MaxPriorityFee, "maxPriorityFee")
	// baseFee is derived (gasPrice - maxPriorityFeePerGas); only valid
	// when both inputs succeeded.
	if s.Errors["gasPrice"] != nil || s.Errors["maxPriorityFee"] != nil {
		fmt.Fprintln(out, "  baseFee=ERR(needs gasPrice and maxPriorityFeePerGas)")
	} else {
		fmt.Fprintf(out, "  baseFee=%s\n", bigStrOrZero(s.BaseFee))
	}
}

func printBlockEIP1559(out io.Writer, s *l2.BlockEIP1559Snapshot) {
	if len(s.ExtraData) > 0 {
		fmt.Fprintf(out, "  block=%s extraData=0x%x\n", bigStrOrZero(s.BlockNumber), s.ExtraData)
	}
	if s.Err != nil {
		fmt.Fprintf(out, "  ERR %v\n", s.Err)
		return
	}
	fmt.Fprintf(out, "  version=%d (%s)\n", s.Version, s.ForkName())
	fmt.Fprintf(out, "  denominator=%d\n", s.Denominator)
	fmt.Fprintf(out, "  elasticity=%d\n", s.Elasticity)
	if s.HasMinBaseFee {
		fmt.Fprintf(out, "  minBaseFee=%d\n", s.MinBaseFee)
	}
}

func printSystemConfig(out io.Writer, s *l1.SystemConfigSnapshot) {
	emitU32 := func(label string, v uint32, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%d\n", label, v)
	}
	emitU64 := func(label string, v uint64, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%d\n", label, v)
	}
	emitU16 := func(label string, v uint16, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%d\n", label, v)
	}
	emitBig := func(label string, v *big.Int, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%s\n", label, bigStrOrZero(v))
	}
	emitU32("basefeeScalar", s.BasefeeScalar, "basefeeScalar")
	emitU32("blobbasefeeScalar", s.BlobBasefeeScalar, "blobbasefeeScalar")
	emitBig("scalar", s.Scalar, "scalar")
	emitBig("overhead", s.Overhead, "overhead")
	emitU64("gasLimit", s.GasLimit, "gasLimit")
	emitU32("eip1559Denominator", s.EIP1559Denominator, "eip1559Denominator")
	emitU32("eip1559Elasticity", s.EIP1559Elasticity, "eip1559Elasticity")
	emitU32("operatorFeeScalar", s.OperatorFeeScalar, "operatorFeeScalar")
	emitU64("operatorFeeConstant", s.OperatorFeeConstant, "operatorFeeConstant")
	emitU16("daFootprintGasScalar", s.DAFootprintGasScalar, "daFootprintGasScalar")
	emitU64("minBaseFee", s.MinBaseFee, "minBaseFee")

	if e := s.Errors["resourceConfig"]; e != nil {
		fmt.Fprintf(out, "  resourceConfig=ERR(%v)\n", e)
	} else {
		rc := s.ResourceConfig
		fmt.Fprintf(out,
			"  resourceConfig.maxResourceLimit=%d elasticityMultiplier=%d baseFeeMaxChangeDenominator=%d minimumBaseFee=%d systemTxMaxGas=%d maximumBaseFee=%s\n",
			rc.MaxResourceLimit, rc.ElasticityMultiplier, rc.BaseFeeMaxChangeDenominator,
			rc.MinimumBaseFee, rc.SystemTxMaxGas, bigStrOrZero(rc.MaximumBaseFee),
		)
	}
}

func printGasPriceOracle(out io.Writer, s *l2.GasPriceOracleSnapshot) {
	// Live Ecotone+ fee inputs (public getters). Each may carry a
	// per-field error if its eth_call reverted (e.g. a pre-Ecotone
	// chain that lacks the getter).
	emitBig := func(label string, v *big.Int, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%s\n", label, bigStrOrZero(v))
	}
	emitU32 := func(label string, v uint32, field string) {
		if e := s.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		fmt.Fprintf(out, "  %s=%d\n", label, v)
	}
	emitBig("baseFee", s.BaseFee, "baseFee")
	emitBig("l1BaseFee", s.L1BaseFee, "l1BaseFee")
	emitBig("blobBaseFee", s.BlobBaseFee, "blobBaseFee")
	emitU32("baseFeeScalar", s.BaseFeeScalar, "baseFeeScalar")
	emitU32("blobBaseFeeScalar", s.BlobBaseFeeScalar, "blobBaseFeeScalar")
	emitBig("decimals", s.Decimals, "decimals")

	// The three FastLZ→Brotli regression constants are `private
	// constant` in the contract, so they live as inline PUSH operands
	// in the bytecode and cannot be read via eth_call. We print the
	// values pinned in code and rely on the version() drift check
	// below to flag when the deployed contract has likely changed.
	// Values are padded to a uniform width so the trailing
	// "(constant, pinned ...)" suffix lines up across rows.
	pinned := fmt.Sprintf("(constant, pinned to v%s)", s.ConstantsSourceVersion)
	vCost := fmt.Sprintf("%d", s.CostIntercept)
	vCoef := fmt.Sprintf("%d", s.CostFastlzCoef)
	vMin := bigStrOrZero(s.MinTransactionSize)
	valW := max(len(vCost), len(vCoef), len(vMin))
	fmt.Fprintf(out, "  costIntercept=%-*s  %s\n", valW, vCost, pinned)
	fmt.Fprintf(out, "  costFastlzCoef=%-*s  %s\n", valW, vCoef, pinned)
	fmt.Fprintf(out, "  minTransactionSize=%-*s  %s\n", valW, vMin, pinned)
	if e := s.Errors["version"]; e != nil {
		fmt.Fprintf(out, "  version=ERR(%v) — drift undetectable, constants may not match deployed bytecode\n", e)
		return
	}
	if s.VersionMatches {
		fmt.Fprintf(out, "  version=%s (matches pinned v%s)\n", s.Version, s.ConstantsSourceVersion)
	} else {
		fmt.Fprintf(out, "  version=%s (DRIFT: pinned constants are from v%s — values may not match deployed bytecode)\n",
			s.Version, s.ConstantsSourceVersion)
	}
}

func printVault(out io.Writer, v l2.VaultSnapshot) {
	bal := bigStrOrErr(v.Balance, v.Errors["balance"])
	tp := bigStrOrErr(v.TotalProcessed, v.Errors["totalProcessed"])
	fmt.Fprintf(out, "  %s (%s) balance=%s totalProcessed=%s\n",
		v.Name, v.Address, bal, tp,
	)
}

func bigStrOrZero(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func bigStrOrErr(n *big.Int, e error) string {
	if e != nil {
		return fmt.Sprintf("ERR(%v)", e)
	}
	if n == nil {
		return "0"
	}
	return n.String()
}

func init() {
	readNetworkFeeCmd.Flags().DurationVar(
		&readNetworkFeeTimeout, "timeout", 0,
		"per-section RPC timeout (default: 10s)",
	)
	readCmd.AddCommand(readNetworkFeeCmd)
}
