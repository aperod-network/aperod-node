package explorer_test

import (
        "fmt"
        "os"
        "testing"

        "github.com/aperod/aperod/explorer"
)

// Integration tests run only when DATABASE_URL is set.
// Unit tests run always and test data-type contracts.

func testIndexer(t *testing.T) *explorer.Indexer {
        t.Helper()
        connStr := os.Getenv("DATABASE_URL")
        if connStr == "" {
                t.Skip("DATABASE_URL not set — skipping DB integration test")
        }
        idx, err := explorer.New(connStr)
        if err != nil {
                t.Fatalf("explorer.New: %v", err)
        }
        t.Cleanup(func() { idx.Close() })
        return idx
}

// uniqueHex generates a 64-char hex string that is unique per (height, salt).
func uniqueHex(height uint64, salt int) string {
        // Encode height and salt into the hash so each block/tx has a distinct hash.
        v := height*1_000_000 + uint64(salt)
        s := fmt.Sprintf("%016x%016x%016x%016x", v, v^0xdeadbeefcafebabe, v*3+7, v^0x0102030405060708)
        return s[:64]
}

func sampleBlock(height uint64) explorer.BlockData {
        return explorer.BlockData{
                Height:       height,
                Hash:         uniqueHex(height, 0),
                PrevHash:     uniqueHex(height, 1),
                TimestampNs:  int64(height)*3_000_000_000 + 1_700_000_000_000_000_000,
                ValidatorPub: "a3f9" + uniqueHex(height, 2)[:60],
                MerkleRoot:   uniqueHex(height, 3),
                TxCount:      int(height%5) + 1,
        }
}

func sampleTx(blockHash string, blockHeight uint64, idx int, coinbase bool) explorer.TxData {
        return explorer.TxData{
                Hash:        uniqueHex(blockHeight, 100+idx),
                BlockHash:   blockHash,
                BlockHeight: blockHeight,
                TxIndex:     idx,
                IsCoinbase:  coinbase,
                Inputs:      map[bool]int{true: 0, false: 11}[coinbase],
                Outputs:     1,
                Fee:         map[bool]uint64{true: 0, false: 1000}[coinbase],
                SizeBytes:   256,
                Version:     1,
        }
}

// TestBlockData_TypeContracts verifies that BlockData and TxData compile with
// all expected fields — no DB required.
func TestBlockData_TypeContracts(t *testing.T) {
        b := sampleBlock(0)
        if b.Height != 0 {
                t.Errorf("height mismatch: got %d", b.Height)
        }
        tx := sampleTx(b.Hash, b.Height, 0, true)
        if !tx.IsCoinbase {
                t.Error("expected coinbase")
        }
        if tx.Fee != 0 {
                t.Errorf("coinbase fee should be 0, got %d", tx.Fee)
        }
}

func TestAddrTxData_TypeContracts(t *testing.T) {
        a := explorer.AddrTxData{
                Address:     "APR" + "x" + "y" + "z",
                TxHash:      "ab" + "cd",
                BlockHeight: 5,
                TxIndex:     0,
                OutputIndex: 0,
        }
        if a.BlockHeight != 5 {
                t.Errorf("block height: got %d", a.BlockHeight)
        }
}

// ─── Integration tests (require DATABASE_URL) ─────────────────────────────────

func TestIndexer_New(t *testing.T) {
        idx := testIndexer(t)
        _ = idx
}

func TestIndexer_IndexBlock_Basic(t *testing.T) {
        idx := testIndexer(t)

        b := sampleBlock(999_001)
        txs := []explorer.TxData{
                sampleTx(b.Hash, b.Height, 0, true),
                sampleTx(b.Hash, b.Height, 1, false),
        }

        if err := idx.IndexBlock(b, txs, nil); err != nil {
                t.Fatalf("IndexBlock: %v", err)
        }

        // Retrieve by height
        row, err := idx.GetBlockByHeight(int64(b.Height))
        if err != nil {
                t.Fatalf("GetBlockByHeight: %v", err)
        }
        if row == nil {
                t.Fatal("block not found after indexing")
        }
        if row.Hash != b.Hash {
                t.Errorf("hash mismatch: got %s want %s", row.Hash, b.Hash)
        }
        if row.TxCount != b.TxCount {
                t.Errorf("tx_count: got %d want %d", row.TxCount, b.TxCount)
        }

        // Retrieve by hash
        row2, err := idx.GetBlockByHash(b.Hash)
        if err != nil {
                t.Fatalf("GetBlockByHash: %v", err)
        }
        if row2 == nil || row2.Height != row.Height {
                t.Error("GetBlockByHash returned wrong result")
        }
}

