// Package contracts loads L1 contract addresses from a state.json
// emitted by op-deployer.
//
// op-ctl's [contracts].state_root TOML key points at this file; only
// fields op-ctl actively consumes are decoded here. Add new fields as
// future `op-ctl read` subcommands demand them — json.Unmarshal
// silently ignores unknown keys, so partial coverage is safe.
package contracts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Addresses is the subset of L1 contract addresses op-ctl reads from.
// Fields are returned in their on-disk form (hex with 0x prefix,
// mixed case as op-deployer wrote them) — call sites that need
// canonical lower-case can normalize at use time.
//
// Per-field requiredness is enforced at the *consumer*, not at load:
// only DisputeGameFactoryProxy is required up front today (legacy of
// the `read dispute-game` command shipping first). Other fields default
// to empty when absent and the subcommand that needs them surfaces the
// missing-address error — that way adding a new field doesn't break
// operator configs that pre-date it.
//
// L2BlockTime is sourced from appliedIntent.globalDeployOverrides.l2BlockTime
// (an integer count of seconds in state.json) and converted to a Duration.
// Zero when the override is absent; consumers apply their own fallback.
type Addresses struct {
	DisputeGameFactoryProxy string
	SystemConfigProxy       string
	L2BlockTime             time.Duration
}

// stateFile mirrors the shape of state.json. Only the fields op-ctl
// actively consumes are decoded; the surrounding sections (l1StateDump,
// implementationsDeployment, etc.) are ignored.
type stateFile struct {
	AppliedIntent      appliedIntentSection `json:"appliedIntent"`
	OpChainDeployments []opChainDeployment  `json:"opChainDeployments"`
}

// appliedIntentSection holds the parts of state.json's appliedIntent
// op-ctl reads. globalDeployOverrides currently sources the chain's
// L2 block time; future overrides can land here without rippling.
type appliedIntentSection struct {
	GlobalDeployOverrides *globalDeployOverridesSection `json:"globalDeployOverrides"`
}

// globalDeployOverridesSection mirrors appliedIntent.globalDeployOverrides.
// L2BlockTime is a pointer so we can distinguish "absent" from "explicitly 0";
// op-deployer writes the override as an integer count of seconds.
type globalDeployOverridesSection struct {
	L2BlockTime *uint64 `json:"l2BlockTime"`
}

type opChainDeployment struct {
	ID                      string `json:"id"`
	DisputeGameFactoryProxy string `json:"DisputeGameFactoryProxy"`
	SystemConfigProxy       string `json:"SystemConfigProxy"`
}

// Load reads state.json from path and returns the L1 contract address
// set for the active op-chain deployment. v1 op-deployer outputs hold
// exactly one entry in opChainDeployments — we treat index 0 as the
// active chain. When that assumption breaks (multi-chain deployments)
// the selector will need to grow a chain-id argument.
func Load(path string) (*Addresses, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("contracts: state_root path is empty (set [contracts].state_root in config.toml)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("contracts: read %s: %w", path, err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, fmt.Errorf("contracts: decode %s: %w", path, err)
	}
	if len(sf.OpChainDeployments) == 0 {
		return nil, fmt.Errorf("contracts: %s: no opChainDeployments entries", path)
	}
	chain := sf.OpChainDeployments[0]
	if strings.TrimSpace(chain.DisputeGameFactoryProxy) == "" {
		return nil, fmt.Errorf("contracts: %s: opChainDeployments[0].DisputeGameFactoryProxy is empty", path)
	}
	return &Addresses{
		DisputeGameFactoryProxy: chain.DisputeGameFactoryProxy,
		SystemConfigProxy:       chain.SystemConfigProxy,
		L2BlockTime:             extractL2BlockTime(sf),
	}, nil
}

// LoadL2BlockTime returns appliedIntent.globalDeployOverrides.l2BlockTime
// from state.json as a Duration. Returns 0 when the file is missing,
// unreadable, malformed, or the override is absent — callers apply their
// own fallback.
//
// This is intentionally separate from Load so the status-block command
// can read the L2 block cadence without depending on DisputeGameFactoryProxy
// being populated (Load requires it; LoadL2BlockTime does not).
func LoadL2BlockTime(path string) time.Duration {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return 0
	}
	return extractL2BlockTime(sf)
}

func extractL2BlockTime(sf stateFile) time.Duration {
	if sf.AppliedIntent.GlobalDeployOverrides == nil ||
		sf.AppliedIntent.GlobalDeployOverrides.L2BlockTime == nil {
		return 0
	}
	return time.Duration(*sf.AppliedIntent.GlobalDeployOverrides.L2BlockTime) * time.Second
}

// LoadL2ChainID returns opChainDeployments[0].id from state.json — the
// hex L2 chain identifier op-deployer writes when the chain is
// provisioned. The value is returned exactly as written (no case
// folding): EIP-55 / on-disk case is meaningful to operators who use
// the chain id as a directory name (e.g. config/{l2-chainid}/batcher.db
// for `op-ctl read batch`).
//
// Like LoadL2BlockTime, this is intentionally separate from Load so
// `op-ctl read batch` can resolve its cache directory without
// depending on DisputeGameFactoryProxy being populated.
func LoadL2ChainID(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("contracts: state_root path is empty (set [contracts].state_root in config.toml)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("contracts: read %s: %w", path, err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return "", fmt.Errorf("contracts: decode %s: %w", path, err)
	}
	if len(sf.OpChainDeployments) == 0 {
		return "", fmt.Errorf("contracts: %s: no opChainDeployments entries", path)
	}
	id := strings.TrimSpace(sf.OpChainDeployments[0].ID)
	if id == "" {
		return "", fmt.Errorf("contracts: %s: opChainDeployments[0].id is empty", path)
	}
	return id, nil
}
