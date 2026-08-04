// Package explorer provides a PostgreSQL indexer for Aperod blockchain data.
// The indexer uses its own flat data types (BlockData, TxData) so callers
// (e.g. cmd/node) convert from core types before passing them in.  This keeps
// the package free of core-package dependencies and easy to test.
//
// Usage:
//
//      idx, err := explorer.New(os.Getenv("DATABASE_URL"))
//      if err != nil { log.Fatal(err) }
//      defer idx.Close()
//
//      // Index a single block with its transactions:
//      if err := idx.IndexBlock(bd, txList); err != nil { log.Fatal(err) }
//
//      // Full resync (call IndexBlock for each block in sequence):
//      if err := idx.ResyncFromHeight(0, fetchFn); err != nil { log.Fatal(err) }
package explorer

import (
        "context"
        "database/sql"
        "fmt"
        "time"

        _ "github.com/lib/pq"
)

// ─── Data-transfer types ──────────────────────────────────────────────────────

// BlockData is a flat representation of a block suitable for indexing.
// Callers should convert from *core.Block before calling IndexBlock.
type BlockData struct {
        Height       uint64
        Hash         string // 64-char hex
        PrevHash     string // 64-char hex
        TimestampNs  int64  // Unix nanoseconds
        ValidatorPub string // hex-encoded
        MerkleRoot   string // 64-char hex
        TxCount      int
}

// TxData is a flat representation of a transaction suitable for indexing.
type TxData struct {
        Hash        string // 64-char hex
        BlockHash   string // 64-char hex
        BlockHeight uint64
        TxIndex     int
        IsCoinbase  bool
        Inputs      int
        Outputs     int
        Fee         uint64
        SizeBytes   int
        Version     int
}

// AddrTxData links an address to a transaction output.
type AddrTxData struct {
        Address     string
        TxHash      string
        BlockHeight uint64
        TxIndex     int
        OutputIndex int
}

// ─── Indexer ──────────────────────────────────────────────────────────────────

// Indexer writes Aperod blockchain data to PostgreSQL.
type Indexer struct {
        db *sql.DB
}

// New opens a connection to the PostgreSQL database and ensures the schema exists.
func New(connStr string) (*Indexer, error) {
        db, err := sql.Open("postgres", connStr)
        if err != nil {
                return nil, fmt.Errorf("explorer: open db: %w", err)
        }
        db.SetMaxOpenConns(10)
        db.SetMaxIdleConns(5)
        db.SetConnMaxLifetime(5 * time.Minute)

        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()

        if err := db.PingContext(ctx); err != nil {
                db.Close()
                return nil, fmt.Errorf("explorer: ping db: %w", err)
        }

        idx := &Indexer{db: db}
        if err := idx.migrate(ctx); err != nil {
                db.Close()
                return nil, fmt.Errorf("explorer: migrate: %w", err)
        }
        return idx, nil
}

// Close releases the database connection pool.
func (idx *Indexer) Close() error {
        return idx.db.Close()
}

