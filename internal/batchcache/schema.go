// Package batchcache persists Etherscan-fetched batcher transactions
// in a per-L2-chain SQLite database (`{configDir}/{l2-chainid}/batcher.db`)
// so `op-ctl read batch` can survive partial syncs, throttle around
// Etherscan's daily quota, and render newest-first in the TUI without
// re-hitting the API on every invocation.
//
// The package leans on `modernc.org/sqlite` to stay CGO-free, which
// keeps op-ctl's pure-Go bootstrap intact (matches the project's "no
// CGO" principle from the deep-interview spec).
package batchcache

// schemaV1 is the IF-NOT-EXISTS migration applied at Open() time.
// value_wei and gas_price are TEXT because uint256 overflows INTEGER.
// The DESC index on block_number serves the newest-first TUI read.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS batches (
	block_number INTEGER NOT NULL,
	timestamp    INTEGER NOT NULL,
	tx_hash      TEXT    PRIMARY KEY,
	from_addr    TEXT    NOT NULL,
	to_addr      TEXT    NOT NULL,
	value_wei    TEXT    NOT NULL,
	gas_limit    INTEGER NOT NULL DEFAULT 0,
	gas_used     INTEGER NOT NULL,
	gas_price    TEXT    NOT NULL DEFAULT '0',
	method_id    TEXT    NOT NULL,
	input_data   BLOB,
	status       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_batches_block ON batches(block_number DESC);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// pragmaWAL enables write-ahead-logging. The crash-friendly partial
// commit story relies on it: a process killed mid-sync still leaves
// the durable rows reachable on next open, and -wal/-shm sidecar files
// resolve cleanly when SQLite re-opens with the same flags.
const pragmaWAL = `PRAGMA journal_mode=WAL;`

// metaKeyLastSyncedAt + metaKeyLastSyncedBlock are the only meta keys
// the package writes today; centralizing them avoids typos when a
// future Open() check needs to scan them.
const (
	metaKeyLastSyncedAt    = "last_synced_at"
	metaKeyLastSyncedBlock = "last_synced_block"
)
