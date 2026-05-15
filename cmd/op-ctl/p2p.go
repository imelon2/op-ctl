package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
	"op-ctl/internal/namespace"
	"op-ctl/internal/opnode"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/errscreen"
	"op-ctl/internal/tui/menu"
	peerstui "op-ctl/internal/tui/peers"
)

// errBackendCancelled is returned by pickBackend when the operator
// dismisses the picker menu (q/esc). Callers in the looped TUI flow
// translate it into "exit this level" rather than a hard error.
var errBackendCancelled = errors.New("backend selection cancelled")

var (
	p2pConsensusBackend string
	p2pConsensusDir     string
	p2pConsensusTimeout time.Duration
	p2pConsensusPlain   bool

	p2pExecutionBackend string
	p2pExecutionDir     string
	p2pExecutionTimeout time.Duration
	p2pExecutionPlain   bool

	p2pDiscoveryConsensusBackend string
	p2pDiscoveryConsensusDir     string
	p2pDiscoveryConsensusTimeout time.Duration
	p2pDiscoveryConsensusPlain   bool
)

var p2pCmd = &cobra.Command{
	Use:   "p2p",
	Short: "P2P inspection commands",
}

// p2pPeerCmd groups the per-peer commands (consensus / execution) one
// level below p2p so the menu reads `p2p > peer > consensus`. Future
// p2p sub-areas (e.g. gossip, scoring, traffic) can sit alongside
// `peer` instead of crowding p2p's root listing.
var p2pPeerCmd = &cobra.Command{
	Use:   "peer",
	Short: "Per-peer queries (consensus opp2p_peers, execution admin_peers)",
}

// p2pDiscoveryCmd groups the discovery-table commands. Currently only
// `consensus` (op-node opp2p_discoveryTable); execution-side discv4
// dumps could slot in here too if op-geth ever exposes them.
var p2pDiscoveryCmd = &cobra.Command{
	Use:   "discovery",
	Short: "Discovery-table inspection commands",
}

var p2pConsensusCmd = &cobra.Command{
	Use:   "consensus",
	Short: "List opp2p_peers on a consensus-layer node, with namespace name lookup",
	Long: "Calls opp2p_peers(false) on a chosen backend's consensus_rpc_url and prints " +
		"every peer in the dump (currently connected and previously seen). PeerIDs " +
		"that match an entry in the namespace directory (default ./namespace) are " +
		"prefixed with the backend name, e.g. (fullnode)16Uiu2HAk....\n\n" +
		"Without --backend a TUI menu lets you pick which node to query when " +
		"there's more than one configured.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}
		bs := cfg.BackendList()

		idx, err := namespace.LoadIndex(p2pConsensusDir)
		if err != nil {
			return err
		}

		resolver := buildResolver(cfg)
		defer closeResolver(resolver)

		// One-shot mode: --backend or --plain bypasses the loop so the
		// command stays scriptable.
		if p2pConsensusBackend != "" || p2pConsensusPlain {
			b, err := pickBackend(p2pConsensusBackend, bs)
			if err != nil {
				return err
			}
			return runConsensus(cmd, resolver, b, idx)
		}

		// Persistent TUI: loop the backend picker so q in peers (or in a
		// failed RPC overlay) returns here instead of dropping to shell.
		for {
			b, err := pickBackend("", bs)
			if errors.Is(err, errBackendCancelled) {
				return nil
			}
			if err != nil {
				_ = errscreen.Run(err.Error())
				continue
			}
			if err := runConsensus(cmd, resolver, b, idx); err != nil {
				_ = errscreen.Run(err.Error())
			}
		}
	},
}

// runConsensus does the RPC + render half of the consensus flow,
// shared between one-shot mode (--backend or --plain) and the looped
// TUI mode. Plain output goes to stdout; TUI output enters alt-screen
// via peerstui.Run.
func runConsensus(cmd *cobra.Command, resolver *sshtunnel.Resolver, b config.Backend, idx *namespace.Index) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, p2pConsensusTimeout)
	defer cancel()

	hc, err := resolver.HTTPClient(ctx, b.SSHJump)
	if err != nil {
		return fmt.Errorf("ssh tunnel for %s: %w", b.Name, err)
	}
	dump, _, err := opnode.Peers(ctx, hc, b.ConsensusRPCURL, false)
	if err != nil {
		return fmt.Errorf("opp2p_peers on %s (%s): %w", b.Name, b.ConsensusRPCURL, err)
	}
	if p2pConsensusPlain {
		renderPeerDump(cmd.OutOrStdout(), b, dump, idx)
		return nil
	}
	return peerstui.Run(b, dump, idx)
}

