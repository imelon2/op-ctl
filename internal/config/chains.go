package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ChainEntry is one chain selectable from the operator's config.yaml,
// pairing the human-friendly chain name (the YAML map key) with the
// absolute path of the per-chain TOML config it points to.
type ChainEntry struct {
	Name       string
	ConfigPath string
}

// chainSpec is the YAML shape of a single chain entry. Kept as its own
// struct so additional fields (rpc overrides, descriptions, ...) can be
// added later without touching every call site.
type chainSpec struct {
	Config string `yaml:"config"`
}

// DiscoverChains parses the operator's config.yaml — a map of
// chain-name → { config: <toml path> } — and returns the entries sorted
// by name. The `config` value is resolved relative to the YAML file's
// directory, so `./config/foo.toml` under /…/op-ctl/config.yaml lands at
// /…/op-ctl/config/foo.toml regardless of which cwd op-ctl was launched
// from.
//
// A missing YAML file returns (nil, nil) so the caller can branch on
// len() without an errors.Is dance — picker / CLI code surfaces a
// friendlier message than a raw os.PathError. Other I/O and parse
// errors propagate wrapped.
func DiscoverChains(yamlPath string) ([]ChainEntry, error) {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", yamlPath, err)
	}
	var raw map[string]chainSpec
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", yamlPath, err)
	}
	baseDir, err := filepath.Abs(filepath.Dir(yamlPath))
	if err != nil {
		return nil, fmt.Errorf("resolve %s parent: %w", yamlPath, err)
	}
	out := make([]ChainEntry, 0, len(raw))
	for name, spec := range raw {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s: empty chain name", yamlPath)
		}
		if strings.TrimSpace(spec.Config) == "" {
			return nil, fmt.Errorf("%s: chain %q has empty 'config' field", yamlPath, name)
		}
		p := spec.Config
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		out = append(out, ChainEntry{Name: name, ConfigPath: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
