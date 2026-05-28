package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"op-ctl/internal/config"
	"op-ctl/internal/sshtunnel"
	"op-ctl/internal/tui/app"
)

var configPath string

var rootCmd = &cobra.Command{
	Use:           "op-ctl",
	Short:         "OP-Stack L2 paychain CLI",
	Long:          "op-ctl inspects op-stack-based L2 paychain nodes. Chains are listed in config.yaml (next to the binary) and resolved to per-chain TOML configs.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runApp,
}

// runApp is the rootCmd RunE: invoked when op-ctl is launched without
// a subcommand. It loads config and hands control to the unified
// bubbletea app, which drives every screen (root menu / submenu /
// backend picker / peers / list output / namespace output / errors)
// inside one alt-screen so transitions are seamless.
//
// Direct subcommand calls (`op-ctl namespace`, `op-ctl p2p consensus
// --backend ...`, etc.) bypass this entirely and run their own flows
// — useful for scripting and the existing --plain output.
func runApp(cmd *cobra.Command, _ []string) error {
	path, candidates, err := resolveConfigPath(configPath, defaultConfigYAML(), true)
	if err != nil {
		return err
	}
	if path != "" {
		// Single or explicit-path flow: load now, run the App directly.
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		resolver := buildResolver(cfg)
		defer func() {
			if cerr := resolver.Close(); cerr != nil {
				fmt.Fprintln(os.Stderr, "ssh resolver close:", cerr)
			}
		}()
		// Per-RPC timeout resolves from [global].namespace_timeout
		// with a 5s fallback. Namespace dir is partitioned by chain
		// name derived from the config filename so per-chain JSON
		// outputs never collide.
		timeout := resolveDuration(0, cfg.Global.NamespaceTimeout, 5*time.Second)
		return app.Run(cmd, cfg, resolver, defaultNamespaceDir(path), timeout)
	}
	// Multi-config flow: hand the picker to the App so both phases
	// share one alt-screen — no flicker between picker exit and main
	// menu entry. The loader closure also derives the per-chain
	// namespace dir from the operator's selected path.
	loader := func(chosen string) (*config.Config, *sshtunnel.Resolver, string, time.Duration, error) {
		cfg, err := config.Load(chosen)
		if err != nil {
			return nil, nil, "", 0, err
		}
		timeout := resolveDuration(0, cfg.Global.NamespaceTimeout, 5*time.Second)
		return cfg, buildResolver(cfg), defaultNamespaceDir(chosen), timeout, nil
	}
	return app.RunWithPicker(cmd.Context(), cmd, candidates, loader)
}

// buildResolver converts the TOML-decoded inline bastion table into the
// shape sshtunnel.NewResolver expects. config.Bastion and
// sshtunnel.BastionConfig are kept as separate types so the config
// package stays free of crypto/ssh dependencies — this single seam owns
// the translation.
func buildResolver(cfg *config.Config) *sshtunnel.Resolver {
	inline := make(map[string]sshtunnel.BastionConfig, len(cfg.Bastions))
	for name, b := range cfg.Bastions {
		inline[name] = sshtunnel.BastionConfig{
			Alias:             name,
			Host:              b.Host,
			Port:              b.Port,
			User:              b.User,
			IdentityFile:      b.IdentityFile,
			KnownHosts:        b.KnownHosts,
			ProxyJump:         b.ProxyJump,
			KeepaliveInterval: b.KeepaliveInterval,
		}
	}
	return sshtunnel.NewResolver(inline, nil)
}

// resolveInt mirrors resolveDuration for int knobs. The flag sentinel
// is -1 ("unset") because 0 is a meaningful explicit value (e.g.,
// "retry 0 times"). When flagVal < 0, the config value is used; when
// the config is also zero / unset the hard default applies.
func resolveInt(flagVal, cfgVal, fallback int) int {
	if flagVal >= 0 {
		return flagVal
	}
	if cfgVal > 0 {
		return cfgVal
	}
	return fallback
}

// chainNameFromConfigPath derives the chain slug used as a sub-folder
// of ./namespace. The on-disk convention is `config.<chain>.toml`;
// anything else falls back to the bare basename minus `.toml`. An
// empty or unrecognized path returns "default" so writes never land
// in a bare `./namespace/` shared across chains.
func chainNameFromConfigPath(p string) string {
	base := filepath.Base(p)
	base = strings.TrimSuffix(base, ".toml")
	base = strings.TrimPrefix(base, "config.")
	if base == "" || base == "config" {
		return "default"
	}
	return base
}

