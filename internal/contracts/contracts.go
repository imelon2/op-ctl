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
type Addresses struct {
	DisputeGameFactoryProxy string
	SystemConfigProxy       string
}

// stateFile mirrors the shape of state.json. Only opChainDeployments
// is decoded; the surrounding sections (appliedIntent, l1StateDump,
// implementationsDeployment, etc.) are ignored.
type stateFile struct {
	OpChainDeployments []opChainDeployment `json:"opChainDeployments"`
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
	}, nil
}