// migrate creates all required tables if they do not exist yet.
// Idempotent — safe to call on every startup.
func (idx *Indexer) migrate(ctx context.Context) error {
        _, err := idx.db.ExecContext(ctx, `
                CREATE TABLE IF NOT EXISTS blocks (
                        height        INTEGER     PRIMARY KEY,
                        hash          TEXT        NOT NULL UNIQUE,
                        prev_hash     TEXT        NOT NULL,
                        timestamp_ns  BIGINT      NOT NULL,
                        validator_pub TEXT        NOT NULL,
                        merkle_root   TEXT        NOT NULL,
                        tx_count      INTEGER     NOT NULL DEFAULT 0,
                        indexed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
                );
                CREATE INDEX IF NOT EXISTS blocks_hash_idx   ON blocks (hash);
                CREATE INDEX IF NOT EXISTS blocks_height_idx ON blocks (height);

                CREATE TABLE IF NOT EXISTS transactions (
                        hash         TEXT    PRIMARY KEY,
                        block_hash   TEXT    NOT NULL,
                        block_height INTEGER NOT NULL,
                        tx_index     INTEGER NOT NULL,
                        is_coinbase  BOOLEAN NOT NULL DEFAULT FALSE,
                        inputs       INTEGER NOT NULL DEFAULT 0,
                        outputs      INTEGER NOT NULL DEFAULT 1,
                        fee          BIGINT  NOT NULL DEFAULT 0,
                        size_bytes   INTEGER NOT NULL DEFAULT 0,
                        version      INTEGER NOT NULL DEFAULT 1,
                        indexed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
                );
                CREATE INDEX IF NOT EXISTS txs_block_height_idx ON transactions (block_height);
                CREATE INDEX IF NOT EXISTS txs_block_hash_idx   ON transactions (block_hash);

                CREATE TABLE IF NOT EXISTS address_txs (
                        id           SERIAL  PRIMARY KEY,
                        address      TEXT    NOT NULL,
                        tx_hash      TEXT    NOT NULL,
                        block_height INTEGER NOT NULL,
                        tx_index     INTEGER NOT NULL DEFAULT 0,
                        output_index INTEGER NOT NULL DEFAULT 0
                );
                CREATE INDEX IF NOT EXISTS addr_txs_address_idx ON address_txs (address);
                CREATE INDEX IF NOT EXISTS addr_txs_tx_idx      ON address_txs (tx_hash);

                CREATE TABLE IF NOT EXISTS chain_stats (
                        id                   INTEGER PRIMARY KEY DEFAULT 1,
                        last_indexed_height  INTEGER NOT NULL DEFAULT -1,
                        total_txs            INTEGER NOT NULL DEFAULT 0,
                        tps_last_1min        REAL    NOT NULL DEFAULT 0,
                        tps_last_10min       REAL    NOT NULL DEFAULT 0,
                        tps_last_60min       REAL    NOT NULL DEFAULT 0,
                        updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
                );
                INSERT INTO chain_stats (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
        `)
        return err
}

// ─── IndexBlock ───────────────────────────────────────────────────────────────

// IndexBlock inserts a block and its transactions atomically.
// If the block already exists (same height) the call is a no-op for that block.
// addr allows optional address→tx mappings (may be nil/empty).
func (idx *Indexer) IndexBlock(block BlockData, txs []TxData, addr []AddrTxData) error {
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()

        dbTx, err := idx.db.BeginTx(ctx, nil)
        if err != nil {
                return fmt.Errorf("indexer: begin tx: %w", err)
        }
        defer dbTx.Rollback() //nolint:errcheck

        // Upsert block (skip duplicates by height)
        _, err = dbTx.ExecContext(ctx, `
                INSERT INTO blocks (height, hash, prev_hash, timestamp_ns, validator_pub, merkle_root, tx_count)
                VALUES ($1,$2,$3,$4,$5,$6,$7)
                ON CONFLICT (height) DO NOTHING`,
                int64(block.Height), block.Hash, block.PrevHash,
                block.TimestampNs, block.ValidatorPub, block.MerkleRoot, block.TxCount,
        )
        if err != nil {
                return fmt.Errorf("indexer: upsert block: %w", err)
        }

        // Insert transactions
        for _, tx := range txs {
                _, err = dbTx.ExecContext(ctx, `
                        INSERT INTO transactions (hash, block_hash, block_height, tx_index, is_coinbase, inputs, outputs, fee, size_bytes, version)
                        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
                        ON CONFLICT (hash) DO NOTHING`,
                        tx.Hash, tx.BlockHash, int64(tx.BlockHeight), tx.TxIndex,
                        tx.IsCoinbase, tx.Inputs, tx.Outputs, int64(tx.Fee), tx.SizeBytes, tx.Version,
                )
                if err != nil {
                        return fmt.Errorf("indexer: upsert tx %s: %w", tx.Hash, err)
                }
        }

        // Insert address→tx mappings
        for _, a := range addr {
                _, err = dbTx.ExecContext(ctx, `
                        INSERT INTO address_txs (address, tx_hash, block_height, tx_index, output_index)
                        VALUES ($1,$2,$3,$4,$5)`,
                        a.Address, a.TxHash, int64(a.BlockHeight), a.TxIndex, a.OutputIndex,
                )
                if err != nil {
                        return fmt.Errorf("indexer: insert addr_tx: %w", err)
                }
        }

        // Update chain stats
        var totalTxs int
        if err := dbTx.QueryRowContext(ctx,
                `SELECT COALESCE(SUM(tx_count),0) FROM blocks WHERE height <= $1`,
                int64(block.Height),
        ).Scan(&totalTxs); err != nil {
                return fmt.Errorf("indexer: count txs: %w", err)
        }

        _, err = dbTx.ExecContext(ctx, `
                UPDATE chain_stats SET
                        last_indexed_height = $1,
                        total_txs           = $2,
                        updated_at          = NOW()
                WHERE id = 1`,
                int64(block.Height), totalTxs,
        )
        if err != nil {
                return fmt.Errorf("indexer: update stats: %w", err)
        }

        return dbTx.Commit()
}

