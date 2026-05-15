// Package namespace persists per-backend node identity (peerID, nodeID,
// ENR, enode) as JSON files inside a directory.
//
// The files exist so future commands that enumerate connected peers can
// translate opaque IDs back into the operator-friendly backend names from
// config.toml. Any field that could not be retrieved from the node is
// written as an empty string so the operator can fill it in by hand.
package namespace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is the on-disk shape of one backend's identity record.
//
// Consensus and Execution are kept separate because op-stack nodes run
// two distinct discovery stacks (op-node libp2p + opdiscv5 on the
// consensus side, devp2p discv5 on the execution side) and the IDs
// don't overlap.
type Entry struct {
	Name      string    `json:"name"`
	Consensus Consensus `json:"consensus"`
	Execution Execution `json:"execution"`
}

// Consensus holds identifiers from op-node's opp2p_self.
type Consensus struct {
	PeerID string `json:"peer_id"`
	NodeID string `json:"node_id"`
	ENR    string `json:"enr"`
}

// Execution holds identifiers from the EL admin_nodeInfo.
type Execution struct {
	NodeID string `json:"node_id"`
	Enode  string `json:"enode"`
	ENR    string `json:"enr"`
}

// Write serializes e to <dir>/<e.Name>.json, creating dir if needed.
//
// Writes are atomic: data lands in <path>.tmp first and is renamed into
// place so a crash mid-write can never leave a half-written namespace
// file behind.
func Write(dir string, e Entry) (string, error) {
	if e.Name == "" {
		return "", fmt.Errorf("namespace: entry name is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("namespace: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, e.Name+".json")
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", fmt.Errorf("namespace: marshal %s: %w", e.Name, err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fmt.Errorf("namespace: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("namespace: rename %s -> %s: %w", tmp, path, err)
	}
	return path, nil
}

// LoadAll reads every *.json file in dir as an Entry and returns them
// sorted by Name. A missing dir returns (nil, nil) so callers can show
// "(no entries)" without distinguishing not-yet-populated from empty.
// Hidden files and *.tmp staging files are skipped.
func LoadAll(dir string) ([]Entry, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("namespace: readdir %s: %w", dir, err)
	}
	var out []Entry
	for _, de := range ents {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("namespace: read %s: %w", path, err)
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("namespace: decode %s: %w", path, err)
		}
		if e.Name == "" {
			e.Name = strings.TrimSuffix(name, ".json")
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Index is a reverse-lookup of identifier -> backend name built from a
// directory of namespace files. A single Entry contributes up to six
// keys (consensus peer_id / node_id / enr, execution node_id / enode /
// enr); empty strings are skipped. First Entry to register a key wins
// on collision so a later hand-edited file can't silently overwrite an
// auto-populated one.
type Index struct {
	byID map[string]string
}

// LoadIndex reads dir via LoadAll and builds an Index. A missing dir
// returns an empty (non-nil) Index so callers can Lookup unconditionally.
func LoadIndex(dir string) (*Index, error) {
	entries, err := LoadAll(dir)
	if err != nil {
		return nil, err
	}
	return BuildIndex(entries), nil
}

// BuildIndex constructs an Index from an in-memory entry slice. Useful
// for tests that don't want to round-trip through the filesystem.
func BuildIndex(entries []Entry) *Index {
	idx := &Index{byID: make(map[string]string, len(entries)*6)}
	for _, e := range entries {
		for _, id := range []string{
			e.Consensus.PeerID, e.Consensus.NodeID, e.Consensus.ENR,
			e.Execution.NodeID, e.Execution.Enode, e.Execution.ENR,
		} {
			if id == "" {
				continue
			}
			if _, exists := idx.byID[id]; !exists {
				idx.byID[id] = e.Name
			}
		}
	}
	return idx
}

// Lookup returns the backend name for an identifier or "" if unknown.
// Safe to call on a nil receiver.
func (i *Index) Lookup(id string) string {
	if i == nil {
		return ""
	}
	return i.byID[id]
}
