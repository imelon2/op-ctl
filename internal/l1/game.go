package l1

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// GameStatus mirrors Solidity's GameStatus enum:
//
//	0 = IN_PROGRESS
//	1 = CHALLENGER_WINS
//	2 = DEFENDER_WINS
type GameStatus uint8

const (
	GameStatusInProgress      GameStatus = 0
	GameStatusChallengerWins  GameStatus = 1
	GameStatusDefenderWins    GameStatus = 2
)

// String returns the canonical Solidity enum name (or UNKNOWN(n) when
// the on-chain value drifts from the known set).
func (s GameStatus) String() string {
	switch s {
	case GameStatusInProgress:
		return "IN_PROGRESS"
	case GameStatusChallengerWins:
		return "CHALLENGER_WINS"
	case GameStatusDefenderWins:
		return "DEFENDER_WINS"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint8(s))
	}
}

// GameSnapshot is the union of FaultDisputeGame + PermissionedDisputeGame
// view-method results op-ctl renders on the detail screen. Fields are
// grouped by relation to match the 8-section render order, but the on-
// wire fetch is one batched JSON-RPC POST.
//
// Per-field errors: a permissionless game's proposer()/challenger()
// will revert. Rather than failing the whole snapshot we record the
// failure in Errors[fieldName] and leave the field at its zero value
// — the UI then renders "ERR <message>" instead of the value.
type GameSnapshot struct {
	Address string

	// 1. Identity
	Version     string
	GameType    uint32
	L2ChainID   *big.Int
	GameCreator string

	// 2. Status & Timing
	Status     GameStatus
	CreatedAt  uint64 // unix seconds
	ResolvedAt uint64

	// 3. Roles
	Proposer                string
	Challenger              string
	L2BlockNumberChallenged bool
	L2BlockNumberChallenger string

	// 4. Output Root Claim
	RootClaim     string // 0x...bytes32
	L1Head        string // 0x...bytes32
	ExtraData     string // 0x...hex of bytes payload
	L2BlockNumber *big.Int // decoded l2BlockNumber() — same value extraData encodes
	ClaimDataLen  *big.Int

	// 5. Anchor
	AnchorStateRegistry string
	StartingBlockNumber *big.Int
	StartingRootHash    string

	// 6. Execution VM
	AbsolutePrestate string // 0x...bytes32
	VM               string // MIPS64 instance address

	// 7. Bond Vault
	WETH string // DelayedWETH address

	// 8. Game Parameters
	MaxGameDepth     *big.Int
	SplitDepth       *big.Int
	MaxClockDuration uint64 // Solidity Duration is uint64 seconds
	ClockExtension   uint64

	// Errors maps field name → call/decode error for any call that
	// failed. Absent key means the field is populated.
	Errors map[string]error

	// Latency is the wall-clock duration of the batched RPC POST.
	Latency time.Duration
}

// snapshotCall pairs a field name with its eth_call data and a
// decoder. The decoder writes into the snapshot or returns an error
// recorded into Errors. Splitting the call set into this table makes
// it easy to add new fields without touching FetchGameSnapshot's
// control flow.
type snapshotCall struct {
	field   string
	data    string
	decode  func(raw string, s *GameSnapshot) error
}

// FetchGameSnapshot issues a single batched JSON-RPC eth_call request
// containing every selector listed in snapshotCalls, then decodes
// each result into s. Per-call failures populate s.Errors but never
// abort the snapshot — a partial picture is more useful than nothing
// when one call reverts.
//
// A transport-level error (build req, HTTP non-2xx, malformed
// envelope) is returned as the function's err return; the partial
// snapshot is also returned so callers can still display Address and
// the breadcrumb header.
func FetchGameSnapshot(ctx context.Context, hc *http.Client, l1RPCURL, gameAddr string) (*GameSnapshot, error) {
	s := &GameSnapshot{
		Address: gameAddr,
		Errors:  map[string]error{},
	}
	calls := snapshotCalls()
	reqs := make([]EthCallReq, len(calls))
	for i, c := range calls {
		reqs[i] = EthCallReq{To: gameAddr, Data: c.data}
	}
	results, latency, err := EthCallBatch(ctx, hc, l1RPCURL, reqs)
	s.Latency = latency
	if err != nil {
		return s, err
	}
	for i, c := range calls {
		r := results[i]
		if r.Err != nil {
			s.Errors[c.field] = r.Err
			continue
		}
		if derr := c.decode(r.Result, s); derr != nil {
			s.Errors[c.field] = derr
		}
	}
	return s, nil
}

