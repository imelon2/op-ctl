package batchcache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"op-ctl/internal/etherscan"
)

// Store wraps the *sql.DB plus the on-disk path so error messages can
// be specific. All methods are safe for sequential use within a
// single goroutine — concurrent writers are not supported (matches
// `db.SetMaxOpenConns(1)` set in Open).
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) `{baseDir}/{l2ChainID}/batcher.db` and
// applies schemaV1 + WAL pragmas. baseDir is typically the directory
// of the loading config.toml so the cache lives next to the
// chain-specific state.json + namespace dir.
//
// l2ChainID is passed verbatim — the deep-interview spec calls for
// preserving the on-disk case (see contracts.LoadL2ChainID for the
// matching producer). An empty l2ChainID is rejected so we never
// create a `{baseDir}//batcher.db` ambiguous path.
func Open(baseDir, l2ChainID string) (*Store, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("batchcache: baseDir is empty")
	}
	if l2ChainID == "" {
		return nil, fmt.Errorf("batchcache: l2ChainID is empty")
	}
	dir := filepath.Join(baseDir, l2ChainID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("batchcache: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "batcher.db")
	// The DSN explicitly enables FK + foreign_keys to keep future
	// migrations safe; today there are no FK constraints to enforce.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("batchcache: open %s: %w", path, err)
	}
	// Single open conn keeps the writer story simple: every UpsertPage
	// + List + Get serializes through one connection, so modernc/sqlite's
	// "one writer at a time" rule is automatic. Concurrent callers
	// would block at the database/sql layer rather than racing on the
	// SQLite mutex.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(pragmaWAL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("batchcache: enable WAL on %s: %w", path, err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("batchcache: apply schemaV1 on %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// Path returns the absolute path the store writes to. Used by callers
// for diagnostic logs (e.g. "cache at /…/batcher.db (12 batches)").
func (s *Store) Path() string { return s.path }

// Close releases the underlying *sql.DB. Safe to call on a nil Store
// (no-op) so cleanup deferred in CLI RunE branches that early-fail
// never panics.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// LastSyncedAt reads meta.last_synced_at. A zero time + nil error
// means the cache is fresh (no sync has succeeded yet) — the caller
// treats that as "always expired" so the next op-ctl run triggers an
// Etherscan pass.
func (s *Store) LastSyncedAt() (time.Time, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaKeyLastSyncedAt).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("batchcache: read last_synced_at: %w", err)
	}
	t, perr := time.Parse(time.RFC3339, raw)
	if perr != nil {
		// A malformed meta row is operationally indistinguishable
		// from "never synced" — log nothing, just treat as expired.
		return time.Time{}, nil
	}
	return t, nil
}

// MaxBlockNumber returns the highest block_number persisted, or 0
// when the table is empty. `op-ctl read batch` resumes incremental
// fetches from `max(MaxBlockNumber()+1, cfg.Batch.StartBlock)`.
func (s *Store) MaxBlockNumber() (uint64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(block_number), 0) FROM batches`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("batchcache: max block: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return uint64(n.Int64), nil
}

// UpsertPage commits one Etherscan-returned page of transactions in
// a single SQL transaction:
//
//  1. INSERT OR IGNORE per row — duplicates from an overlapping
//     re-fetch are silently dropped, preserving the existing rows.
//  2. INSERT OR REPLACE meta.last_synced_at = now (UTC, RFC3339) and
//     meta.last_synced_block = max(block_number across THIS page).
//
// The ratchet on last_synced_block intentionally uses the page-max
// rather than the cumulative max — the Etherscan client guarantees
// sort=asc, so the page-max is monotone non-decreasing across calls.
// The empty-page case is a no-op (no commit, meta unchanged), which
// matches the etherscan client's "no transactions found" terminator.
func (s *Store) UpsertPage(_ int, txs []etherscan.Tx) error {
	if len(txs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("batchcache: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO batches (
			block_number, timestamp, tx_hash, from_addr, to_addr,
			value_wei, gas_limit, gas_used, gas_price,
			method_id, input_data, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("batchcache: prepare insert: %w", err)
	}
	defer stmt.Close()

	var maxBlock uint64
	for _, t := range txs {
		if _, err := stmt.ExecContext(ctx,
			int64(t.BlockNumber), t.TimeStamp, t.Hash, t.From, t.To,
			t.Value, int64(t.Gas), int64(t.GasUsed), t.GasPrice,
			t.MethodID, []byte(t.Input), t.Status,
		); err != nil {
			return fmt.Errorf("batchcache: insert tx %s: %w", t.Hash, err)
		}
		if t.BlockNumber > maxBlock {
			maxBlock = t.BlockNumber
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyLastSyncedAt, now); err != nil {
		return fmt.Errorf("batchcache: update last_synced_at: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyLastSyncedBlock, strconv.FormatUint(maxBlock, 10)); err != nil {
		return fmt.Errorf("batchcache: update last_synced_block: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("batchcache: commit: %w", err)
	}
	return nil
}

// AverageGaps returns the mean inter-batch block delta and time delta
// (in seconds) computed over the full cache. The math is (max - min) /
// (n - 1), so it is O(1) in SQLite and works on millions of rows. Both
// values are zero when fewer than 2 rows are cached.
func (s *Store) AverageGaps(ctx context.Context) (avgBlocks, avgSeconds float64, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(CAST(MAX(block_number) - MIN(block_number) AS REAL) / NULLIF(COUNT(*) - 1, 0), 0),
			COALESCE(CAST(MAX(timestamp)    - MIN(timestamp)    AS REAL) / NULLIF(COUNT(*) - 1, 0), 0)
		FROM batches
	`)
	if err = row.Scan(&avgBlocks, &avgSeconds); err != nil {
		return 0, 0, fmt.Errorf("batchcache: avg gaps: %w", err)
	}
	return avgBlocks, avgSeconds, nil
}

