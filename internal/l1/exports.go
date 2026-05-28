package l1

import "math/big"

// Exported wrappers for the package-internal ABI helpers, so sibling
// packages (e.g. internal/l2) can reuse the same hand-rolled codec
// without copy-pasting it. Each wrapper trivially delegates; the
// unexported names remain in use by the rest of this package so the
// existing call sites and tests don't churn.
//
// Pinning these as exports rather than refactoring abi.go itself keeps
// the change additive — l1's own internals are unaffected.

// SelectorOf returns the 4-byte function selector for the given
// canonical Solidity signature (e.g. "totalProcessed()"), 0x-prefixed
// lowercase hex.
func SelectorOf(sig string) string { return selectorOf(sig) }

// DecodeHexData strips the 0x prefix and decodes the eth_call result
// body into raw bytes.
func DecodeHexData(s string) ([]byte, error) { return decodeHexData(s) }

// WordAt slices the 32-byte word at word index idx from raw.
func WordAt(raw []byte, idx int) ([]byte, error) { return wordAt(raw, idx) }

// WordToUint256 converts a 32-byte big-endian word into a *big.Int.
func WordToUint256(w []byte) *big.Int { return wordToUint256(w) }

// WordToUint64 returns the trailing 8 bytes of the word as a big-endian
// uint64.
func WordToUint64(w []byte) uint64 { return wordToUint64(w) }

// WordToUint32 returns the trailing 4 bytes of the word as a big-endian
// uint32.
func WordToUint32(w []byte) uint32 { return wordToUint32(w) }

// WordToUint16 returns the trailing 2 bytes of the word as a big-endian
// uint16.
func WordToUint16(w []byte) uint16 { return wordToUint16(w) }

// WordToUint8 returns the trailing byte.
func WordToUint8(w []byte) uint8 { return wordToUint8(w) }

// WordToAddress returns "0x"+lowercase hex of the trailing 20 bytes.
func WordToAddress(w []byte) string { return wordToAddress(w) }

// ReadDynamicOffset reads the byte offset stored in a 32-byte head
// word — the first hop in ABI-decoding a dynamic-typed return value.
func ReadDynamicOffset(w []byte) (int, error) { return readDynamicOffset(w) }

// DecodeStringAt reads an ABI-encoded string at byte offset off
// inside raw: a uint256 length word followed by padded UTF-8 bytes.
func DecodeStringAt(raw []byte, off int) (string, error) { return decodeStringAt(raw, off) }
