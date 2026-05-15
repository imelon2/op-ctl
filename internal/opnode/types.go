package opnode

// PeerInfo is the response of op-node's opp2p_self JSON-RPC method.
//
// Field names mirror op-node's PeerInfo struct exactly (op-service/p2p).
type PeerInfo struct {
	PeerID          string   `json:"peerID"`
	NodeID          string   `json:"nodeID"`
	UserAgent       string   `json:"userAgent"`
	ProtocolVersion string   `json:"protocolVersion"`
	ENR             string   `json:"ENR"`
	Addresses       []string `json:"addresses"`
	Protocols       []string `json:"protocols"`
	Connectedness   any      `json:"connectedness"`
	Direction       any      `json:"direction"`
	Protected       bool     `json:"protected"`
	ChainID         uint64   `json:"chainID"`
	Latency         uint64   `json:"latency"`
	GossipBlocks    bool     `json:"gossipBlocks"`
}