// p2pExecutionCmd is the CLI counterpart of the TUI's p2p / execution
// flow: net_peerCount + admin_peers on a backend's execution RPC. The
// admin call commonly fails when the operator hasn't enabled the
// `admin` JSON-RPC namespace; we surface that as a tailored hint
// instead of a raw error string.
var p2pExecutionCmd = &cobra.Command{
	Use:   "execution",
	Short: "Show net_peerCount + admin_peers for an execution-layer node",
	Long: "Calls net_peerCount and admin_peers on a chosen backend's " +
		"execution_rpc_url. net_peerCount lives in the always-on `net` " +
		"namespace; admin_peers requires `admin` to be enabled (e.g. " +
		"--http.api eth,net,web3,admin on op-geth/reth). When admin is " +
		"disabled the count still shows and the per-peer list is replaced " +
		"with a hint explaining how to enable it.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}
		bs := cfg.BackendList()

		idx, err := namespace.LoadIndex(p2pExecutionDir)
		if err != nil {
			return err
		}

		resolver := buildResolver(cfg)
		defer closeResolver(resolver)

		// One-shot CLI mode (scriptable). Loop semantics live in the
		// unified TUI app; this path mirrors p2pConsensusCmd's plain
		// fallback for piping/grep workflows.
		if p2pExecutionBackend != "" || p2pExecutionPlain {
			b, err := pickBackend(p2pExecutionBackend, bs)
			if err != nil {
				return err
			}
			return runExecution(cmd, resolver, b, idx)
		}

		for {
			b, err := pickBackend("", bs)
			if errors.Is(err, errBackendCancelled) {
				return nil
			}
			if err != nil {
				_ = errscreen.Run(err.Error())
				continue
			}
			if err := runExecution(cmd, resolver, b, idx); err != nil {
				_ = errscreen.Run(err.Error())
			}
		}
	},
}

// runExecution issues both calls on the chosen backend and renders
// the result. Always pushes the screen on success or partial success;
// only failed-everything (or a hard CLI write error) bubbles up.
func runExecution(cmd *cobra.Command, resolver *sshtunnel.Resolver, b config.Backend, idx *namespace.Index) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, p2pExecutionTimeout)
	defer cancel()

	hc, err := resolver.HTTPClient(ctx, b.SSHJump)
	if err != nil {
		return fmt.Errorf("ssh tunnel for %s: %w", b.Name, err)
	}
	count, _, countErr := elnode.PeerCount(ctx, hc, b.ExecutionRPCURL)
	peers, _, peersErr := elnode.AdminPeers(ctx, hc, b.ExecutionRPCURL)

	if countErr != nil && peersErr != nil {
		return fmt.Errorf("both execution calls failed on %s (%s):\n  net_peerCount: %v\n  admin_peers: %v",
			b.Name, b.ExecutionRPCURL, countErr, peersErr)
	}

	if p2pExecutionPlain {
		renderExecutionPeers(cmd.OutOrStdout(), b, count, countErr, peers, peersErr, idx)
		return nil
	}
	// TUI is owned by the unified app; one-shot non-plain reuses the
	// app's Run path indirectly via the existing main flow. For
	// `--backend X` without --plain, fall back to plain output (no
	// alt-screen for a single command).
	renderExecutionPeers(cmd.OutOrStdout(), b, count, countErr, peers, peersErr, idx)
	return nil
}

