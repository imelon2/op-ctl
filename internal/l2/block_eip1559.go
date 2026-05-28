package l2

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"op-ctl/internal/l1"
)

// EIP-1559 extraData layouts. An OP-Stack L2 block header encodes the
// live EIP-1559 parameters into its extraData; the version byte (index
// 0) selects the layout:
//
//	Holocene (version 0x00, 9 bytes):
//	  [0]     version     (0x00)
//	  [1:5]   denominator (uint32, big-endian)
//	  [5:9]   elasticity  (uint32, big-endian)
//
//	Jovian (version 0x01, 17 bytes) — adds minBaseFee:
//	  [0]     version     (0x01)
//	  [1:5]   denominator (uint32, big-endian)
//	  [5:9]   elasticity  (uint32, big-endian)
//	  [9:17]  minBaseFee  (uint64, big-endian)
const (
	HoloceneExtraDataLen     = 9
	HoloceneExtraDataVersion = 0x00
	JovianExtraDataLen       = 17
	JovianExtraDataVersion   = 0x01
)

// BlockEIP1559Snapshot holds the EIP-1559 parameters decoded from the
// latest L2 block's extraData. Err captures either the RPC failure or
// a decode/format mismatch; ExtraData is retained even on a decode
// failure so the operator can eyeball the raw bytes.
type BlockEIP1559Snapshot struct {
	BlockNumber *big.Int
	ExtraData   []byte

	Version     uint8
	Denominator uint32
	Elasticity  uint32
	MinBaseFee  uint64

	// HasMinBaseFee is true only for the Jovian layout (version 0x01);
	// Holocene (version 0x00) carries no minBaseFee, so callers should
	// skip rendering that field when this is false.
	HasMinBaseFee bool

	Latency time.Duration
	Err     error
}

// ForkName maps the decoded version byte to its activation fork. Only
// meaningful when Err is nil.
func (s *BlockEIP1559Snapshot) ForkName() string {
	switch s.Version {
	case HoloceneExtraDataVersion:
		return "Holocene"
	case JovianExtraDataVersion:
		return "Jovian"
	default:
		return "unknown"
	}
}

// FetchLatestBlockEIP1559 fetches the latest L2 block and decodes its
// extraData into the EIP-1559 parameters, dispatching on the version
// byte (Holocene 0x00 / Jovian 0x01). A one-shot read (the
// caller invokes it on entry / manual refresh, not on a tick). RPC and
// decode failures land in s.Err; the returned error is non-nil only
// for the empty-URL guard, mirroring the sibling fetchers.
func FetchLatestBlockEIP1559(ctx context.Context, hc *http.Client, l2RPCURL string) (*BlockEIP1559Snapshot, error) {
	s := &BlockEIP1559Snapshot{}
	if strings.TrimSpace(l2RPCURL) == "" {
		s.Err = fmt.Errorf("l2_rpc_url is empty (set [rpc].l2_rpc_url in config.toml)")
		return s, s.Err
	}
	hdr, lat, err := l1.EthGetBlockByNumber(ctx, hc, l2RPCURL, "latest")
	s.Latency = lat
	if err != nil {
		s.Err = err
		return s, nil
	}
	s.BlockNumber = hdr.Number
	s.ExtraData = hdr.ExtraData
	if err := s.decode(); err != nil {
		s.Err = err
	}
	return s, nil
}

// decode dispatches on the version byte: 0x00 → Holocene (9 bytes),
// 0x01 → Jovian (17 bytes, with minBaseFee). Returns a descriptive
// error (without populating the fields) when the version is unknown or
// the length doesn't match the layout that version mandates.
func (s *BlockEIP1559Snapshot) decode() error {
	d := s.ExtraData
	if len(d) == 0 {
		return fmt.Errorf("empty extraData (pre-Holocene block?)")
	}
	switch d[0] {
	case HoloceneExtraDataVersion:
		if len(d) != HoloceneExtraDataLen {
			return fmt.Errorf("Holocene extraData (version 0) must be %d bytes, got %d", HoloceneExtraDataLen, len(d))
		}
		s.Version = d[0]
		s.Denominator = binary.BigEndian.Uint32(d[1:5])
		s.Elasticity = binary.BigEndian.Uint32(d[5:9])
		return nil
	case JovianExtraDataVersion:
		if len(d) != JovianExtraDataLen {
			return fmt.Errorf("Jovian extraData (version 1) must be %d bytes, got %d", JovianExtraDataLen, len(d))
		}
		s.Version = d[0]
		s.Denominator = binary.BigEndian.Uint32(d[1:5])
		s.Elasticity = binary.BigEndian.Uint32(d[5:9])
		s.MinBaseFee = binary.BigEndian.Uint64(d[9:17])
		s.HasMinBaseFee = true
		return nil
	default:
		return fmt.Errorf("unknown extraData version byte 0x%02x (want 0x00 Holocene or 0x01 Jovian)", d[0])
	}
}