// effectiveNamespaceDir returns the directory subcommands should read
// namespace files from. When the operator passed an explicit
// --namespace-dir / --dir flag, it wins (no rewriting). Otherwise
// fall back to defaultNamespaceDir, anchored at the config file's
// project root so the path is cwd-independent.
func effectiveNamespaceDir(flag, configPath string) string {
	if flag != "" {
		return flag
	}
	return defaultNamespaceDir(configPath)
}

// defaultNamespaceDir returns the per-chain default namespace
// directory. The path is resolved relative to the *parent* of the
// config file's directory — i.e. project_root/namespace/<chain> — so
// op-ctl finds the same JSON snapshots regardless of which cwd it
// was launched from. Operators can still override via the
// subcommand's --dir flag when they keep namespace files elsewhere.
//
// Resolution logic mirrors [contracts].state_root: relative paths are
// re-anchored to the directory containing the config file. The repo
// layout under config/<chain>.toml puts namespace/ as a sibling, so
// the "parent of config dir" walk lands in the expected place.
func defaultNamespaceDir(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		// Fallback: cwd-relative — preserves prior behavior if Abs
		// ever fails (unusual but theoretically possible on a path
		// that resolves through a deleted parent during invocation).
		return filepath.Join("./namespace", chainNameFromConfigPath(configPath))
	}
	projectRoot := filepath.Dir(filepath.Dir(abs))
	return filepath.Join(projectRoot, "namespace", chainNameFromConfigPath(configPath))
}

// defaultConfigYAML returns the path to op-ctl's chain-registry file
// (config.yaml) when --config is not given. It resolves to a sibling
// of the binary itself (via os.Executable) so an alias like
// `alias op-ctl='/path/to/op-ctl'` works from any cwd. Falls back to
// the bare relative `config.yaml` when os.Executable fails.
func defaultConfigYAML() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(filepath.Dir(exe), "config.yaml")
}

// resolveConfigPath picks how op-ctl should load its TOML config. It
// returns exactly one of `path` or `candidates` (never both):
//
//   - explicit != "" — returns explicit as `path`, candidates nil.
//   - 0 chains in yaml — returns err naming the yaml path.
//   - 1 chain — returns that chain's TOML as `path`.
//   - 2+ chains && allowPicker — returns the candidate slice; the
//     caller is expected to drive an in-App picker (so picker and
//     menu share one alt-screen — no flicker between them).
//   - 2+ chains && !allowPicker — returns err suggesting --config.
//
// allowPicker is true only for the root TUI path. Subcommands are
// scripting-facing and must surface a deterministic error instead.
//
// The chain registry lives at <yamlPath> (typically config.yaml next
// to the binary): a YAML map of chain-name → { config: <toml path> }.
// Relative paths inside resolve against the YAML file's directory.
func resolveConfigPath(explicit, yamlPath string, allowPicker bool) (path string, candidates []config.ChainEntry, err error) {
	if explicit != "" {
		return explicit, nil, nil
	}
	chains, derr := config.DiscoverChains(yamlPath)
	if derr != nil {
		return "", nil, derr
	}
	switch {
	case len(chains) == 0:
		return "", nil, fmt.Errorf(
			"no chains defined in %s; pass --config <path> or add an entry "+
				"(map of <chain>: { config: <toml path> })",
			yamlPath,
		)
	case len(chains) == 1:
		return chains[0].ConfigPath, nil, nil
	}
	if !allowPicker {
		names := make([]string, len(chains))
		for i, c := range chains {
			names[i] = c.Name
		}
		return "", nil, fmt.Errorf(
			"multiple chains in %s: %s; pass --config <toml path> to select one",
			yamlPath, strings.Join(names, ", "),
		)
	}
	return "", chains, nil
}

// loadResolvedConfig is the subcommand entry-point: resolves --config
// (or auto-picks the lone discovery match, or errors with a candidate
// list) and loads the chosen TOML. The interactive picker is never
// invoked here — subcommands are scripting-facing, so an ambiguous
// state surfaces as an error rather than blocking on a TTY prompt.
func loadResolvedConfig() (*config.Config, error) {
	path, _, err := resolveConfigPath(configPath, defaultConfigYAML(), false)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

func init() {
	rootCmd.PersistentFlags().StringVar(
		&configPath, "config", "",
		"path to TOML config defining [backends.*]; when empty, the chain is resolved from <binary>/config.yaml (a map of chain-name → { config: <toml path> }) — if it lists multiple chains, an interactive picker selects one (subcommands error out instead and require --config)",
	)
	rootCmd.AddCommand(namespaceCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(p2pCmd)
	rootCmd.AddCommand(stateCmd)
	rootCmd.AddCommand(readCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
