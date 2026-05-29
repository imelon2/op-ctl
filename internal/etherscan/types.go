// Package etherscan exposes a minimal, stateless client for the
// Etherscan V2 multi-chain API. It is shaped to mirror the package-
// function style of internal/l1 (EthCallBatch et al.) — there is no
// NewClient constructor; HTTP plumbing and chainID resolution are
// the caller's responsibility, passed in per call.
//
// Only the endpoints `op-ctl read batch` needs are implemented:
//   - ResolveChainID:  JSON-RPC eth_chainId on an L1 endpoint, used
//                      to compute the V2 `chainid` query parameter.
//   - FetchTxList:     paginated account-txlist with caller-supplied
//                      onPage callback (so the cache layer can commit
//                      per-page and survive a partial sync).
package etherscan

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Tx is the slim projection of an Etherscan txlist row consumed by
// `op-ctl read batch`. Etherscan returns every numeric field as a
// JSON string, so decoding goes through txRaw + Decode before the
// caller sees a typed struct.
//
// Field choices match the SQLite cache columns in
// internal/batchcache.schemaV1 — the cache stores only what the TUI
// list and detail screens render. Adding a field here means also
// extending the schema + UpsertPage.
type Tx struct {
	BlockNumber uint64 // block_number
	TimeStamp   int64  // unix seconds
	Hash        string // 0x... tx hash
	From        string // sender address (the batcher EOA)
	To          string // recipient (the batch inbox)
	Value       string // wei as decimal string (uint256 overflows int64)
	Gas         uint64 // gas limit the tx requested
	GasUsed     uint64 // gas units actually consumed
	GasPrice    string // wei as decimal string (legacy field; EIP-1559 reports the effective price)
	MethodID    string // first-4-byte selector (e.g. "0x6a")
	Input       string // full calldata, 0x-prefixed
	Status      int    // 1 = success, 0 = reverted
}

// txRaw mirrors the on-the-wire JSON: every field is a string.
// Conversion to typed Tx happens in (txRaw).toTx so the caller sees
// numerically-typed values.
type txRaw struct {
	BlockNumber     string `json:"blockNumber"`
	TimeStamp       string `json:"timeStamp"`
	Hash            string `json:"hash"`
	From            string `json:"from"`
	To              string `json:"to"`
	Value           string `json:"value"`
	Gas             string `json:"gas"`
	GasUsed         string `json:"gasUsed"`
	GasPrice        string `json:"gasPrice"`
	MethodID        string `json:"methodId"`
	Input           string `json:"input"`
	TxReceiptStatus string `json:"txreceipt_status"`
}

// listResponse is the envelope Etherscan V2 returns for any
// `module=account&action=txlist` page. Result is decoded as raw JSON
// because Etherscan sometimes returns an error string instead of an
// array (notably on rate-limit / API-key issues) — the wrapper
// surfaces that case as a typed error.
type listResponse struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// decodeTxList parses one Etherscan txlist HTTP response body. It
// returns the status flag verbatim ("1" / "0"), the human-readable
// message ("OK" / "No transactions found" / "Max rate limit reached"
// / …), and — when the body actually carries a tx array — the parsed
// Tx slice.
//
// status=="0" with message=="No transactions found" is a legitimate
// terminal page (we have caught up to the chain head), so the caller
// treats it as a clean stop, not an error. Any other status=="0"
// case is left to the caller to wrap with the page number and the
// originating address for diagnostics.
func decodeTxList(body []byte) (status, message string, txs []Tx, err error) {
	var env listResponse
	if err = json.Unmarshal(body, &env); err != nil {
		return "", "", nil, fmt.Errorf("etherscan: decode envelope: %w", err)
	}
	status = env.Status
	message = env.Message
	// status="0" + non-array result (a string) is the rate-limit /
	// auth-error branch; return the message so the caller can wrap
	// it with the current page number.
	if status == "0" {
		// Result may be a JSON string or null in this case — do not
		// attempt to parse as an array. Empty txs is correct.
		return status, message, nil, nil
	}
	var raws []txRaw
	if err = json.Unmarshal(env.Result, &raws); err != nil {
		return status, message, nil, fmt.Errorf("etherscan: decode result array: %w", err)
	}
	txs = make([]Tx, 0, len(raws))
	for i, r := range raws {
		tx, terr := r.toTx()
		if terr != nil {
			return status, message, nil, fmt.Errorf("etherscan: decode tx[%d]: %w", i, terr)
		}
		txs = append(txs, tx)
	}
	return status, message, txs, nil
}

// toTx narrows an on-the-wire txRaw into the typed Tx. Numeric fields
// are parsed via strconv; Value is preserved as a decimal string
// because wei amounts can overflow uint64 (uint256 on chain).
func (r txRaw) toTx() (Tx, error) {
	blk, err := strconv.ParseUint(strings.TrimSpace(r.BlockNumber), 10, 64)
	if err != nil {
		return Tx{}, fmt.Errorf("blockNumber %q: %w", r.BlockNumber, err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(r.TimeStamp), 10, 64)
	if err != nil {
		return Tx{}, fmt.Errorf("timeStamp %q: %w", r.TimeStamp, err)
	}
	gas, err := strconv.ParseUint(strings.TrimSpace(r.GasUsed), 10, 64)
	if err != nil {
		return Tx{}, fmt.Errorf("gasUsed %q: %w", r.GasUsed, err)
	}
	// `gas` is the requested limit; tolerate empty/missing on pages
	// from older Etherscan endpoints by defaulting to 0 rather than
	// failing the whole row.
	var gasLimit uint64
	if s := strings.TrimSpace(r.Gas); s != "" {
		v, perr := strconv.ParseUint(s, 10, 64)
		if perr != nil {
			return Tx{}, fmt.Errorf("gas %q: %w", r.Gas, perr)
		}
		gasLimit = v
	}
	// txreceipt_status is "1" on success, "0" on revert, sometimes
	// "" for pre-Byzantium txs (irrelevant for batch txs on modern
	// chains). Treat empty as success so the cache row stays usable.
	status := 1
	if s := strings.TrimSpace(r.TxReceiptStatus); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil {
			return Tx{}, fmt.Errorf("txreceipt_status %q: %w", r.TxReceiptStatus, perr)
		}
		status = n
	}
	return Tx{
		BlockNumber: blk,
		TimeStamp:   ts,
		Hash:        r.Hash,
		From:        r.From,
		To:          r.To,
		Value:       r.Value,
		Gas:         gasLimit,
		GasUsed:     gas,
		GasPrice:    r.GasPrice,
		MethodID:    r.MethodID,
		Input:       r.Input,
		Status:      status,
	}, nil
}
