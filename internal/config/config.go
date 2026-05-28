// Package config loads op-ctl's TOML configuration with deterministic
// source-order preservation for [backends.*] tables.
//
// Source order is preserved via toml.MetaData.Keys() because BurntSushi/toml
// decodes Go map[string]T into hash order. The TUI must show backend cards
// in the order the operator wrote them in config.toml, so we capture the
// key sequence at decode time and expose it via BackendList().
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	sshconfig "github.com/kevinburke/ssh_config"
)

// WarningWriter receives non-fatal Load() warnings (e.g. an alias defined
// in both config.toml and ~/.ssh/config). Tests override this to capture
// the output; production keeps os.Stderr.
var WarningWriter io.Writer = os.Stderr

// SSHConfigGet reads a key for an alias from the operator's ~/.ssh/config.
// Wrapped behind a package-level variable so tests can override it
// without touching the kevinburke/ssh_config global cache, which would
// otherwise leak state between tests in the same process.
var SSHConfigGet = sshconfig.Get

// Backend models a single [backends.<name>] table.
//
// Name is injected from the TOML map key (TOML decodes the table name into
// the map key, not into a struct field). ConsensusRPCURL is the op-node
// endpoint used by `op-ctl p2p self` (opp2p namespace); ExecutionRPCURL
// is the op-geth / op-erigon endpoint used by `op-ctl admin info` (admin
// namespace). Either may be empty if only one subcommand is used; the
// missing-URL error surfaces from the JSON-RPC call site, not config load.
//
// SSHJump, when non-empty, names an SSH bastion alias the RPC traffic for
// this backend must traverse. The alias is resolved against Config.Bastions
// first (inline definitions); if absent there, ~/.ssh/config is consulted.
// A single alias applies to BOTH ConsensusRPCURL and ExecutionRPCURL — the
// real-world case is that consensus + execution sit in the same VPC behind
// the same bastion.
type Backend struct {
	Name            string `toml:"-"`
	ExecutionRPCURL string `toml:"execution_rpc_url"`
	ConsensusRPCURL string `toml:"consensus_rpc_url"`
	SSHJump         string `toml:"ssh_jump"`
}

// Bastion models a single [bastions.<name>] table — an inline SSH bastion
// definition for operators who do not maintain ~/.ssh/config.
//
// KeepaliveIntervalRaw is the on-disk string ("30s"); KeepaliveInterval is
// the parsed value populated by Load.
// Path fields (IdentityFile, KnownHosts) are not pre-expanded here — the
// sshtunnel package consumes them and applies expansion at dial time so
// the raw config remains debug-friendly.
//
// ProxyJump, when set, names another alias the dialer must reach FIRST
// — analogous to OpenSSH's ProxyJump directive. The chain can be N deep
// (laptop → A → B → … → this) and each link can be either another inline
// [bastions.X] entry OR a Host block in ~/.ssh/config. Cycles are rejected
// at Load() time so the dialer never has to defend against them.
type Bastion struct {
	Name                 string `toml:"-"`
	Host                 string `toml:"host"`
	Port                 int    `toml:"port"`
	User                 string `toml:"user"`
	IdentityFile         string `toml:"identity_file"`
	KnownHosts           string `toml:"known_hosts"`
	ProxyJump            string `toml:"proxy_jump"`
	KeepaliveIntervalRaw string `toml:"keepalive_interval"`

	KeepaliveInterval time.Duration `toml:"-"`
}

// Config is the decoded op-ctl configuration.
//
// keyOrder records the source-file order of [backends.<name>] tables and
// is the source of truth for BackendList() — the Backends map alone is
// unordered.
type Config struct {
	Global    GlobalConfig       `toml:"global"`
	RPC       RPCConfig          `toml:"rpc"`
	Contracts ContractsConfig    `toml:"contracts"`
	Backends  map[string]Backend `toml:"backends"`
	Bastions  map[string]Bastion `toml:"bastions"`
	State     StateConfig        `toml:"state"`

	keyOrder []string
	path     string
}

// RPCConfig holds RPC endpoints op-ctl reads from.
//
// L1RPCURL is the settlement-layer endpoint used for view calls on L1
// contracts (DisputeGameFactoryProxy, SystemConfigProxy, ...). L2RPCURL
// targets the L2 EL node for view calls on L2 predeploys (BaseFeeVault,
// SequencerFeeVault, ...) and for eth_getBalance on those vaults.
//
// op-ctl is L2-paychain centric so this is strictly read-only access on
// both sides (no signing keys, no transaction submission).
type RPCConfig struct {
	L1RPCURL string `toml:"l1_rpc_url"`
	L2RPCURL string `toml:"l2_rpc_url"`
}