// p2pDiscoveryConsensusCmd is the CLI counterpart of the TUI's
// p2p / discovery / consensus flow: opp2p_discoveryTable on a
// backend's consensus RPC. Discovery is commonly disabled on
// sequencer-style nodes (--p2p.no-discovery); we surface that as a
// tailored hint instead of a raw -32000 error.
var p2pDiscoveryConsensusCmd = &cobra.Command{
	Use:   "consensus",
	Short: "Show opp2p_discoveryTable (ENRs the consensus node has discovered)",
	Long: "Calls opp2p_discoveryTable on a chosen backend's consensus_rpc_url. " +
		"Each ENR is cross-referenced against the namespace dir; matched " +
		"entries get prefixed with the backend name so you can spot which " +
		"of your own nodes are visible in the discovery table.\n\n" +
		"When the node has discovery turned off (op-node returns " +
		"\"discovery disabled\"), the output explains how to re-enable it " +
		"instead of just printing the raw error.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadResolvedConfig()
		if err != nil {
			return err
		}
		bs := cfg.BackendList()

		idx, err := namespace.LoadIndex(p2pDiscoveryConsensusDir)
		if err != nil {
			return err
		}

		resolver := buildResolver(cfg)
		defer closeResolver(resolver)

		if p2pDiscoveryConsensusBackend != "" || p2pDiscoveryConsensusPlain {
			b, err := pickBackend(p2pDiscoveryConsensusBackend, bs)
			if err != nil {
				return err
			}
			return runDiscoveryConsensus(cmd, resolver, b, idx)
		}

		for {
			b, err := pickBackend("", bs)
			if errors.Is(err, errBackendCancelled) {
				return nil
			}
			if err != nil {
				_ = errscreen.Run(err.Error())
				continue
			}
			if err := runDiscoveryConsensus(cmd, resolver, b, idx); err != nil {
				_ = errscreen.Run(err.Error())
			}
		}
	},
}

func runDiscoveryConsensus(cmd *cobra.Command, resolver *sshtunnel.Resolver, b config.Backend, idx *namespace.Index) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, p2pDiscoveryConsensusTimeout)
	defer cancel()

	hc, err := resolver.HTTPClient(ctx, b.SSHJump)
	if err != nil {
		return fmt.Errorf("ssh tunnel for %s: %w", b.Name, err)
	}
	enrs, _, err := opnode.DiscoveryTable(ctx, hc, b.ConsensusRPCURL)
	renderDiscoveryConsensus(cmd.OutOrStdout(), b, enrs, err, idx)
	return nil
}

// closeResolver is a defer-friendly wrapper that swallows non-fatal
// shutdown errors after logging — the program is exiting either way.
func closeResolver(r *sshtunnel.Resolver) {
	if r == nil {
		return
	}
	if err := r.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "ssh resolver close:", err)
	}
}

// renderDiscoveryConsensus is the plain-text counterpart of
// discoveryConsensusScreen. Used by the CLI --plain path; the TUI
// path goes through the unified app program.
func renderDiscoveryConsensus(
	w io.Writer, b config.Backend,
	enrs []string, err error,
	idx *namespace.Index,
) {
	fmt.Fprintf(w, "%s (%s)\n", b.Name, b.ConsensusRPCURL)
	if err != nil {
		if opnode.IsDiscoveryDisabled(err) {
			fmt.Fprintln(w, "discovery: DISABLED (op-node is running with --p2p.no-discovery)")
			fmt.Fprintf(w, "  %v\n", err)
		} else {
			fmt.Fprintf(w, "discovery: ERROR  %v\n", err)
		}
		return
	}
	matched := 0
	for _, e := range enrs {
		if idx.Lookup(e) != "" {
			matched++
		}
	}
	fmt.Fprintf(w, "total: %d  matched: %d\n\n", len(enrs), matched)

	rows := make([]string, len(enrs))
	copy(rows, enrs)
	sort.SliceStable(rows, func(i, j int) bool {
		ai, bi := idx.Lookup(rows[i]), idx.Lookup(rows[j])
		if (ai != "") != (bi != "") {
			return ai != ""
		}
		if ai != bi {
			return ai < bi
		}
		return rows[i] < rows[j]
	})
	for i, e := range rows {
		name := idx.Lookup(e)
		label := e
		if name != "" {
			label = "(" + name + ")" + e
		}
		fmt.Fprintf(w, "  #%d  %s\n", i+1, label)
	}
}

