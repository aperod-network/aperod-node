// Command explorer-indexer is a standalone process that indexes Aperod
// blockchain data into PostgreSQL by polling the Go node REST API.
//
// Environment variables:
//
//	DATABASE_URL  — PostgreSQL connection string (required)
//	GO_NODE_URL   — Go node REST base URL shared with aperod-api
//	NODE_API_URL  — legacy override for the Go node REST base URL
//	POLL_INTERVAL — seconds between tip polls after catch-up (default: 5)
//	BATCH_SIZE    — blocks per batch during initial catch-up (default: 50)
//	BACKFILL_FROM_HEIGHT — optional initial catch-up start; invalid values fail
//	  startup and this value is never used for steady-state polling
//
// Startup behaviour:
//   - Waits for the Go node to report ok:true and syncing:false before indexing.
//     This prevents indexing a partial window while the node replays its chain.
//   - Reads chain_stats.last_indexed_height to resume where it left off.
//   - After initial catch-up polls every POLL_INTERVAL seconds for new blocks.
//   - All indexing is idempotent — restarting is always safe.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aperod/aperod/explorer"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "explorer-indexer: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// ── Configuration ──────────────────────────────────────────────────────────
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	// GO_NODE_URL is the canonical shared deployment setting used by the API.
	// Keep NODE_API_URL as a backwards-compatible override for existing units.
	nodeURL := os.Getenv("NODE_API_URL")
	if nodeURL == "" {
		nodeURL = os.Getenv("GO_NODE_URL")
	}
	if nodeURL == "" {
		nodeURL = "http://127.0.0.1:8545"
	}

	pollInterval := 5 * time.Second
	if s := os.Getenv("POLL_INTERVAL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			pollInterval = time.Duration(n) * time.Second
		}
	}

	batchSize := uint64(50)
	if s := os.Getenv("BATCH_SIZE"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil && n > 0 {
			batchSize = n
		}
	}

	backfillFrom, err := backfillFromHeightFromEnv()
	if err != nil {
		return err
	}

	log.Info("explorer-indexer starting",
		"node_url", nodeURL,
		"poll_interval", pollInterval,
		"batch_size", batchSize,
	)
	if backfillFrom != nil {
		log.Info("initial catch-up override enabled; steady-state resumes chain_stats checkpoint",
			"backfill_from_height", *backfillFrom)
	}

	// ── Connect to PostgreSQL ──────────────────────────────────────────────────
	idx, err := explorer.New(dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer idx.Close()
	log.Info("connected to PostgreSQL")

	client := &nodeClient{base: nodeURL, http: &http.Client{Timeout: 30 * time.Second}}

	// ── Wait for Go node to be fully ready ────────────────────────────────────
	// The node REST API is reachable during startup replay/sync, so we must
	// wait until it reports ok:true AND syncing:false before indexing.
	// Otherwise we may record last_indexed_height against a partial chain window.
	log.Info("waiting for Go node to finish syncing…")
	for {
		ready, err := client.isNodeReady()
		if err != nil {
			log.Warn("node not reachable yet", "err", err)
		} else if ready {
			log.Info("Go node is ready — starting catch-up")
			break
		} else {
			log.Info("Go node still syncing — waiting")
		}
		time.Sleep(10 * time.Second)
	}

	// ── Initial catch-up ───────────────────────────────────────────────────────
	if err := catchUp(idx, client, batchSize, backfillFrom, log); err != nil {
		return fmt.Errorf("initial catch-up failed: %w", err)
	}

	// ── Steady-state polling ───────────────────────────────────────────────────
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	tpsTick := 0
	for range ticker.C {
		if err := catchUp(idx, client, batchSize, nil, log); err != nil {
			log.Error("catch-up error", "err", err)
		}

		tpsTick++
		if tpsTick%2 == 0 {
			if err := idx.UpdateTPS(); err != nil {
				log.Warn("tps update failed", "err", err)
			}
		}
	}
	return nil
}

// catchUp indexes all blocks from last_indexed_height+1 up to the current
// node tip.  Returns nil when the node is temporarily unreachable (logs warning
// and defers to the next tick) to avoid crashing the polling loop.
func backfillFromHeightFromEnv() (*uint64, error) {
	value, ok := os.LookupEnv("BACKFILL_FROM_HEIGHT")
	if !ok || value == "" {
		return nil, nil
	}
	height, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("BACKFILL_FROM_HEIGHT must be an unsigned integer: %w", err)
	}
	return &height, nil
}