// ContractsConfig points at the on-disk state.json (op-deployer output)
// that lists this chain's deployed L1 contract addresses. The path is
// resolved relative to the directory of the loading config.toml when not
// absolute, so an operator can write `./config/state.json` regardless
// of the cwd op-ctl was invoked from.
type ContractsConfig struct {
	StateRoot string `toml:"state_root"`
}

// GlobalConfig holds process-wide defaults under the [global] TOML
// section. Per-subcommand tables (p2p.*, admin.*, state.*) still own
// their own defaults; [global] is for settings that span subcommands
// — currently the namespace-write timeout (consumed by both
// `op-ctl namespace` and the TUI's namespace screen) and the L2 block
// time used by `op-ctl status block` to translate L2 lag counts into
// human-readable durations.
type GlobalConfig struct {
	NamespaceTimeoutRaw string `toml:"namespace_timeout"`
	L2BlockTimeRaw      string `toml:"l2_block_time"`
	NamespaceRetryRaw   *int   `toml:"namespace_retry"`

	NamespaceTimeout time.Duration `toml:"-"`
	L2BlockTime      time.Duration `toml:"-"`
	// NamespaceRetry is the number of *additional* attempts on a
	// failed RPC during `op-ctl namespace` (so 3 means up to 4 total
	// attempts per backend per probe). 0 disables retries. Defaults
	// to 3 when unset.
	NamespaceRetry int `toml:"-"`
}

// StateConfig namespaces defaults for `op-ctl status ...` subcommands.
// The TOML key remained `state` even after the CLI rename (status); a
// rename in the config schema would break existing operator files.
type StateConfig struct {
	Block  StateBlockConfig  `toml:"block"`
	TxPool StateTxPoolConfig `toml:"txpool"`
}

// StateBlockConfig holds defaults for `op-ctl status block`. Adds an
// IntervalRaw because polling cadence is a config-worthy default.
type StateBlockConfig struct {
	TimeoutRaw  string `toml:"timeout"`
	IntervalRaw string `toml:"interval"`
	Fullscreen  bool   `toml:"fullscreen"` // reserved; not yet consumed.

	Timeout  time.Duration `toml:"-"`
	Interval time.Duration `toml:"-"`
}

// StateTxPoolConfig holds defaults for `op-ctl status txpool`. Same
// shape as StateBlockConfig — they share the poll-then-render loop.
type StateTxPoolConfig struct {
	TimeoutRaw  string `toml:"timeout"`
	IntervalRaw string `toml:"interval"`
	Fullscreen  bool   `toml:"fullscreen"` // reserved; not yet consumed.

	Timeout  time.Duration `toml:"-"`
	Interval time.Duration `toml:"-"`

	Detail StateTxPoolDetailConfig `toml:"detail"`
}

// StateTxPoolDetailConfig holds defaults for the drill-down detail
// screen reached from `op-ctl status txpool`. The `Refresh` knob
// controls the Stage-1 list auto-refresh cadence (the compact
// txpool_inspect blob). Default 10s; explicit "0s" disables auto-refresh
// (manual-only); values >0 and <5s are clamped up to 5s with a warning.
// The heavy txpool_content RPC is NEVER on this timer — it fires only
// on Stage-2 user action.
type StateTxPoolDetailConfig struct {
	RefreshRaw string `toml:"refresh"`

	Refresh time.Duration `toml:"-"`
}