// ─── ResyncFromHeight ─────────────────────────────────────────────────────────

// FetchFn is a callback that returns the block and its transactions for a given
// height.  Return a nil BlockData pointer to signal the end of the chain.
type FetchFn func(height uint64) (*BlockData, []TxData, []AddrTxData, error)

// ResyncFromHeight re-indexes every block starting at startHeight using fetch.
// Safe to call when blocks are already indexed — duplicates are skipped.
func (idx *Indexer) ResyncFromHeight(startHeight uint64, fetch FetchFn) error {
        for h := startHeight; ; h++ {
                block, txs, addr, err := fetch(h)
                if err != nil {
                        return fmt.Errorf("indexer: fetch height %d: %w", h, err)
                }
                if block == nil {
                        break // reached tip
                }
                if err := idx.IndexBlock(*block, txs, addr); err != nil {
                        return fmt.Errorf("indexer: index height %d: %w", h, err)
                }
        }
        return nil
}

// ─── Query helpers ────────────────────────────────────────────────────────────

// BlockRow is the DB row for a block.
type BlockRow struct {
        Height       int64
        Hash         string
        PrevHash     string
        TimestampNs  int64
        ValidatorPub string
        MerkleRoot   string
        TxCount      int
}

const blockSelectCols = `height, hash, prev_hash, timestamp_ns, validator_pub, merkle_root, tx_count`

func scanBlock(row *sql.Row) (*BlockRow, error) {
        r := &BlockRow{}
        err := row.Scan(&r.Height, &r.Hash, &r.PrevHash, &r.TimestampNs, &r.ValidatorPub, &r.MerkleRoot, &r.TxCount)
        if err == sql.ErrNoRows {
                return nil, nil
        }
        return r, err
}

// GetBlockByHeight retrieves a block by its height.  Returns nil if not found.
func (idx *Indexer) GetBlockByHeight(height int64) (*BlockRow, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        return scanBlock(idx.db.QueryRowContext(ctx,
                `SELECT `+blockSelectCols+` FROM blocks WHERE height=$1`, height))
}

// GetBlockByHash retrieves a block by its hex hash.  Returns nil if not found.
func (idx *Indexer) GetBlockByHash(hash string) (*BlockRow, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        return scanBlock(idx.db.QueryRowContext(ctx,
                `SELECT `+blockSelectCols+` FROM blocks WHERE hash=$1`, hash))
}