func TestIndexer_IndexBlock_Idempotent(t *testing.T) {
        idx := testIndexer(t)

        b := sampleBlock(999_002)
        if err := idx.IndexBlock(b, nil, nil); err != nil {
                t.Fatalf("first IndexBlock: %v", err)
        }
        // Second call must not return an error (ON CONFLICT DO NOTHING)
        if err := idx.IndexBlock(b, nil, nil); err != nil {
                t.Fatalf("second IndexBlock (idempotent): %v", err)
        }
}

func TestIndexer_GetTxByHash(t *testing.T) {
        idx := testIndexer(t)

        b := sampleBlock(999_003)
        tx := sampleTx(b.Hash, b.Height, 0, false)
        if err := idx.IndexBlock(b, []explorer.TxData{tx}, nil); err != nil {
                t.Fatalf("IndexBlock: %v", err)
        }

        row, err := idx.GetTxByHash(tx.Hash)
        if err != nil {
                t.Fatalf("GetTxByHash: %v", err)
        }
        if row == nil {
                t.Fatal("tx not found after indexing")
        }
        if row.BlockHeight != int64(b.Height) {
                t.Errorf("block_height: got %d want %d", row.BlockHeight, b.Height)
        }
        if row.Fee != int64(tx.Fee) {
                t.Errorf("fee: got %d want %d", row.Fee, tx.Fee)
        }
}

func TestIndexer_GetAddrTxs(t *testing.T) {
        idx := testIndexer(t)

        b := sampleBlock(999_004)
        tx := sampleTx(b.Hash, b.Height, 0, false)
        addr := []explorer.AddrTxData{
                {Address: "APRtest999004", TxHash: tx.Hash, BlockHeight: b.Height},
        }
        if err := idx.IndexBlock(b, []explorer.TxData{tx}, addr); err != nil {
                t.Fatalf("IndexBlock: %v", err)
        }

        results, err := idx.GetAddrTxs("APRtest999004", 10, 0)
        if err != nil {
                t.Fatalf("GetAddrTxs: %v", err)
        }
        if len(results) == 0 {
                t.Fatal("no addr_txs found")
        }
        if results[0].TxHash != tx.Hash {
                t.Errorf("tx_hash mismatch: got %s", results[0].TxHash)
        }
}

func TestIndexer_ListBlocks(t *testing.T) {
        idx := testIndexer(t)

        // Index two blocks
        for _, h := range []uint64{999_010, 999_011} {
                b := sampleBlock(h)
                if err := idx.IndexBlock(b, nil, nil); err != nil {
                        t.Fatalf("IndexBlock(%d): %v", h, err)
                }
        }

        blocks, total, err := idx.ListBlocks(5, 0)
        if err != nil {
                t.Fatalf("ListBlocks: %v", err)
        }
        if total < 2 {
                t.Errorf("total: got %d, expected >= 2", total)
        }
        if len(blocks) == 0 {
                t.Error("empty blocks list")
        }
        // Blocks must be newest-first
        for i := 1; i < len(blocks); i++ {
                if blocks[i].Height > blocks[i-1].Height {
                        t.Errorf("blocks not ordered newest-first at index %d", i)
                }
        }
}

func TestIndexer_GetStats(t *testing.T) {
        idx := testIndexer(t)

        stats, err := idx.GetStats()
        if err != nil {
                t.Fatalf("GetStats: %v", err)
        }
        if stats == nil {
                t.Fatal("nil stats")
        }
}

func TestIndexer_UpdateTPS(t *testing.T) {
        idx := testIndexer(t)

        // Index a block now so there are recent txs
        b := sampleBlock(999_020)
        b.TxCount = 5
        if err := idx.IndexBlock(b, nil, nil); err != nil {
                t.Fatalf("IndexBlock: %v", err)
        }

        if err := idx.UpdateTPS(); err != nil {
                t.Fatalf("UpdateTPS: %v", err)
        }

        stats, err := idx.GetStats()
        if err != nil {
                t.Fatalf("GetStats after UpdateTPS: %v", err)
        }
        _ = stats // TPS values depend on DB state; just ensure no error
}

func TestIndexer_ResyncFromHeight(t *testing.T) {
        idx := testIndexer(t)

        chain := []explorer.BlockData{
                sampleBlock(999_100),
                sampleBlock(999_101),
                sampleBlock(999_102),
        }

        i := 0
        fetch := func(h uint64) (*explorer.BlockData, []explorer.TxData, []explorer.AddrTxData, error) {
                if i >= len(chain) {
                        return nil, nil, nil, nil
                }
                b := chain[i]
                i++
                return &b, nil, nil, nil
        }

        if err := idx.ResyncFromHeight(999_100, fetch); err != nil {
                t.Fatalf("ResyncFromHeight: %v", err)
        }

        for _, b := range chain {
                row, err := idx.GetBlockByHeight(int64(b.Height))
                if err != nil {
                        t.Fatalf("GetBlockByHeight(%d): %v", b.Height, err)
                }
                if row == nil {
                        t.Errorf("block %d not found after resync", b.Height)
                }
        }
}
