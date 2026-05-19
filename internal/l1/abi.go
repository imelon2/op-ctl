package l1

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// selectorOf returns the 4-byte function selector for an EVM signature
// string as a 0x-prefixed lowercase hex. The selector is
// keccak256(sig)[0:4] using Ethereum's keccak256 — NewLegacyKeccak256
// in golang.org/x/crypto/sha3 (NOT the NIST SHA3-256 variant).
//
// Callers pass the exact canonical signature, e.g. "gameAtIndex(uint256)"
// — function name + paren-wrapped, comma-separated argument types with
// no spaces or names. Mistyping the signature silently changes the
// selector, so each call site should pin the literal string and add a
// comment naming the Solidity declaration it corresponds to.
func selectorOf(sig string) string {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(sig))
	sum := h.Sum(nil)
	return "0x" + hex.EncodeToString(sum[:4])
}

// encodeUint256 returns the 64-character lowercase hex (no 0x prefix)
// of n encoded as a big-endian 32-byte word. n must fit in 256 bits;
// values exceeding that produce an error (rather than silently
// truncating, which would build malformed calldata).
func encodeUint256(n *big.Int) (string, error) {
	if n == nil {
		return "", fmt.Errorf("encodeUint256: nil input")
	}
	if n.Sign() < 0 {
		return "", fmt.Errorf("encodeUint256: negative input")
	}
	b := n.Bytes()
	if len(b) > 32 {
		return "", fmt.Errorf("encodeUint256: value exceeds 256 bits (%d bytes)", len(b))
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return hex.EncodeToString(out), nil
}

// encodeUint64 is a convenience wrapper for the common "index" case.
func encodeUint64(n uint64) string {
	s, _ := encodeUint256(new(big.Int).SetUint64(n))
	return s
}

// decodeHexData strips an optional 0x prefix and returns the raw bytes
// of the eth_call result body. Callers pass this to the word helpers
// below to slice individual 32-byte words.
func decodeHexData(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if len(s) == 0 {
		return nil, fmt.Errorf("decodeHexData: empty payload")
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decodeHexData: hex decode: %w", err)
	}
	return b, nil
}

// wordAt returns the 32-byte word at word index idx (byte offset
// idx*32). Returns an error when the buffer is too short — callers
// should never silently read past the end.
func wordAt(raw []byte, idx int) ([]byte, error) {
	off := idx * 32
	if off+32 > len(raw) {
		return nil, fmt.Errorf("wordAt: out of range — buf=%d need=%d at idx=%d", len(raw), off+32, idx)
	}
	return raw[off : off+32], nil
}

// wordToUint256 converts a 32-byte big-endian word into a *big.Int.
func wordToUint256(w []byte) *big.Int { return new(big.Int).SetBytes(w) }

// wordToUint64 returns the last 8 bytes of the word as a big-endian
// uint64. Solidity's Timestamp (uint64) and the lower portion of
// uint256 indices decode this way.
func wordToUint64(w []byte) uint64 {
	var n uint64
	for _, b := range w[24:] {
		n = (n << 8) | uint64(b)
	}
	return n
}

// wordToUint32 returns the last 4 bytes of the word as a big-endian
// uint32. Solidity's GameType is uint32.
func wordToUint32(w []byte) uint32 {
	var n uint32
	for _, b := range w[28:] {
		n = (n << 8) | uint32(b)
	}
	return n
}

// wordToUint8 returns the last byte. Solidity's GameStatus enum and
// other small enums are packed in the lowest byte of a 32-byte word.
func wordToUint8(w []byte) uint8 { return w[31] }

// wordToBool returns the last byte != 0. Solidity bool is right-
// aligned in a 32-byte word.
func wordToBool(w []byte) bool { return w[31] != 0 }

// wordToAddress returns "0x"+lowercase hex of the last 20 bytes —
// Ethereum addresses are right-aligned in their 32-byte ABI word.
func wordToAddress(w []byte) string {
	return "0x" + hex.EncodeToString(w[12:])
}

// wordToBytes32 returns the full 32-byte word as "0x"+hex. Used for
// hash-typed returns (rootClaim, l1Head, absolutePrestate, ...).
func wordToBytes32(w []byte) string {
	return "0x" + hex.EncodeToString(w)
}

// decodeStringAt reads an ABI-encoded `string` at the byte offset
// inside raw: offset → length → padded payload. Returns the string.
func decodeStringAt(raw []byte, off int) (string, error) {
	b, err := decodeBytesAt(raw, off)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeBytesAt reads an ABI-encoded `bytes` at the byte offset off:
// uint256 length word followed by length bytes padded to a 32-byte
// boundary.
func decodeBytesAt(raw []byte, off int) ([]byte, error) {
	if off+32 > len(raw) {
		return nil, fmt.Errorf("decodeBytesAt: length word out of range at off=%d (buf=%d)", off, len(raw))
	}
	lenW := raw[off : off+32]
	n := int(wordToUint64(lenW)) // ABI string/bytes lengths fit in uint64 for any sane payload.
	start := off + 32
	if start+n > len(raw) {
		return nil, fmt.Errorf("decodeBytesAt: payload truncated — start=%d len=%d buf=%d", start, n, len(raw))
	}
	return raw[start : start+n], nil
}

// readDynamicOffset returns the byte offset stored in a head-section
// word. ABI offsets are uint256 but always small enough to fit in a
// signed int — we widen carefully and reject overflow.
func readDynamicOffset(w []byte) (int, error) {
	n := wordToUint256(w)
	if !n.IsInt64() {
		return 0, fmt.Errorf("readDynamicOffset: offset too large")
	}
	v := n.Int64()
	if v < 0 || v > (1<<31)-1 {
		return 0, fmt.Errorf("readDynamicOffset: offset out of int range: %d", v)
	}
	return int(v), nil
}