// Load reads the TOML file at path, validates it, and returns a Config
// with backend source order captured.
//
// MetaData.Keys() emits a Key per encountered table/value path. For
// [backends.sequencer] it emits ["backends", "sequencer"]. For
// [backends.sequencer.headers] it would also emit
// ["backends", "sequencer", "headers"] (and the parent separately). We
// filter for len(k) == 2 && k[0] == "backends" and dedup by k[1] keeping
// first occurrence.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}
	c.path = path

	seen := map[string]bool{}
	for _, k := range md.Keys() {
		if len(k) == 2 && k[0] == "backends" && !seen[k[1]] {
			seen[k[1]] = true
			c.keyOrder = append(c.keyOrder, k[1])
		}
	}

	if len(c.Backends) == 0 {
		return nil, fmt.Errorf(
			"config: %s: no [backends.*] tables found (at least one backend required)",
			path,
		)
	}
	for name, b := range c.Backends {
		if b.ConsensusRPCURL == "" {
			return nil, fmt.Errorf(
				"config: %s: backend %q missing required consensus_rpc_url",
				path, name,
			)
		}
		b.Name = name
		c.Backends[name] = b
	}

	if err := c.finalizeBastions(path); err != nil {
		return nil, err
	}
	if err := c.validateSSHJumps(path); err != nil {
		return nil, err
	}

	if raw := c.Global.NamespaceTimeoutRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: global.namespace_timeout %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d <= 0 {
			return nil, fmt.Errorf(
				"config: %s: global.namespace_timeout must be positive, got %q",
				path, raw,
			)
		}
		c.Global.NamespaceTimeout = d
	}

	// namespace_retry defaults to 3 when unset; explicit 0 disables
	// retries; negative values are rejected.
	if c.Global.NamespaceRetryRaw == nil {
		c.Global.NamespaceRetry = 3
	} else {
		n := *c.Global.NamespaceRetryRaw
		if n < 0 {
			return nil, fmt.Errorf(
				"config: %s: global.namespace_retry must be >= 0, got %d",
				path, n,
			)
		}
		c.Global.NamespaceRetry = n
	}

	if raw := c.Global.L2BlockTimeRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: global.l2_block_time %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d <= 0 {
			return nil, fmt.Errorf(
				"config: %s: global.l2_block_time must be positive, got %q",
				path, raw,
			)
		}
		c.Global.L2BlockTime = d
	}

	if raw := c.State.Block.TimeoutRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: state.block.timeout %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d <= 0 {
			return nil, fmt.Errorf(
				"config: %s: state.block.timeout must be positive, got %q",
				path, raw,
			)
		}
		c.State.Block.Timeout = d
	}

	if raw := c.State.Block.IntervalRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: state.block.interval %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d <= 0 {
			return nil, fmt.Errorf(
				"config: %s: state.block.interval must be positive, got %q",
				path, raw,
			)
		}
		c.State.Block.Interval = d
	}

	if raw := c.State.TxPool.TimeoutRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: state.txpool.timeout %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d <= 0 {
			return nil, fmt.Errorf(
				"config: %s: state.txpool.timeout must be positive, got %q",
				path, raw,
			)
		}
		c.State.TxPool.Timeout = d
	}

	if raw := c.State.TxPool.IntervalRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: state.txpool.interval %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d <= 0 {
			return nil, fmt.Errorf(
				"config: %s: state.txpool.interval must be positive, got %q",
				path, raw,
			)
		}
		c.State.TxPool.Interval = d
	}

	// state_root is resolved relative to the *config file's* directory
	// when written as a relative path. This makes the path stable
	// regardless of the cwd op-ctl is invoked from — operators expect
	// their config to load the same way whether they run `./op-ctl`
	// from the project root, from another directory via absolute
	// path, or via a shell alias.
	//
	// Absolute paths are passed through verbatim. ~/... is not expanded
	// here; if operators need home-relative paths they should use the
	// shell's expansion before storing in config.
	if raw := strings.TrimSpace(c.Contracts.StateRoot); raw != "" && !filepath.IsAbs(raw) {
		c.Contracts.StateRoot = filepath.Join(filepath.Dir(path), raw)
	}

	// state.txpool.detail.refresh:
	//   unset (zero-value RawString) → 10s default
	//   "0s" / "0"                    → 0 (manual only)
	//   negative                      → load error
	//   >0 and <5s                    → clamped to 5s, one-line warning
	if raw := c.State.TxPool.Detail.RefreshRaw; raw == "" {
		c.State.TxPool.Detail.Refresh = 10 * time.Second
	} else {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"config: %s: state.txpool.detail.refresh %q is not a valid duration: %w",
				path, raw, err,
			)
		}
		if d < 0 {
			return nil, fmt.Errorf(
				"config: %s: state.txpool.detail.refresh must be >= 0, got %q",
				path, raw,
			)
		}
		if d > 0 && d < 5*time.Second {
			fmt.Fprintf(WarningWriter,
				"warning: state.txpool.detail.refresh %q is below the 5s minimum; clamped to 5s\n",
				raw,
			)
			d = 5 * time.Second
		}
		c.State.TxPool.Detail.Refresh = d
	}

	return &c, nil
}

