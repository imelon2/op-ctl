package elnode

import "encoding/json"

// NodeInfo is the response of the execution-layer admin_nodeInfo JSON-RPC
// method (op-geth / op-erigon / geth).
//
// Field names mirror go-ethereum's p2p.NodeInfo struct exactly so a raw
// JSON dump round-trips without surprises. Protocols is intentionally
// json.RawMessage rather than map[string]any: per-protocol payloads vary
// across forks (eth, snap, opp2p, ...) and a typed decode would force us
// to chase upstream schema drift. The expanded view re-marshals the whole
// struct, so RawMessage preserves the original shape.
type NodeInfo struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Enode      string          `json:"enode"`
	ENR        string          `json:"enr"`
	IP         string          `json:"ip"`
	Ports      NodePorts       `json:"ports"`
	ListenAddr string          `json:"listenAddr"`
	Protocols  json.RawMessage `json:"protocols"`
}

// NodePorts captures the two listening ports admin_nodeInfo reports.
// Discovery is UDP (discv5/discv4); Listener is TCP (RLPx).
type NodePorts struct {
	Discovery int `json:"discovery"`
	Listener  int `json:"listener"`
}