// ListBlocks returns blocks ordered newest-first with limit/offset pagination.
// Also returns the total count of indexed blocks.
func (idx *Indexer) ListBlocks(limit, offset int) ([]*BlockRow, int, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        var total int
        if err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks`).Scan(&total); err != nil {
                return nil, 0, err
        }
        if total == 0 {
                return nil, 0, nil
        }

        rows, err := idx.db.QueryContext(ctx,
                `SELECT `+blockSelectCols+` FROM blocks ORDER BY height DESC LIMIT $1 OFFSET $2`,
                limit, offset)
        if err != nil {
                return nil, 0, err
        }
        defer rows.Close()

        var result []*BlockRow
        for rows.Next() {
                r := &BlockRow{}
                if err := rows.Scan(&r.Height, &r.Hash, &r.PrevHash, &r.TimestampNs,
                        &r.ValidatorPub, &r.MerkleRoot, &r.TxCount); err != nil {
                        return nil, 0, err
                }
                result = append(result, r)
        }
        return result, total, rows.Err()
}

// TxRow is the DB row for a transaction.
type TxRow struct {
        Hash        string
        BlockHash   string
        BlockHeight int64
        TxIndex     int
        IsCoinbase  bool
        Inputs      int
        Outputs     int
        Fee         int64
        SizeBytes   int
        Version     int
}

// GetTxByHash retrieves a transaction by its hex hash.  Returns nil if not found.
func (idx *Indexer) GetTxByHash(hash string) (*TxRow, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        r := &TxRow{}
        err := idx.db.QueryRowContext(ctx,
                `SELECT hash, block_hash, block_height, tx_index, is_coinbase, inputs, outputs, fee, size_bytes, version
                 FROM transactions WHERE hash=$1`, hash,
        ).Scan(&r.Hash, &r.BlockHash, &r.BlockHeight, &r.TxIndex, &r.IsCoinbase,
                &r.Inputs, &r.Outputs, &r.Fee, &r.SizeBytes, &r.Version)
        if err == sql.ErrNoRows {
                return nil, nil
        }
        return r, err
}

// GetAddrTxs retrieves transactions for an address, newest-first.
func (idx *Indexer) GetAddrTxs(address string, limit, offset int) ([]AddrTxData, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        rows, err := idx.db.QueryContext(ctx,
                `SELECT address, tx_hash, block_height, tx_index, output_index
                 FROM address_txs WHERE address=$1
                 ORDER BY block_height DESC LIMIT $2 OFFSET $3`,
                address, limit, offset)
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        var result []AddrTxData
        for rows.Next() {
                var a AddrTxData
                var h int64
                if err := rows.Scan(&a.Address, &a.TxHash, &h, &a.TxIndex, &a.OutputIndex); err != nil {
                        return nil, err
                }
                a.BlockHeight = uint64(h)
                result = append(result, a)
        }
        return result, rows.Err()
}

// ─── Stats ────────────────────────────────────────────────────────────────────

// StatsRow is the DB row for chain statistics.
type StatsRow struct {
        LastIndexedHeight int64
        TotalTxs          int
        TpsLast1Min       float64
        TpsLast10Min      float64
        TpsLast60Min      float64
        UpdatedAt         time.Time
}

// GetStats returns the latest chain stats.
func (idx *Indexer) GetStats() (*StatsRow, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        r := &StatsRow{}
        err := idx.db.QueryRowContext(ctx,
                `SELECT last_indexed_height, total_txs, tps_last_1min, tps_last_10min, tps_last_60min, updated_at
                 FROM chain_stats WHERE id=1`,
        ).Scan(&r.LastIndexedHeight, &r.TotalTxs, &r.TpsLast1Min, &r.TpsLast10Min, &r.TpsLast60Min, &r.UpdatedAt)
        if err != nil {
                return nil, err
        }
        return r, nil
}

// UpdateTPS recalculates TPS over the last 1/10/60 minutes and persists it.
func (idx *Indexer) UpdateTPS() error {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()

        calcTPS := func(minutes int) (float64, error) {
                var count int
                err := idx.db.QueryRowContext(ctx, `
                        SELECT COALESCE(SUM(tx_count),0)
                        FROM blocks
                        WHERE indexed_at > NOW() - ($1 * interval '1 minute')`, minutes,
                ).Scan(&count)
                if err != nil {
                        return 0, err
                }
                secs := float64(minutes * 60)
                if secs == 0 || count == 0 {
                        return 0, nil
                }
                return float64(count) / secs, nil
        }

        tps1, err := calcTPS(1)
        if err != nil {
                return err
        }
        tps10, err := calcTPS(10)
        if err != nil {
                return err
        }
        tps60, err := calcTPS(60)
        if err != nil {
                return err
        }

        _, err = idx.db.ExecContext(ctx, `
                UPDATE chain_stats SET
                        tps_last_1min  = $1,
                        tps_last_10min = $2,
                        tps_last_60min = $3,
                        updated_at     = NOW()
                WHERE id = 1`, tps1, tps10, tps60)
        return err
}