// finalizeBastions injects map-key into Name, applies defaults
// (Port=22, KnownHosts=~/.ssh/known_hosts), parses KeepaliveInterval,
// and enforces required fields on inline [bastions.*] entries.
//
// Path fields (IdentityFile, KnownHosts) are validated for path-form
// (rejecting the unsupported "~user" syntax) but not expanded — the
// dialer expands at use-time so the raw config remains readable in
// debug output.
func (c *Config) finalizeBastions(path string) error {
	for name, b := range c.Bastions {
		if b.Host == "" {
			return fmt.Errorf(
				"config: %s: bastion %q missing required host",
				path, name,
			)
		}
		if b.User == "" {
			return fmt.Errorf(
				"config: %s: bastion %q missing required user",
				path, name,
			)
		}
		if b.Port == 0 {
			b.Port = 22
		}
		if b.KnownHosts == "" {
			b.KnownHosts = "~/.ssh/known_hosts"
		}
		if err := validatePathForm(b.IdentityFile); err != nil {
			return fmt.Errorf(
				"config: %s: bastion %q identity_file: %w",
				path, name, err,
			)
		}
		if err := validatePathForm(b.KnownHosts); err != nil {
			return fmt.Errorf(
				"config: %s: bastion %q known_hosts: %w",
				path, name, err,
			)
		}
		if raw := b.KeepaliveIntervalRaw; raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf(
					"config: %s: bastion %q keepalive_interval %q is not a valid duration: %w",
					path, name, raw, err,
				)
			}
			if d <= 0 {
				return fmt.Errorf(
					"config: %s: bastion %q keepalive_interval must be positive, got %q",
					path, name, raw,
				)
			}
			b.KeepaliveInterval = d
		}
		// Normalize OpenSSH-style comma-separated ProxyJump down to its
		// first hop. Symmetric with the ssh_config path so a TOML
		// `proxy_jump = "a,b"` and a `~/.ssh/config` `ProxyJump a,b`
		// both behave as `a`. The chain walker handles further hops by
		// following each alias's own proxy_jump in turn.
		if strings.TrimSpace(b.ProxyJump) != "" {
			b.ProxyJump = firstHop(b.ProxyJump)
		}
		b.Name = name
		c.Bastions[name] = b
	}
	return nil
}

// validateSSHJumps ensures every Backend.SSHJump points at a resolvable
// alias — either an inline [bastions.<alias>] table OR a Host block in
// ~/.ssh/config exposing a non-empty HostName. When BOTH are defined for
// the same alias, the inline entry wins and a one-line warning is written
// to WarningWriter (stderr by default).
//
// As of multi-hop support this also walks each alias's proxy_jump chain,
// checks that every link resolves, and rejects cycles. Catching cycles at
// Load() keeps the dialer's runtime path simple — the guard there is only
// defense-in-depth.
func (c *Config) validateSSHJumps(path string) error {
	for name, b := range c.Backends {
		alias := strings.TrimSpace(b.SSHJump)
		if alias == "" {
			continue
		}
		if err := c.checkAliasResolvable(path, fmt.Sprintf("backend %q ssh_jump", name), alias); err != nil {
			return err
		}
		if chain, err := c.walkProxyChain(alias); err != nil {
			return fmt.Errorf(
				"config: %s: backend %q ssh_jump %q: %w (chain so far: %v)",
				path, name, alias, err, chain,
			)
		}
	}
	// Also validate any inline bastion that has proxy_jump but is not (yet)
	// referenced from a backend — operators should still get a load-time
	// error rather than discovering a misconfiguration on first use.
	for name, ba := range c.Bastions {
		if strings.TrimSpace(ba.ProxyJump) == "" {
			continue
		}
		if chain, err := c.walkProxyChain(name); err != nil {
			return fmt.Errorf(
				"config: %s: bastion %q proxy_jump: %w (chain so far: %v)",
				path, name, err, chain,
			)
		}
	}
	return nil
}

// checkAliasResolvable returns nil iff alias is defined inline OR has a
// non-empty HostName in ~/.ssh/config. When both define the alias, emits
// the shadow-warning. context is a human-readable label for the error
// message (e.g. "backend \"seq-1\" ssh_jump").
func (c *Config) checkAliasResolvable(path, context, alias string) error {
	_, inlineOK := c.Bastions[alias]
	sshHost := SSHConfigGet(alias, "HostName")
	sshHasAlias := sshHost != ""

	switch {
	case inlineOK && sshHasAlias:
		fmt.Fprintf(WarningWriter,
			"warning: bastion alias %q is defined in both config.toml and ~/.ssh/config; using config.toml values\n",
			alias,
		)
		return nil
	case inlineOK, sshHasAlias:
		return nil
	default:
		return fmt.Errorf(
			"config: %s: %s references unknown alias %q (not in [bastions.*] and no matching Host in ~/.ssh/config)",
			path, context, alias,
		)
	}
}