// renderExecutionPeers is the plain-text counterpart of
// executionPeersScreen — used by the CLI --plain path.
func renderExecutionPeers(
	w io.Writer, b config.Backend,
	count uint64, countErr error,
	peers []elnode.AdminPeer, peersErr error,
	idx *namespace.Index,
) {
	fmt.Fprintf(w, "%s (%s)\n", b.Name, b.ExecutionRPCURL)
	if countErr != nil {
		fmt.Fprintf(w, "net_peerCount: ERROR  %v\n", countErr)
	} else {
		fmt.Fprintf(w, "net_peerCount: %d\n", count)
	}

	if peersErr != nil {
		if elnode.IsMethodNotFound(peersErr) {
			fmt.Fprintln(w, "admin_peers:   UNAVAILABLE (admin namespace disabled on this node)")
			fmt.Fprintf(w, "  %v\n", peersErr)
		} else {
			fmt.Fprintf(w, "admin_peers:   ERROR  %v\n", peersErr)
		}
		return
	}
	fmt.Fprintf(w, "admin_peers:   %d\n\n", len(peers))

	rows := make([]elnode.AdminPeer, len(peers))
	copy(rows, peers)
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].ID < rows[j].ID
	})
	for _, p := range rows {
		name := idx.Lookup(p.ID)
		label := p.ID
		if name != "" {
			label = "(" + name + ")" + p.ID
		}
		dir := "out"
		if p.Network.Inbound {
			dir = "in"
		}
		fmt.Fprintf(w, "  %s  %s  [%s, %s]\n", label, p.Name, dir, p.Network.RemoteAddress)
	}
}

// pickBackend returns the backend matching name, or — when name is
// empty — picks via TUI menu (skipped when there's exactly one). An
// empty backend list and "unknown name" both surface as errors so the
// caller doesn't blindly fall through to RPC fan-out with a zero
// Backend value.
func pickBackend(name string, bs []config.Backend) (config.Backend, error) {
	if len(bs) == 0 {
		return config.Backend{}, fmt.Errorf("no backends configured")
	}
	if name != "" {
		for _, b := range bs {
			if b.Name == name {
				return b, nil
			}
		}
		return config.Backend{}, fmt.Errorf("no backend named %q in config", name)
	}
	if len(bs) == 1 {
		return bs[0], nil
	}
	items := make([]menu.Item, len(bs))
	for i, b := range bs {
		items[i] = menu.Item{Name: b.Name, Short: b.ConsensusRPCURL}
	}
	chosen, err := menu.Run(items)
	if err != nil {
		return config.Backend{}, err
	}
	if chosen == "" {
		return config.Backend{}, errBackendCancelled
	}
	for _, b := range bs {
		if b.Name == chosen {
			return b, nil
		}
	}
	return config.Backend{}, fmt.Errorf("internal: chosen backend %q not found", chosen)
}

// peerRow pairs a libp2p peer ID with its op-node-reported attributes
// for ordered rendering. The dump's `peers` field is a map (random
// iteration order), so we copy entries into a slice and sort by ID
// before printing to keep output stable.
type peerRow struct {
	peerID string
	entry  opnode.PeerEntry
}

func renderPeerDump(w io.Writer, b config.Backend, d *opnode.PeerDump, idx *namespace.Index) {
	fmt.Fprintf(w, "%s (%s)\n", b.Name, b.ConsensusRPCURL)
	fmt.Fprintf(w, "totalConnected=%d  totalKnown=%d\n\n", d.TotalConnected, len(d.Peers))

	var connected, other []peerRow
	for id, e := range d.Peers {
		r := peerRow{peerID: id, entry: e}
		if e.Connectedness == 1 {
			connected = append(connected, r)
		} else {
			other = append(other, r)
		}
	}
	sortByID := func(rs []peerRow) {
		sort.Slice(rs, func(i, j int) bool { return rs[i].peerID < rs[j].peerID })
	}
	sortByID(connected)
	sortByID(other)

	writeSection(w, "Connected", connected, idx)
	fmt.Fprintln(w)
	writeSection(w, "Other", other, idx)

	if len(d.BannedPeers) > 0 {
		sort.Strings(d.BannedPeers)
		fmt.Fprintf(w, "\nBanned (%d):\n", len(d.BannedPeers))
		for _, id := range d.BannedPeers {
			fmt.Fprintln(w, "  "+peerLabel(idx, id))
		}
	}
}

