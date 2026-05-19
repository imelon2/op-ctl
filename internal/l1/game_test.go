package l1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// snapshotServer routes by selector — when the request body is a
// batch, each request's `data` prefix tells us which selector is
// being called and we pick a canned response from `bySelector`.
type snapshotServer struct {
	t            *testing.T
	bySelector   map[string]string // selector → "0x..." result OR ":revert" sentinel
	defaultEmpty bool
}

func (s *snapshotServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		body = []byte(strings.TrimSpace(string(body)))
		var reqs []struct {
			ID     int `json:"id"`
			Params []any `json:"params"`
		}
		// Wrap single object in array for uniform handling.
		if len(body) > 0 && body[0] == '{' {
			body = append(append([]byte("["), body...), ']')
		}
		if err := json.Unmarshal(body, &reqs); err != nil {
			s.t.Fatalf("decode req: %v body=%s", err, string(body))
		}
		out := "["
		for i, rq := range reqs {
			if i > 0 {
				out += ","
			}
			callMap, _ := rq.Params[0].(map[string]any)
			data, _ := callMap["data"].(string)
			selector := data
			if len(data) >= 10 {
				selector = data[:10] // 0x + 8 chars
			}
			resp, ok := s.bySelector[selector]
			switch {
			case !ok:
				if s.defaultEmpty {
					out += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"0x"}`, rq.ID)
				} else {
					out += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"no canned response for %s"}}`, rq.ID, selector)
				}
			case resp == ":revert":
				out += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"execution reverted"}}`, rq.ID)
			default:
				out += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"%s"}`, rq.ID, resp)
			}
		}
		out += "]"
		_, _ = w.Write([]byte(out))
	}
}

// helpers for crafting return data
func word(hex string) string {
	if strings.HasPrefix(hex, "0x") {
		hex = hex[2:]
	}
	if len(hex) > 64 {
		panic("word: too long")
	}
	return strings.Repeat("0", 64-len(hex)) + hex
}

func TestFetchGameSnapshot_HappyPath(t *testing.T) {
	// Build a set of canned responses that decode into known values.
	versionResult := "0x" +
		word("20") + // offset
		word("5") + // length
		"312e342e30000000000000000000000000000000000000000000000000000000" // "1.4.0"
	addrResult := func(addr string) string {
		// 20-byte address right-padded into a 32-byte word
		clean := strings.TrimPrefix(addr, "0x")
		return "0x" + strings.Repeat("0", 24) + clean
	}
	gameDataResult := "0x" +
		word("1") + // gameType = 1
		word(strings.Repeat("a", 64)) + // rootClaim = 0xaa...aa
		word("60") + // offset to extraData = 96
		word("20") + // length = 32
		word("8badf00d") // extraData
	bs := &snapshotServer{
		t: t,
		bySelector: map[string]string{
			selectorOf("version()"):                 versionResult,
			selectorOf("l2ChainId()"):               "0x" + word("a5e8"), // 42472
			selectorOf("gameCreator()"):             addrResult("0x1111111111111111111111111111111111111111"),
			selectorOf("status()"):                  "0x" + word("1"), // CHALLENGER_WINS
			selectorOf("createdAt()"):               "0x" + word("67218000"),
			selectorOf("resolvedAt()"):              "0x" + word("67220000"),
			selectorOf("proposer()"):                addrResult("0xa1e46bf02bc5f9d051d606cfcf2deb222ea85afd"),
			selectorOf("challenger()"):              addrResult("0x03674fcda9e016a78010dc9bb6573e83a8b69eef"),
			selectorOf("l2BlockNumberChallenged()"): "0x" + word("0"),
			selectorOf("l2BlockNumberChallenger()"): addrResult("0x0000000000000000000000000000000000000000"),
			selectorOf("gameData()"):                gameDataResult,
			selectorOf("l1Head()"):                  "0x" + word(strings.Repeat("c", 64)),
			selectorOf("l2BlockNumber()"):           "0x" + word("e5849"), // 940617
			selectorOf("claimDataLen()"):            "0x" + word("3"),
			selectorOf("anchorStateRegistry()"):     addrResult("0x025d6d8a2ed2fa6f88f902065772c95850875a02"),
			selectorOf("startingBlockNumber()"):     "0x" + word("64"),
			selectorOf("startingRootHash()"):        "0x" + word(strings.Repeat("d", 64)),
			selectorOf("absolutePrestate()"):        "0x" + word(strings.Repeat("e", 64)),
			selectorOf("vm()"):                      addrResult("0x6ddba09bc4ccb0d6ca9fc5350580f74165707499"),
			selectorOf("weth()"):                    addrResult("0xea0f90b800c2113d680315be57000ba6d2c00b45"),
			selectorOf("maxGameDepth()"):            "0x" + word("49"), // 73
			selectorOf("splitDepth()"):              "0x" + word("1e"), // 30
			selectorOf("maxClockDuration()"):        "0x" + word("49d40"), // 302400
			selectorOf("clockExtension()"):          "0x" + word("2a30"), // 10800
		},
	}
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	snap, err := FetchGameSnapshot(context.Background(), srv.Client(), srv.URL, "0xgame")
	if err != nil {
		t.Fatalf("FetchGameSnapshot: %v", err)
	}
	if len(snap.Errors) != 0 {
		t.Fatalf("expected no errors, got %d: %v", len(snap.Errors), snap.Errors)
	}
	if snap.Version != "1.4.0" {
		t.Errorf("Version: %q", snap.Version)
	}
	if snap.Status != GameStatusChallengerWins {
		t.Errorf("Status: got %s, want CHALLENGER_WINS", snap.Status)
	}
	if snap.CreatedAt != 0x67218000 {
		t.Errorf("CreatedAt: %d", snap.CreatedAt)
	}
	if snap.GameType != 1 {
		t.Errorf("GameType: %d", snap.GameType)
	}
	if snap.RootClaim != "0x"+strings.Repeat("aa", 32) {
		t.Errorf("RootClaim: %s", snap.RootClaim)
	}
	// extraData is 32 bytes — leading word with the lower 4 bytes = 0x8badf00d.
	if !strings.Contains(snap.ExtraData, "8badf00d") {
		t.Errorf("ExtraData missing payload: %s", snap.ExtraData)
	}
	if snap.MaxGameDepth.Int64() != 73 {
		t.Errorf("MaxGameDepth: %v", snap.MaxGameDepth)
	}
	if snap.MaxClockDuration != 302400 {
		t.Errorf("MaxClockDuration: %d", snap.MaxClockDuration)
	}
	if snap.Latency <= 0 {
		t.Errorf("Latency: expected > 0, got %v", snap.Latency)
	}
}