func catchUp(idx *explorer.Indexer, client *nodeClient, batchSize uint64, initialStartOverride *uint64, log *slog.Logger) error {
	stats, err := idx.GetStats()
	if err != nil {
		return fmt.Errorf("get chain stats: %w", err)
	}

	startHeight := uint64(0)
	if stats.LastIndexedHeight >= 0 {
		startHeight = uint64(stats.LastIndexedHeight) + 1
	}
	if initialStartOverride != nil {
		startHeight = *initialStartOverride
	}

	tipHeight, err := client.getTipHeight()
	if err != nil {
		log.Warn("node unreachable — will retry on next tick", "err", err)
		return nil
	}

	if startHeight > tipHeight {
		return nil // fully caught up
	}

	blocksToIndex := tipHeight - startHeight + 1
	log.Info("indexer catch-up started",
		"from", startHeight, "to", tipHeight,
		"blocks", blocksToIndex,
	)

	indexed := uint64(0)
	for h := startHeight; h <= tipHeight; h += batchSize {
		end := h + batchSize - 1
		if end > tipHeight {
			end = tipHeight
		}
		for bh := h; bh <= end; bh++ {
			if err := indexBlock(idx, client, bh, log); err != nil {
				log.Warn("failed to index block — stopping batch",
					"height", bh, "err", err)
				return err
			}
			indexed++
		}
		if end < tipHeight {
			time.Sleep(20 * time.Millisecond)
		}
	}

	if indexed > 0 {
		log.Info("catch-up complete", "indexed", indexed, "tip", tipHeight)
	}
	return nil
}

// indexBlock fetches a block, its transactions, and its outputs from the Go node
// and writes them atomically to PostgreSQL.  address_txs rows are keyed by
// one_time_pub_hex — the canonical identifier for a transaction output that is
// derivable without a view key.
func indexBlock(idx *explorer.Indexer, client *nodeClient, height uint64, log *slog.Logger) error {
	blockResp, err := client.getBlock(height)
	if err != nil {
		return fmt.Errorf("get block %d: %w", height, err)
	}
	if blockResp == nil {
		return fmt.Errorf("block %d: node returned nil (beyond tip?)", height)
	}

	txResp, err := client.getBlockTxs(height)
	if err != nil {
		return fmt.Errorf("get txs for block %d: %w", height, err)
	}

	outResp, err := client.getBlockOutputs(height)
	if err != nil {
		return fmt.Errorf("get outputs for block %d: %w", height, err)
	}

	bd := explorer.BlockData{
		Height:       height,
		Hash:         blockResp.Hash,
		PrevHash:     blockResp.PrevHash,
		TimestampNs:  blockResp.TimestampNs,
		ValidatorPub: blockResp.ValidatorPub,
		MerkleRoot:   blockResp.MerkleRoot,
		TxCount:      len(txResp.Transactions),
	}

	blockHash := blockResp.Hash
	txs := make([]explorer.TxData, 0, len(txResp.Transactions))
	for _, t := range txResp.Transactions {
		txs = append(txs, explorer.TxData{
			Hash:        t.Hash,
			BlockHash:   blockHash,
			BlockHeight: height,
			TxIndex:     t.TxIndex,
			IsCoinbase:  t.IsCoinbase,
			Inputs:      t.Inputs,
			Outputs:     t.Outputs,
			Fee:         t.Fee,
			SizeBytes:   t.Size,
			Version:     1,
			IsBurn:      t.IsBurn,
			BurnedNAPRO: t.BurnedNAPRO,
			BurnAddress: t.BurnAddress,
		})
	}

	// Build address_txs rows from the output list.
	// one_time_pub_hex is used as the address key: it uniquely identifies the
	// output recipient and is stable across restarts.  For transparent outputs
	// (admin mints, coinbase) one_time_pub_hex == spend_pub_hex, so lookups by
	// the recipient's spend key will find these records.
	addr := make([]explorer.AddrTxData, 0, len(outResp.Outputs))
	for _, o := range outResp.Outputs {
		if o.OneTimePubHex == "" {
			continue
		}
		addr = append(addr, explorer.AddrTxData{
			Address:     o.OneTimePubHex,
			TxHash:      o.TxHash,
			BlockHeight: height,
			TxIndex:     o.TxIndex,
			OutputIndex: o.OutputIndex,
		})
	}

	if err := idx.IndexBlock(bd, txs, addr); err != nil {
		return fmt.Errorf("index block %d: %w", height, err)
	}

	log.Debug("indexed block", "height", height, "txs", len(txs), "outputs", len(addr))
	return nil
}

// ─── Node REST client ─────────────────────────────────────────────────────────

type nodeClient struct {
	base string
	http *http.Client
}

