package app

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"op-ctl/internal/config"
	"op-ctl/internal/elnode"
)

func sampleTxPoolTx() *elnode.TxPoolTx {
	oneEther := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	return &elnode.TxPoolTx{
		Hash:       "0xhash",
		From:       "0xfrom",
		To:         "0xto",
		Nonce:      7,
		Gas:        21000,
		Value:      oneEther,
		GasPrice:   big.NewInt(1_000_000_000),
		MaxFee:     big.NewInt(2_000_000_000),
		MaxTip:     big.NewInt(1),
		Type:       2,
		ChainID:    0xa5e8,
		Input:      []byte{0xde, 0xad, 0xbe, 0xef},
		AccessList: json.RawMessage(`[]`),
		R:          "0xrsig",
		S:          "0xssig",
		V:          "0x1",
		YParity:    "0x1",
	}
}

// TestTxDetailScreen_View_Full feeds a fully-populated TxPoolTx and
// asserts every field label + value appears in the rendered output.
func TestTxDetailScreen_View_Full(t *testing.T) {
	tx := sampleTxPoolTx()
	s := newStatusTxPoolTxDetailScreen(
		config.Backend{Name: "seq-1", ExecutionRPCURL: "http://x"},
		tx,
	)
	s = feedTxF(s, tea.WindowSizeMsg{Width: 100, Height: 40})
	out := stripANSI(s.View())

	for _, want := range []string{
		"tx detail · seq-1",
		"http://x",
		"sender 0xfrom",
		"nonce 7",
		"0xhash",
		"0xfrom",
		"0xto",
		"21000",
		"1000000000",
		"2000000000",
		"chainId",
		"input (4 bytes)",
		"0xdeadbeef",
		"signature",
		"0xrsig",
		"in-pool flags",
		"null — in pool",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View should contain %q:\n%s", want, out)
		}
	}
}

// TestTxDetailScreen_View_TxNotInPool covers the race scenario
// (Pre-mortem #3): tx == nil renders the friendly "tx no longer in
// pool" message instead of crashing. Even with the cache pointer
// path, an operator who selects a row JUST as it gets mined can
// land here with a nil pointer.
func TestTxDetailScreen_View_TxNotInPool(t *testing.T) {
	s := newStatusTxPoolTxDetailScreen(
		config.Backend{Name: "seq-1", ExecutionRPCURL: "http://x"},
		nil,
	)
	s = feedTxF(s, tea.WindowSizeMsg{Width: 100, Height: 20})
	out := stripANSI(s.View())
	if !strings.Contains(out, "tx no longer in pool") {
		t.Errorf("expected 'tx no longer in pool' message:\n%s", out)
	}
}

// TestTxDetailScreen_QEmitsPopMsg confirms q pops back rather than
// quitting.
func TestTxDetailScreen_QEmitsPopMsg(t *testing.T) {
	s := newStatusTxPoolTxDetailScreen(
		config.Backend{Name: "seq-1", ExecutionRPCURL: "http://x"},
		sampleTxPoolTx(),
	)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should emit a tea.Cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Errorf("q cmd: got %T, want popMsg", cmd())
	}
}

// TestTxDetailScreen_Scroll exercises the scroll key handlers — j
// bumps offset, k decrements (clamps to 0).
func TestTxDetailScreen_Scroll(t *testing.T) {
	s := newStatusTxPoolTxDetailScreen(
		config.Backend{Name: "seq-1", ExecutionRPCURL: "http://x"},
		sampleTxPoolTx(),
	)
	s = feedTxF(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.offset != 1 {
		t.Errorf("j should set offset=1, got %d", s.offset)
	}
	s = feedTxF(s, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.offset != 0 {
		t.Errorf("k after j should set offset=0, got %d", s.offset)
	}
}

func feedTxF(s statusTxPoolTxDetailScreen, msgs ...tea.Msg) statusTxPoolTxDetailScreen {
	for _, m := range msgs {
		next, _ := s.Update(m)
		s = next.(statusTxPoolTxDetailScreen)
	}
	return s
}