func TestFetchGameSnapshot_PartialFailure(t *testing.T) {
	// Permissionless game: proposer() and challenger() revert. Other
	// fields succeed. Snapshot should report errors only for those two.
	versionResult := "0x" +
		word("20") + word("5") + "312e342e30000000000000000000000000000000000000000000000000000000"
	addrResult := func(addr string) string {
		clean := strings.TrimPrefix(addr, "0x")
		return "0x" + strings.Repeat("0", 24) + clean
	}
	gameDataResult := "0x" + word("0") + word(strings.Repeat("a", 64)) + word("60") + word("20") + word("0")
	bs := &snapshotServer{
		t: t,
		bySelector: map[string]string{
			selectorOf("version()"):                 versionResult,
			selectorOf("l2ChainId()"):               "0x" + word("a5e8"),
			selectorOf("gameCreator()"):             addrResult("0x1111111111111111111111111111111111111111"),
			selectorOf("status()"):                  "0x" + word("0"),
			selectorOf("createdAt()"):               "0x" + word("67218000"),
			selectorOf("resolvedAt()"):              "0x" + word("0"),
			selectorOf("proposer()"):                ":revert",
			selectorOf("challenger()"):              ":revert",
			selectorOf("l2BlockNumberChallenged()"): "0x" + word("0"),
			selectorOf("l2BlockNumberChallenger()"): addrResult("0x0000000000000000000000000000000000000000"),
			selectorOf("gameData()"):                gameDataResult,
			selectorOf("l1Head()"):                  "0x" + word(strings.Repeat("c", 64)),
			selectorOf("l2BlockNumber()"):           "0x" + word("0"),
			selectorOf("claimDataLen()"):            "0x" + word("0"),
			selectorOf("anchorStateRegistry()"):     addrResult("0x025d6d8a2ed2fa6f88f902065772c95850875a02"),
			selectorOf("startingBlockNumber()"):     "0x" + word("0"),
			selectorOf("startingRootHash()"):        "0x" + word(strings.Repeat("0", 64)),
			selectorOf("absolutePrestate()"):        "0x" + word(strings.Repeat("e", 64)),
			selectorOf("vm()"):                      addrResult("0x6ddba09bc4ccb0d6ca9fc5350580f74165707499"),
			selectorOf("weth()"):                    addrResult("0xea0f90b800c2113d680315be57000ba6d2c00b45"),
			selectorOf("maxGameDepth()"):            "0x" + word("49"),
			selectorOf("splitDepth()"):              "0x" + word("1e"),
			selectorOf("maxClockDuration()"):        "0x" + word("49d40"),
			selectorOf("clockExtension()"):          "0x" + word("2a30"),
		},
	}
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	snap, err := FetchGameSnapshot(context.Background(), srv.Client(), srv.URL, "0xgame")
	if err != nil {
		t.Fatalf("FetchGameSnapshot: %v", err)
	}
	if len(snap.Errors) != 2 {
		t.Fatalf("expected 2 errors (proposer, challenger), got %d: %v", len(snap.Errors), snap.Errors)
	}
	if snap.Errors["proposer"] == nil || !strings.Contains(snap.Errors["proposer"].Error(), "execution reverted") {
		t.Errorf("proposer error: %v", snap.Errors["proposer"])
	}
	if snap.Errors["challenger"] == nil {
		t.Errorf("challenger error missing")
	}
	// Other fields should still populate.
	if snap.Version != "1.4.0" {
		t.Errorf("Version (should still decode): %q", snap.Version)
	}
	if snap.Status != GameStatusInProgress {
		t.Errorf("Status: %s", snap.Status)
	}
}

func TestGameStatusString(t *testing.T) {
	cases := []struct {
		in   GameStatus
		want string
	}{
		{GameStatusInProgress, "IN_PROGRESS"},
		{GameStatusChallengerWins, "CHALLENGER_WINS"},
		{GameStatusDefenderWins, "DEFENDER_WINS"},
		{GameStatus(99), "UNKNOWN(99)"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("GameStatus(%d).String() = %q, want %q", uint8(tc.in), got, tc.want)
		}
	}
}