func writeSection(w io.Writer, title string, rs []peerRow, idx *namespace.Index) {
	fmt.Fprintf(w, "%s (%d):\n", title, len(rs))
	if len(rs) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, r := range rs {
		fmt.Fprintf(w, "  %s [%s, %s]\n",
			peerLabel(idx, r.peerID),
			connectednessLabel(r.entry.Connectedness),
			directionLabel(r.entry.Direction),
		)
	}
}

// peerLabel renders "(name)peerID" when the namespace index knows the
// peer, otherwise just the bare peerID. The exact format with no space
// between the closing paren and the ID is intentional — that's what the
// operator asked for.
func peerLabel(idx *namespace.Index, peerID string) string {
	if name := idx.Lookup(peerID); name != "" {
		return "(" + name + ")" + peerID
	}
	return peerID
}

func connectednessLabel(c int) string {
	switch c {
	case 0:
		return "NotConnected"
	case 1:
		return "Connected"
	case 2:
		return "CanConnect"
	case 3:
		return "CannotConnect"
	case 4:
		return "Limited"
	default:
		return fmt.Sprintf("Connectedness(%d)", c)
	}
}

func directionLabel(d int) string {
	switch d {
	case 0:
		return "DirUnknown"
	case 1:
		return "Inbound"
	case 2:
		return "Outbound"
	default:
		return fmt.Sprintf("Direction(%d)", d)
	}
}

func init() {
	p2pConsensusCmd.Flags().StringVar(
		&p2pConsensusBackend, "backend", "",
		"skip the menu and query this backend by name",
	)
	p2pConsensusCmd.Flags().StringVar(
		&p2pConsensusDir, "namespace-dir", "./namespace",
		"directory of namespace files used to translate peerIDs to backend names",
	)
	p2pConsensusCmd.Flags().DurationVar(
		&p2pConsensusTimeout, "timeout", 5*time.Second,
		"opp2p_peers RPC timeout (e.g. 5s, 30s, 1m)",
	)
	p2pConsensusCmd.Flags().BoolVar(
		&p2pConsensusPlain, "plain", false,
		"print plain text instead of the TUI (useful for piping)",
	)
	p2pPeerCmd.AddCommand(p2pConsensusCmd)

	p2pExecutionCmd.Flags().StringVar(
		&p2pExecutionBackend, "backend", "",
		"skip the menu and query this backend by name",
	)
	p2pExecutionCmd.Flags().StringVar(
		&p2pExecutionDir, "namespace-dir", "./namespace",
		"directory of namespace files used to translate node IDs to backend names",
	)
	p2pExecutionCmd.Flags().DurationVar(
		&p2pExecutionTimeout, "timeout", 5*time.Second,
		"per-RPC call timeout (e.g. 5s, 30s, 1m)",
	)
	p2pExecutionCmd.Flags().BoolVar(
		&p2pExecutionPlain, "plain", false,
		"print plain text instead of the TUI (useful for piping)",
	)
	p2pPeerCmd.AddCommand(p2pExecutionCmd)

	p2pCmd.AddCommand(p2pPeerCmd)

	p2pDiscoveryConsensusCmd.Flags().StringVar(
		&p2pDiscoveryConsensusBackend, "backend", "",
		"skip the menu and query this backend by name",
	)
	p2pDiscoveryConsensusCmd.Flags().StringVar(
		&p2pDiscoveryConsensusDir, "namespace-dir", "./namespace",
		"directory of namespace files used to translate ENRs to backend names",
	)
	p2pDiscoveryConsensusCmd.Flags().DurationVar(
		&p2pDiscoveryConsensusTimeout, "timeout", 5*time.Second,
		"per-RPC call timeout (e.g. 5s, 30s, 1m)",
	)
	p2pDiscoveryConsensusCmd.Flags().BoolVar(
		&p2pDiscoveryConsensusPlain, "plain", false,
		"print plain text instead of the TUI (useful for piping)",
	)
	p2pDiscoveryCmd.AddCommand(p2pDiscoveryConsensusCmd)
	p2pCmd.AddCommand(p2pDiscoveryCmd)
}
