package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"op-ctl/internal/namespace"
)

var listDir string

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "Show entries saved in the namespace directory",
	Long: "Reads every *.json file in the namespace directory (default " +
		"./namespace, written by `op-ctl namespace`) and prints each backend " +
		"with its consensus (peer_id, node_id, enr) and execution " +
		"(node_id, enode, enr) identifiers. Empty fields are shown as " +
		"\"(empty)\" so it's easy to spot what still needs to be filled in.",
	RunE: func(cmd *cobra.Command, args []string) error {
		// --dir empty (default) means "derive from chain name", so
		// `op-ctl list` reads the same per-chain directory
		// `op-ctl namespace` wrote into.
		dirEff := listDir
		if dirEff == "" {
			cfg, err := loadResolvedConfig()
			if err != nil {
				return err
			}
			dirEff = defaultNamespaceDir(cfg.Path())
		}
		entries, err := namespace.LoadAll(dirEff)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(entries) == 0 {
			fmt.Fprintf(out, "(no entries in %s)\n", dirEff)
			return nil
		}
		for i, e := range entries {
			if i > 0 {
				fmt.Fprintln(out)
			}
			printEntry(out, e)
		}
		return nil
	},
}

func printEntry(out io.Writer, e namespace.Entry) {
	fmt.Fprintln(out, e.Name)
	fmt.Fprintln(out, "  consensus:")
	fmt.Fprintf(out, "    peer_id: %s\n", orEmpty(e.Consensus.PeerID))
	fmt.Fprintf(out, "    node_id: %s\n", orEmpty(e.Consensus.NodeID))
	fmt.Fprintf(out, "    enr:     %s\n", orEmpty(e.Consensus.ENR))
	fmt.Fprintln(out, "  execution:")
	fmt.Fprintf(out, "    node_id: %s\n", orEmpty(e.Execution.NodeID))
	fmt.Fprintf(out, "    enode:   %s\n", orEmpty(e.Execution.Enode))
	fmt.Fprintf(out, "    enr:     %s\n", orEmpty(e.Execution.ENR))
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

func init() {
	listCmd.Flags().StringVar(
		&listDir, "dir", "",
		"directory to read namespace files from (default: ./namespace/<chain>, derived from the config filename)",
	)
}