// walkProxyChain follows alias.proxy_jump links and returns an error
// describing the chain when it cycles or hits an unknown alias. The
// returned slice is the path it walked (in order, for diagnostics).
//
// proxy_jump is read in this order:
//  1. inline [bastions.<alias>].ProxyJump
//  2. ~/.ssh/config's ProxyJump directive (via SSHConfigGet)
//
// An alias not present inline AND with no ssh_config ProxyJump is a
// terminal (chain end), not an error — the caller has already verified
// alias-resolvability via checkAliasResolvable for the starting point.
func (c *Config) walkProxyChain(start string) ([]string, error) {
	visited := map[string]bool{}
	chain := []string{start}
	visited[start] = true
	current := start
	for {
		next, err := c.proxyJumpOf(current)
		if err != nil {
			return chain, err
		}
		if next == "" {
			return chain, nil
		}
		// Verify the next link is itself resolvable.
		if _, inline := c.Bastions[next]; !inline {
			if SSHConfigGet(next, "HostName") == "" {
				return chain, fmt.Errorf(
					"proxy_jump %q is not defined inline or in ~/.ssh/config",
					next,
				)
			}
		}
		if visited[next] {
			return append(chain, next), fmt.Errorf("proxy_jump cycle detected")
		}
		visited[next] = true
		chain = append(chain, next)
		current = next
	}
}

// proxyJumpOf returns the proxy_jump alias for the given alias.
// Inline bastion entry wins; falls through to ~/.ssh/config's
// ProxyJump directive. Returns "" when neither defines a parent.
//
// OpenSSH allows ProxyJump to list multiple hosts comma-separated
// (`ProxyJump a,b,c` meaning hop through a then b then c). v1 of this
// implementation supports only the single-hop form — we trim whitespace
// and take the first comma-separated token so a multi-hop ProxyJump
// degrades to using just its first hop with a warning emitted by the
// dialer when expansion is attempted. The chain walker here only sees
// the first hop.
func (c *Config) proxyJumpOf(alias string) (string, error) {
	if ba, ok := c.Bastions[alias]; ok && strings.TrimSpace(ba.ProxyJump) != "" {
		return firstHop(ba.ProxyJump), nil
	}
	if pj := SSHConfigGet(alias, "ProxyJump"); strings.TrimSpace(pj) != "" {
		return firstHop(pj), nil
	}
	return "", nil
}

// firstHop returns the first comma-separated alias from an OpenSSH-style
// ProxyJump value, trimmed of whitespace.
func firstHop(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, ","); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// validatePathForm rejects path forms the dialer cannot safely expand.
// Currently this means "~user" (only bare "~" or "~/..." are supported).
// Empty paths are allowed — callers apply their own defaults.
func validatePathForm(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "~") && p != "~" && !strings.HasPrefix(p, "~/") {
		return fmt.Errorf(
			"path %q: ~user form is not supported (only bare ~ or ~/...)",
			p,
		)
	}
	return nil
}

// BackendList returns backends in config-file source order.
//
// This is intentionally NOT alphabetical: operators frequently group nodes
// in operationally meaningful order (sequencer, sync-1, sync-2, archive)
// and an alphabetical reshuffle would scramble the dashboard.
func (c *Config) BackendList() []Backend {
	out := make([]Backend, 0, len(c.keyOrder))
	for _, name := range c.keyOrder {
		out = append(out, c.Backends[name])
	}
	return out
}

// Path returns the source file path Load was called with (used for error
// messages further up the call stack).
func (c *Config) Path() string { return c.path }

// DiscoverConfigs returns every *.toml file directly inside dir, as
// absolute paths sorted lexicographically by base name. Hidden files
// (leading '.') and nested directories are skipped.
//
// A missing directory returns an empty slice with a nil error so the
// caller can branch on len() without an errors.Is dance — picker / CLI
// code surfaces a friendlier message than a raw os.PathError. Other
// I/O errors (permission denied, etc.) propagate wrapped.
func DiscoverConfigs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("discover configs in %s: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute path of %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if filepath.Ext(name) != ".toml" {
			continue
		}
		out = append(out, filepath.Join(abs, name))
	}
	sort.Slice(out, func(i, j int) bool {
		return filepath.Base(out[i]) < filepath.Base(out[j])
	})
	return out, nil
}
