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
	"op-ctl/internal/tui/app"
)

var (
	readDisputeGameTimeout time.Duration
	readDisputeGamePlain   bool
	readDisputeGameAddress string
)

// readCmd groups read-only L1 contract queries. The leaf subcommands
// each map to a specific view-method call on an L1 contract whose
// address is loaded from the state.json named by [contracts].state_root,
// dialed against the endpoint at [rpc].l1_rpc_url.
var readCmd = &cobra.Command{
	Use:   "read",
	Short: "Read L1 contract state (DisputeGameFactory, ...)",
	Long: "Read-only L1 contract calls used by op-ctl to inspect this " +
		"chain's settlement-layer state. Endpoint comes from " +
		"[rpc].l1_rpc_url; contract addresses are loaded from the " +
		"state.json named by [contracts].state_root.",
}

// readDisputeGameCmd is the first leaf of `op-ctl read`: returns the
// running counter of dispute games created by DisputeGameFactoryProxy
// on L1. Future iterations will add `list` / `at` subcommands that
// drill into individual games using gameAtIndex(); the screen layout
// already foreshadows that flow.
var readDisputeGameCmd = &cobra.Command{
	Use:   "dispute-game",
	Short: "Call DisputeGameFactoryProxy.gameCount() and show how many games exist",
	Long: "Calls gameCount() on the DisputeGameFactoryProxy address from " +
		"state.json over the L1 RPC endpoint. The returned uint256 is " +
		"the total number of dispute games ever created by the factory " +
		"— each withdrawal-finalization fault proof produces one entry.",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Signal context unblocks both the --plain select loop and the
		// alt-screen tea.Program on Ctrl+C / SIGTERM. Cobra is not
		// configured to install one in this repo, mirroring state.go.
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

		// timeout precedence: --timeout flag > 10s hard default.
		// No config knob yet — added when an operator demonstrates a
		// flaky L1 endpoint that benefits from a per-chain default.
		timeoutEff := readDisputeGameTimeout
		if timeoutEff <= 0 {
			timeoutEff = 10 * time.Second
		}

		if readDisputeGamePlain {
			out := cmd.OutOrStdout()
			if readDisputeGameAddress != "" {
				return runReadDisputeGamePlainDetail(ctx, out, cfg.URLs.L1RPCURL, readDisputeGameAddress, timeoutEff)
			}
			return runReadDisputeGamePlainList(ctx, out, cfg.URLs.L1RPCURL, addrs.DisputeGameFactoryProxy, timeoutEff)
		}
		if readDisputeGameAddress != "" {
			return app.RunReadDisputeGameDetail(ctx, cfg.URLs.L1RPCURL, readDisputeGameAddress, timeoutEff)
		}
		return app.RunReadDisputeGame(ctx, cfg.URLs.L1RPCURL, addrs.DisputeGameFactoryProxy, timeoutEff)
	},
}

