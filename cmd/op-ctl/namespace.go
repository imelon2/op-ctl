package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"op-ctl/internal/namespace"
	"op-ctl/internal/probe"
)

var (
	namespaceDir     string
	namespaceTimeout time.Duration
	// namespaceRetry: -1 sentinel means "flag not set, fall back to
	// config or hard default". Non-negative values override.
	namespaceRetry int
)

var namespaceCmd = &cobra.Command{
	Use:   "namespace",
	Short: "Snapshot per-backend node identity (peerID, nodeID, ENR) into a directory",
	Long: "For each [backends.*] entry in config.toml, calls opp2p_self on " +
		"consensus_rpc_url and admin_nodeInfo on execution_rpc_url in parallel " +
		"and writes one JSON file per backend into the namespace directory " +
		"(default ./namespace). Fields that can't be retrieved are written as " +
		"empty strings so they can be filled in by hand. The resulting files " +
		"let later commands map opaque peer IDs back to the backend names.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}
		bs := cfg.BackendList()

		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		resolver := buildResolver(cfg)
		defer func() {
			if cerr := resolver.Close(); cerr != nil {
				fmt.Fprintln(os.Stderr, "ssh resolver close:", cerr)
			}
		}()

		// --dir empty (default) means "derive from chain name": for
		// `config.pp-testnet.toml` writes land under
		// `./namespace/pp-testnet/`. Explicit --dir still wins so
		// scripts can pin a custom location.
		dirEff := namespaceDir
		if dirEff == "" {
			dirEff = defaultNamespaceDir(cfg.Path())
		}

		timeoutEff := resolveDuration(namespaceTimeout, cfg.Global.NamespaceTimeout, 5*time.Second)
		retryEff := resolveInt(namespaceRetry, cfg.Global.NamespaceRetry, 3)
		consensus := probe.ProbeAll(ctx, timeoutEff, retryEff, resolver, bs)
		execution := probe.AdminProbeAll(ctx, timeoutEff, retryEff, resolver, bs)

		out := cmd.OutOrStdout()
		for i, b := range bs {
			e := namespace.Entry{Name: b.Name}
			if c := consensus[i]; c.OK && c.Result != nil {
				e.Consensus.PeerID = c.Result.PeerID
				e.Consensus.NodeID = c.Result.NodeID
				e.Consensus.ENR = c.Result.ENR
			}
			if x := execution[i]; x.OK && x.Result != nil {
				e.Execution.NodeID = x.Result.ID
				e.Execution.Enode = x.Result.Enode
				e.Execution.ENR = x.Result.ENR
			}

			path, err := namespace.Write(dirEff, e)
			if err != nil {
				return err
			}

			fmt.Fprintf(out, "%s -> %s [%s]\n", b.Name, path, statusFor(consensus[i], execution[i]))
		}
		return nil
	},
}

// statusFor summarizes which of the two RPC fan-outs succeeded so the
// operator can see at a glance which entries need to be filled in by
// hand.
func statusFor(c probe.Probe, x probe.AdminProbe) string {
	switch {
	case c.OK && x.OK:
		return "ok"
	case !c.OK && !x.OK:
		return "empty (consensus + execution failed)"
	case !c.OK:
		return "partial (consensus failed)"
	default:
		return "partial (execution failed)"
	}
}

func init() {
	namespaceCmd.Flags().StringVar(
		&namespaceDir, "dir", "",
		"directory to write per-backend namespace files into (default: ./namespace/<chain>, derived from the config filename)",
	)
	namespaceCmd.Flags().DurationVar(
		&namespaceTimeout, "timeout", 0,
		"per-RPC call timeout (default: 5s, or [global].namespace_timeout from config)",
	)
	namespaceCmd.Flags().IntVar(
		&namespaceRetry, "retry", -1,
		"number of retry attempts on a failed RPC (default: 3, or [global].namespace_retry from config; 0 disables retries)",
	)
}