// snapshotCalls returns the static list of (field, data, decoder)
// entries fetched on every detail-screen open. Keeping it in one
// place makes the selector ↔ field mapping trivial to audit.
func snapshotCalls() []snapshotCall {
	// Decoder helpers — all share the same shape: parse hex, slice one
	// word (or use a per-type helper), assign into s.
	decodeAddrInto := func(field string, set func(*GameSnapshot, string)) func(string, *GameSnapshot) error {
		return func(raw string, s *GameSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToAddress(w))
			return nil
		}
	}
	decodeBytes32Into := func(field string, set func(*GameSnapshot, string)) func(string, *GameSnapshot) error {
		return func(raw string, s *GameSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToBytes32(w))
			return nil
		}
	}
	decodeUint256Into := func(field string, set func(*GameSnapshot, *big.Int)) func(string, *GameSnapshot) error {
		return func(raw string, s *GameSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToUint256(w))
			return nil
		}
	}
	decodeUint64Into := func(field string, set func(*GameSnapshot, uint64)) func(string, *GameSnapshot) error {
		return func(raw string, s *GameSnapshot) error {
			buf, err := decodeHexData(raw)
			if err != nil {
				return err
			}
			if len(buf) < 32 {
				return fmt.Errorf("%s: result too short (%d)", field, len(buf))
			}
			w, _ := wordAt(buf, 0)
			set(s, wordToUint64(w))
			return nil
		}
	}

	return []snapshotCall{
		// --- 1. Identity ---
		{
			field: "version",
			data:  selectorOf("version()"),
			decode: func(raw string, s *GameSnapshot) error {
				buf, err := decodeHexData(raw)
				if err != nil {
					return err
				}
				if len(buf) < 32 {
					return fmt.Errorf("version: short result")
				}
				w0, _ := wordAt(buf, 0)
				off, err := readDynamicOffset(w0)
				if err != nil {
					return err
				}
				v, err := decodeStringAt(buf, off)
				if err != nil {
					return err
				}
				s.Version = v
				return nil
			},
		},
		{
			field:  "l2ChainId",
			data:   selectorOf("l2ChainId()"),
			decode: decodeUint256Into("l2ChainId", func(s *GameSnapshot, n *big.Int) { s.L2ChainID = n }),
		},
		{
			field:  "gameCreator",
			data:   selectorOf("gameCreator()"),
			decode: decodeAddrInto("gameCreator", func(s *GameSnapshot, a string) { s.GameCreator = a }),
		},

		// --- 2. Status & Timing ---
		{
			field: "status",
			data:  selectorOf("status()"),
			decode: func(raw string, s *GameSnapshot) error {
				buf, err := decodeHexData(raw)
				if err != nil {
					return err
				}
				if len(buf) < 32 {
					return fmt.Errorf("status: short result")
				}
				w, _ := wordAt(buf, 0)
				s.Status = GameStatus(wordToUint8(w))
				return nil
			},
		},
		{
			field:  "createdAt",
			data:   selectorOf("createdAt()"),
			decode: decodeUint64Into("createdAt", func(s *GameSnapshot, n uint64) { s.CreatedAt = n }),
		},
		{
			field:  "resolvedAt",
			data:   selectorOf("resolvedAt()"),
			decode: decodeUint64Into("resolvedAt", func(s *GameSnapshot, n uint64) { s.ResolvedAt = n }),
		},

		// --- 3. Roles (proposer/challenger may revert on Permissionless games) ---
		{
			field:  "proposer",
			data:   selectorOf("proposer()"),
			decode: decodeAddrInto("proposer", func(s *GameSnapshot, a string) { s.Proposer = a }),
		},
		{
			field:  "challenger",
			data:   selectorOf("challenger()"),
			decode: decodeAddrInto("challenger", func(s *GameSnapshot, a string) { s.Challenger = a }),
		},
		{
			field: "l2BlockNumberChallenged",
			data:  selectorOf("l2BlockNumberChallenged()"),
			decode: func(raw string, s *GameSnapshot) error {
				buf, err := decodeHexData(raw)
				if err != nil {
					return err
				}
				if len(buf) < 32 {
					return fmt.Errorf("l2BlockNumberChallenged: short result")
				}
				w, _ := wordAt(buf, 0)
				s.L2BlockNumberChallenged = wordToBool(w)
				return nil
			},
		},
		{
			field:  "l2BlockNumberChallenger",
			data:   selectorOf("l2BlockNumberChallenger()"),
			decode: decodeAddrInto("l2BlockNumberChallenger", func(s *GameSnapshot, a string) { s.L2BlockNumberChallenger = a }),
		},

		// --- 4. Output Root Claim ---
		// gameData() returns (uint32 gameType_, bytes32 rootClaim_, bytes extraData_).
		// Head: [gameType][rootClaim][offsetToExtraData]; tail at offset: [len][padded bytes].
		{
			field: "gameData",
			data:  selectorOf("gameData()"),
			decode: func(raw string, s *GameSnapshot) error {
				buf, err := decodeHexData(raw)
				if err != nil {
					return err
				}
				if len(buf) < 96 {
					return fmt.Errorf("gameData: short result (%d)", len(buf))
				}
				w0, _ := wordAt(buf, 0)
				w1, _ := wordAt(buf, 1)
				w2, _ := wordAt(buf, 2)
				s.GameType = wordToUint32(w0)
				s.RootClaim = wordToBytes32(w1)
				off, err := readDynamicOffset(w2)
				if err != nil {
					return err
				}
				b, err := decodeBytesAt(buf, off)
				if err != nil {
					return err
				}
				s.ExtraData = "0x" + hex.EncodeToString(b)
				return nil
			},
		},
		{
			field:  "l1Head",
			data:   selectorOf("l1Head()"),
			decode: decodeBytes32Into("l1Head", func(s *GameSnapshot, h string) { s.L1Head = h }),
		},
		{
			// l2BlockNumber() is the decoded form of extraData (the
			// game contract unpacks the first 32 bytes of extraData
			// into a uint256). Surfacing both lets the operator see
			// the raw payload AND the human-readable block number.
			field:  "l2BlockNumber",
			data:   selectorOf("l2BlockNumber()"),
			decode: decodeUint256Into("l2BlockNumber", func(s *GameSnapshot, n *big.Int) { s.L2BlockNumber = n }),
		},
		{
			field:  "claimDataLen",
			data:   selectorOf("claimDataLen()"),
			decode: decodeUint256Into("claimDataLen", func(s *GameSnapshot, n *big.Int) { s.ClaimDataLen = n }),
		},

		// --- 5. Anchor ---
		{
			field:  "anchorStateRegistry",
			data:   selectorOf("anchorStateRegistry()"),
			decode: decodeAddrInto("anchorStateRegistry", func(s *GameSnapshot, a string) { s.AnchorStateRegistry = a }),
		},
		{
			field:  "startingBlockNumber",
			data:   selectorOf("startingBlockNumber()"),
			decode: decodeUint256Into("startingBlockNumber", func(s *GameSnapshot, n *big.Int) { s.StartingBlockNumber = n }),
		},
		{
			field:  "startingRootHash",
			data:   selectorOf("startingRootHash()"),
			decode: decodeBytes32Into("startingRootHash", func(s *GameSnapshot, h string) { s.StartingRootHash = h }),
		},

		// --- 6. Execution VM ---
		{
			field:  "absolutePrestate",
			data:   selectorOf("absolutePrestate()"),
			decode: decodeBytes32Into("absolutePrestate", func(s *GameSnapshot, h string) { s.AbsolutePrestate = h }),
		},
		{
			field:  "vm",
			data:   selectorOf("vm()"),
			decode: decodeAddrInto("vm", func(s *GameSnapshot, a string) { s.VM = a }),
		},

		// --- 7. Bond Vault ---
		{
			field:  "weth",
			data:   selectorOf("weth()"),
			decode: decodeAddrInto("weth", func(s *GameSnapshot, a string) { s.WETH = a }),
		},

		// --- 8. Game Parameters ---
		{
			field:  "maxGameDepth",
			data:   selectorOf("maxGameDepth()"),
			decode: decodeUint256Into("maxGameDepth", func(s *GameSnapshot, n *big.Int) { s.MaxGameDepth = n }),
		},
		{
			field:  "splitDepth",
			data:   selectorOf("splitDepth()"),
			decode: decodeUint256Into("splitDepth", func(s *GameSnapshot, n *big.Int) { s.SplitDepth = n }),
		},
		{
			field:  "maxClockDuration",
			data:   selectorOf("maxClockDuration()"),
			decode: decodeUint64Into("maxClockDuration", func(s *GameSnapshot, n uint64) { s.MaxClockDuration = n }),
		},
		{
			field:  "clockExtension",
			data:   selectorOf("clockExtension()"),
			decode: decodeUint64Into("clockExtension", func(s *GameSnapshot, n uint64) { s.ClockExtension = n }),
		},
	}
}