// nodeStatusResponse maps GET /api/v1/status.
type nodeStatusResponse struct {
	OK      bool  `json:"ok"`
	Syncing bool  `json:"syncing"`
	Height  int64 `json:"height"`
}

// blockAPIResponse maps GET /api/v1/blocks/{height}.
type blockAPIResponse struct {
	Hash         string `json:"hash"`
	Height       uint64 `json:"height"`
	PrevHash     string `json:"prev_hash"`
	TimestampRFC string `json:"timestamp"` // RFC3339 from the node
	TimestampNs  int64  `json:"-"`         // computed after parse
	ValidatorPub string `json:"validator_pub"`
	MerkleRoot   string `json:"merkle_root"`
	TxCount      int    `json:"tx_count"`
}

// blockTxsAPIResponse maps GET /api/v1/blocks/{height}/transactions.
type blockTxsAPIResponse struct {
	BlockHash    string   `json:"block_hash"`
	BlockHeight  uint64   `json:"block_height"`
	TxCount      int      `json:"tx_count"`
	Transactions []txItem `json:"transactions"`
}

type txItem struct {
	Hash        string `json:"hash"`
	TxIndex     int    `json:"tx_index"`
	IsCoinbase  bool   `json:"is_coinbase"`
	Inputs      int    `json:"inputs"`
	Outputs     int    `json:"outputs"`
	Fee         uint64 `json:"fee"`
	Size        int    `json:"size"`
	IsBurn      bool   `json:"is_burn"`
	BurnedNAPRO string `json:"burned_napro"`
	BurnAddress string `json:"burn_address"`
}

// blockOutputsAPIResponse maps GET /api/v1/blocks/{height}/outputs.
type blockOutputsAPIResponse struct {
	BlockHash   string       `json:"block_hash"`
	BlockHeight uint64       `json:"block_height"`
	OutputCount int          `json:"output_count"`
	Outputs     []outputItem `json:"outputs"`
}

type outputItem struct {
	TxHash        string `json:"tx_hash"`
	TxIndex       int    `json:"tx_index"`
	OutputIndex   int    `json:"output_index"`
	OneTimePubHex string `json:"one_time_pub_hex"`
	IsCoinbase    bool   `json:"is_coinbase"`
}

// blocksListResponse maps GET /api/v1/blocks?limit=1.
type blocksListResponse struct {
	Total uint64 `json:"total"`
}

func (c *nodeClient) get(url string) ([]byte, int, error) {
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func (c *nodeClient) isNodeReady() (bool, error) {
	body, status, err := c.get(c.base + "/api/v1/status")
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("status endpoint returned %d", status)
	}
	var r nodeStatusResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return false, fmt.Errorf("parse status: %w", err)
	}
	return r.OK && !r.Syncing, nil
}

func (c *nodeClient) getTipHeight() (uint64, error) {
	body, status, err := c.get(c.base + "/api/v1/blocks?limit=1")
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("node returned %d", status)
	}
	var r blocksListResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("parse blocks list: %w", err)
	}
	if r.Total == 0 {
		return 0, nil
	}
	return r.Total - 1, nil // total = tip+1
}

func (c *nodeClient) getBlock(height uint64) (*blockAPIResponse, error) {
	body, status, err := c.get(fmt.Sprintf("%s/api/v1/blocks/%d", c.base, height))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil // beyond tip
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("node returned %d for block %d", status, height)
	}
	var r blockAPIResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse block %d: %w", height, err)
	}
	if r.TimestampRFC != "" {
		if t, err := time.Parse(time.RFC3339, r.TimestampRFC); err == nil {
			r.TimestampNs = t.UnixNano()
		}
	}
	return &r, nil
}

func (c *nodeClient) getBlockTxs(height uint64) (*blockTxsAPIResponse, error) {
	body, status, err := c.get(fmt.Sprintf("%s/api/v1/blocks/%d/transactions", c.base, height))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("node returned %d for block %d txs", status, height)
	}
	var r blockTxsAPIResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse block %d txs: %w", height, err)
	}
	return &r, nil
}

func (c *nodeClient) getBlockOutputs(height uint64) (*blockOutputsAPIResponse, error) {
	body, status, err := c.get(fmt.Sprintf("%s/api/v1/blocks/%d/outputs", c.base, height))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// Pruned block — no outputs available; return empty list gracefully.
		return &blockOutputsAPIResponse{BlockHeight: height, Outputs: nil}, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("node returned %d for block %d outputs", status, height)
	}
	var r blockOutputsAPIResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse block %d outputs: %w", height, err)
	}
	return &r, nil
}