// MarkSynced bumps meta.last_synced_at to time.Now().UTC() without
// touching the row table. Used by the prefetcher to record a
// "sync attempt finished" even when Etherscan returned zero new rows
// — without this, TTL would never reset once the cache caught up to
// chain head and every invocation would re-page.
func (s *Store) MarkSynced(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyLastSyncedAt, now)
	if err != nil {
		return fmt.Errorf("batchcache: mark synced: %w", err)
	}
	return nil
}

// Count returns the total number of cached transactions. Used by the
// TUI header (`cached: N batches`).
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM batches`).Scan(&n); err != nil {
		return 0, fmt.Errorf("batchcache: count: %w", err)
	}
	return n, nil
}

// List returns up to `limit` transactions starting at `offset`,
// ordered DESC by block_number (newest first). Matches the TUI list
// screen's contract: page i shows rows offset = i*pageSize, limit =
// pageSize.
func (s *Store) List(ctx context.Context, limit, offset int) ([]etherscan.Tx, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT block_number, timestamp, tx_hash, from_addr, to_addr,
		       value_wei, gas_limit, gas_used, gas_price,
		       method_id, input_data, status
		  FROM batches
		 ORDER BY block_number DESC
		 LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("batchcache: list: %w", err)
	}
	defer rows.Close()
	return scanTxRows(rows)
}

// Get returns the cached row for tx_hash, or (nil, nil) when absent.
// The TUI detail screen uses this lazy lookup so the list page does
// not have to pre-materialize every row's full input blob.
func (s *Store) Get(ctx context.Context, txHash string) (*etherscan.Tx, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT block_number, timestamp, tx_hash, from_addr, to_addr,
		       value_wei, gas_limit, gas_used, gas_price,
		       method_id, input_data, status
		  FROM batches
		 WHERE tx_hash = ?
	`, txHash)
	if err != nil {
		return nil, fmt.Errorf("batchcache: get %s: %w", txHash, err)
	}
	defer rows.Close()
	out, err := scanTxRows(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// scanTxRows hydrates a *sql.Rows into typed etherscan.Tx values.
// input_data is stored as BLOB and rendered back into the string
// form `etherscan.Tx.Input` expects.
func scanTxRows(rows *sql.Rows) ([]etherscan.Tx, error) {
	var out []etherscan.Tx
	for rows.Next() {
		var (
			t        etherscan.Tx
			input    []byte
			blkNum   int64
			gasLimit int64
			gasUsed  int64
		)
		if err := rows.Scan(
			&blkNum, &t.TimeStamp, &t.Hash, &t.From, &t.To,
			&t.Value, &gasLimit, &gasUsed, &t.GasPrice,
			&t.MethodID, &input, &t.Status,
		); err != nil {
			return nil, fmt.Errorf("batchcache: scan row: %w", err)
		}
		t.BlockNumber = uint64(blkNum)
		t.Gas = uint64(gasLimit)
		t.GasUsed = uint64(gasUsed)
		t.Input = string(input)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("batchcache: rows err: %w", err)
	}
	return out, nil
}