// runReadDisputeGamePlainList prints factory version + gameCount and
// the 10 newest games (one game per line) as plain text — useful for
// piping into grep / scripting.
func runReadDisputeGamePlainList(ctx context.Context, out io.Writer, l1RPCURL, factoryAddr string, timeout time.Duration) error {
	headerCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	calls := []l1.EthCallReq{
		{To: factoryAddr, Data: l1.VersionSelectorData()},
		{To: factoryAddr, Data: l1.GameCountSelectorData()},
	}
	results, hdrLat, err := l1.EthCallBatch(headerCtx, nil, l1RPCURL, calls)
	if err != nil {
		return fmt.Errorf("header batch: %w", err)
	}
	var version string
	if results[0].Err == nil {
		version, _ = l1.DecodeVersionResult(results[0].Result)
	}
	var count *big.Int
	if results[1].Err == nil {
		count, _ = l1.DecodeUint256Result(results[1].Result)
	}
	fmt.Fprintf(out, "factory=%s l1=%s\n", factoryAddr, l1RPCURL)
	fmt.Fprintf(out, "version=%s gameCount=%s header_latency=%dms\n",
		orErr(version, results[0].Err),
		orErrBig(count, results[1].Err),
		hdrLat.Milliseconds(),
	)
	if count == nil || count.Sign() == 0 {
		fmt.Fprintln(out, "(no games to list)")
		return nil
	}
	// Newest 10 indices.
	total := count.Uint64()
	n := uint64(10)
	if total < n {
		n = total
	}
	indices := make([]uint64, 0, n)
	for i := uint64(0); i < n; i++ {
		indices = append(indices, total-1-i)
	}
	listCtx, cancelList := context.WithTimeout(ctx, timeout)
	defer cancelList()
	listings, errs, listLat, err := l1.GameAtIndexBatch(listCtx, nil, l1RPCURL, factoryAddr, indices)
	if err != nil {
		return fmt.Errorf("gameAtIndex batch: %w", err)
	}
	fmt.Fprintf(out, "list_latency=%dms\n", listLat.Milliseconds())
	for i, g := range listings {
		if errs[i] != nil {
			fmt.Fprintf(out, "  idx=%d ERR %v\n", g.Index, errs[i])
			continue
		}
		t := time.Unix(int64(g.Timestamp), 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(out, "  idx=%d gameType=%d proxy=%s createdAt=%s\n",
			g.Index, g.GameType, g.Proxy, t)
	}
	return nil
}

// runReadDisputeGamePlainDetail issues one batched snapshot fetch for
// the supplied game address and prints every field (one per line) so
// the output stays trivially greppable.
func runReadDisputeGamePlainDetail(ctx context.Context, out io.Writer, l1RPCURL, gameAddr string, timeout time.Duration) error {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	snap, err := l1.FetchGameSnapshot(callCtx, nil, l1RPCURL, gameAddr)
	if err != nil {
		// snap is still populated with the address — emit the failure
		// inline so callers see the partial context.
		fmt.Fprintf(out, "address=%s l1=%s ERR %v\n", gameAddr, l1RPCURL, err)
		return err
	}
	fmt.Fprintf(out, "address=%s l1=%s snapshot_latency=%dms\n", snap.Address, l1RPCURL, snap.Latency.Milliseconds())
	emit := func(label string, value string, field string) {
		if e := snap.Errors[field]; e != nil {
			fmt.Fprintf(out, "  %s=ERR(%v)\n", label, e)
			return
		}
		if value == "" {
			fmt.Fprintf(out, "  %s=(empty)\n", label)
			return
		}
		fmt.Fprintf(out, "  %s=%s\n", label, value)
	}
	emit("version", snap.Version, "version")
	emit("gameType", fmt.Sprintf("%d", snap.GameType), "gameData")
	emit("l2ChainId", bigStr(snap.L2ChainID), "l2ChainId")
	emit("gameCreator", snap.GameCreator, "gameCreator")
	emit("status", snap.Status.String(), "status")
	emit("createdAt", timeStr(snap.CreatedAt), "createdAt")
	emit("resolvedAt", timeStr(snap.ResolvedAt), "resolvedAt")
	emit("proposer", snap.Proposer, "proposer")
	emit("challenger", snap.Challenger, "challenger")
	emit("l2BlockNumberChallenged", fmt.Sprintf("%t", snap.L2BlockNumberChallenged), "l2BlockNumberChallenged")
	emit("l2BlockNumberChallenger", snap.L2BlockNumberChallenger, "l2BlockNumberChallenger")
	emit("rootClaim", snap.RootClaim, "gameData")
	emit("l1Head", snap.L1Head, "l1Head")
	emit("extraData", snap.ExtraData, "gameData")
	emit("l2BlockNumber", bigStr(snap.L2BlockNumber), "l2BlockNumber")
	emit("claimDataLen", bigStr(snap.ClaimDataLen), "claimDataLen")
	emit("anchorStateRegistry", snap.AnchorStateRegistry, "anchorStateRegistry")
	emit("startingBlockNumber", bigStr(snap.StartingBlockNumber), "startingBlockNumber")
	emit("startingRootHash", snap.StartingRootHash, "startingRootHash")
	emit("absolutePrestate", snap.AbsolutePrestate, "absolutePrestate")
	emit("vm", snap.VM, "vm")
	emit("weth", snap.WETH, "weth")
	emit("maxGameDepth", bigStr(snap.MaxGameDepth), "maxGameDepth")
	emit("splitDepth", bigStr(snap.SplitDepth), "splitDepth")
	emit("maxClockDuration", fmt.Sprintf("%d", snap.MaxClockDuration), "maxClockDuration")
	emit("clockExtension", fmt.Sprintf("%d", snap.ClockExtension), "clockExtension")

	// Phase 2: claimData[] + getChallengerDuration() per claim. Only
	// fired when ClaimDataLen > 0 (avoids an empty RPC batch).
	if snap.ClaimDataLen != nil && snap.ClaimDataLen.Sign() > 0 {
		n := snap.ClaimDataLen.Uint64()
		cdCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		claims, errs, claimLat, err := l1.FetchClaimData(cdCtx, nil, l1RPCURL, gameAddr, n)
		if err != nil {
			fmt.Fprintf(out, "  claimData=ERR(%v) claimData_latency=%dms\n", err, claimLat.Milliseconds())
			return nil
		}
		fmt.Fprintf(out, "  claimData_count=%d claimData_latency=%dms\n", len(claims), claimLat.Milliseconds())
		for i, cd := range claims {
			if errs[i] != nil {
				fmt.Fprintf(out, "  claim[%d]=ERR(%v)\n", i, errs[i])
				continue
			}
			parent := "—"
			if cd.HasParent() {
				parent = fmt.Sprintf("%d", cd.ParentIndex)
			}
			counteredBy := "—"
			if cd.IsCountered() {
				counteredBy = cd.CounteredBy
			}
			fmt.Fprintf(out,
				"  claim[%d] parent=%s claimant=%s counteredBy=%s bond=%s claim=%s position=%s clock=%s remaining=%ds\n",
				cd.Index, parent, cd.Claimant, counteredBy,
				bigStr(cd.Bond), cd.Claim, bigStr(cd.Position), bigStr(cd.Clock),
				cd.ChallengerDuration,
			)
		}
	}
	return nil
}

func orErr(s string, e error) string {
	if e != nil {
		return fmt.Sprintf("ERR(%v)", e)
	}
	return s
}

func orErrBig(n *big.Int, e error) string {
	if e != nil {
		return fmt.Sprintf("ERR(%v)", e)
	}
	return bigStr(n)
}

func bigStr(n *big.Int) string {
	if n == nil {
		return ""
	}
	return n.String()
}

func timeStr(unix uint64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(int64(unix), 0).UTC().Format(time.RFC3339)
}

func init() {
	readDisputeGameCmd.Flags().DurationVar(
		&readDisputeGameTimeout, "timeout", 0,
		"per-RPC timeout (default: 10s)",
	)
	readDisputeGameCmd.Flags().BoolVar(
		&readDisputeGamePlain, "plain", false,
		"print to stdout instead of the alt-screen TUI (for piping/grep)",
	)
	readDisputeGameCmd.Flags().StringVar(
		&readDisputeGameAddress, "address", "",
		"FaultDisputeGame proxy address to inspect; when set, shows the per-game detail instead of the paginated list",
	)
	readCmd.AddCommand(readDisputeGameCmd)
}
