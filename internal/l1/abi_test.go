package l1

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

func TestSelectorOf_KnownSignatures(t *testing.T) {
	cases := []struct {
		sig  string
		want string
	}{
		// keccak256("gameCount()")[0:4] — cross-checks the Phase-1
		// hardcoded constant in client.go.
		{"gameCount()", "0x4d1975b4"},
		// keccak256("gameAtIndex(uint256)")[0:4]
		{"gameAtIndex(uint256)", "0xbb8aa1fc"},
		// keccak256("version()")[0:4]
		{"version()", "0x54fd4d50"},
	}
	for _, tc := range cases {
		got := selectorOf(tc.sig)
		if got != tc.want {
			t.Errorf("selectorOf(%q): got %s, want %s", tc.sig, got, tc.want)
		}
	}
}

func TestSelectorOf_MatchesPhase1Constant(t *testing.T) {
	if got := selectorOf("gameCount()"); got != gameCountSelector {
		t.Errorf("selectorOf gameCount() = %s, hardcoded gameCountSelector = %s", got, gameCountSelector)
	}
}

func TestEncodeUint256(t *testing.T) {
	cases := []struct {
		name string
		in   *big.Int
		want string
	}{
		{"zero", big.NewInt(0), strings.Repeat("0", 64)},
		{"one", big.NewInt(1), strings.Repeat("0", 63) + "1"},
		{"max-uint64", new(big.Int).SetUint64(^uint64(0)), strings.Repeat("0", 48) + strings.Repeat("f", 16)},
		{"max-uint256", new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), strings.Repeat("f", 64)},
	}
	for _, tc := range cases {
		got, err := encodeUint256(tc.in)
		if err != nil {
			t.Errorf("%s: encodeUint256 err: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestEncodeUint256_Overflow(t *testing.T) {
	tooBig := new(big.Int).Lsh(big.NewInt(1), 256) // exactly 257 bits
	if _, err := encodeUint256(tooBig); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestEncodeUint256_Negative(t *testing.T) {
	if _, err := encodeUint256(big.NewInt(-1)); err == nil {
		t.Fatal("expected error for negative input")
	}
}

func TestEncodeUint64(t *testing.T) {
	got := encodeUint64(7)
	want := strings.Repeat("0", 63) + "7"
	if got != want {
		t.Errorf("encodeUint64(7) = %s, want %s", got, want)
	}
}

func TestDecodeHexData(t *testing.T) {
	b, err := decodeHexData("0xdeadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(b) != "deadbeef" {
		t.Errorf("got %x, want deadbeef", b)
	}
	// Odd-length padding.
	b, err = decodeHexData("0xabc")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(b) != "0abc" {
		t.Errorf("got %x, want 0abc", b)
	}
}

func TestDecodeHexData_Empty(t *testing.T) {
	if _, err := decodeHexData(""); err == nil {
		t.Fatal("expected error for empty")
	}
	if _, err := decodeHexData("0x"); err == nil {
		t.Fatal("expected error for 0x only")
	}
}

func TestWordAt(t *testing.T) {
	raw := make([]byte, 96)
	for i := range raw {
		raw[i] = byte(i)
	}
	w, err := wordAt(raw, 1) // bytes 32..63
	if err != nil {
		t.Fatal(err)
	}
	if w[0] != 32 || w[31] != 63 {
		t.Errorf("wordAt(1) bounds wrong: first=%d last=%d", w[0], w[31])
	}
	if _, err := wordAt(raw, 3); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestWordToTypes(t *testing.T) {
	// 32-byte word with last 20 bytes = 0xaabb...20, last 8 bytes = ..., etc.
	w := make([]byte, 32)
	// fill last 20 bytes with a recognizable pattern
	for i := 12; i < 32; i++ {
		w[i] = byte(i)
	}
	// Address = "0x" + hex of last 20 bytes
	wantAddr := "0x0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if got := wordToAddress(w); got != wantAddr {
		t.Errorf("wordToAddress: got %s, want %s", got, wantAddr)
	}

	// Bytes32 = "0x" + 32 byte hex
	if got, wantLen := wordToBytes32(w), 2+64; len(got) != wantLen {
		t.Errorf("wordToBytes32 length: got %d, want %d", len(got), wantLen)
	}

	// uint64 = last 8 bytes big-endian
	want64 := uint64(0)
	for i := 24; i < 32; i++ {
		want64 = (want64 << 8) | uint64(w[i])
	}
	if got := wordToUint64(w); got != want64 {
		t.Errorf("wordToUint64: got %d, want %d", got, want64)
	}

	// uint32 = last 4 bytes
	want32 := uint32(0)
	for i := 28; i < 32; i++ {
		want32 = (want32 << 8) | uint32(w[i])
	}
	if got := wordToUint32(w); got != want32 {
		t.Errorf("wordToUint32: got %d, want %d", got, want32)
	}

	// uint8 = last byte
	if got := wordToUint8(w); got != 31 {
		t.Errorf("wordToUint8: got %d, want 31", got)
	}

	// bool = last byte != 0
	if got := wordToBool(w); !got {
		t.Errorf("wordToBool: got false, want true")
	}
	zero := make([]byte, 32)
	if got := wordToBool(zero); got {
		t.Errorf("wordToBool(zero): got true, want false")
	}
}

func TestWordToUint256(t *testing.T) {
	w := make([]byte, 32)
	w[31] = 5
	if got := wordToUint256(w); got.Int64() != 5 {
		t.Errorf("uint256: got %d, want 5", got.Int64())
	}
}

// Build an ABI-encoded `(bytes32, string)` return for testing the
// dynamic decoder. Head: word0 = some hash, word1 = offset (0x40 = 64).
// Tail at offset 0x40: length word (n), then n bytes padded to 32.
func buildBytes32AndString(hashHex string, s string) []byte {
	out := make([]byte, 0, 128)
	// word0: bytes32 (hash)
	hash, _ := hex.DecodeString(hashHex)
	if len(hash) != 32 {
		panic("hash must be 32 bytes")
	}
	out = append(out, hash...)
	// word1: offset = 0x40 (64) — points to the start of the dynamic
	// payload (length word).
	off := make([]byte, 32)
	off[31] = 64
	out = append(out, off...)
	// length word
	lenW := make([]byte, 32)
	n := len(s)
	for i := 0; i < 8; i++ {
		lenW[31-i] = byte(n >> (8 * i))
	}
	out = append(out, lenW...)
	// payload, padded to 32
	pad := (32 - n%32) % 32
	out = append(out, []byte(s)...)
	out = append(out, make([]byte, pad)...)
	return out
}

func TestDecodeStringAt(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	raw := buildBytes32AndString(hash, "OptimismPortal 1.4.0")
	// word 1 holds the offset to the string
	w1, _ := wordAt(raw, 1)
	off, err := readDynamicOffset(w1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeStringAt(raw, off)
	if err != nil {
		t.Fatal(err)
	}
	if got != "OptimismPortal 1.4.0" {
		t.Errorf("got %q, want %q", got, "OptimismPortal 1.4.0")
	}
}

func TestDecodeBytesAt_LongerPayload(t *testing.T) {
	// 70-byte payload exercises multi-word padding (70 → ceil/32 = 96-byte tail).
	payload := strings.Repeat("z", 70)
	raw := buildBytes32AndString(strings.Repeat("00", 32), payload)
	w1, _ := wordAt(raw, 1)
	off, _ := readDynamicOffset(w1)
	got, err := decodeBytesAt(raw, off)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestDecodeBytesAt_TruncatedPayload(t *testing.T) {
	// Length says 50, buffer only has 20 bytes after length word.
	// Layout: [head pad 32B][length word 32B at off=32][20-byte payload]
	// Last byte of the length word is at raw[63].
	raw := make([]byte, 32+32+20)
	raw[63] = 50 // length = 50, but only 20 payload bytes follow
	if _, err := decodeBytesAt(raw, 32); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestReadDynamicOffset_Sane(t *testing.T) {
	w := make([]byte, 32)
	w[31] = 0x60 // 96
	off, err := readDynamicOffset(w)
	if err != nil {
		t.Fatal(err)
	}
	if off != 96 {
		t.Errorf("got %d, want 96", off)
	}
}
