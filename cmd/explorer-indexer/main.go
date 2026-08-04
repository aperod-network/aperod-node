// Command explorer-indexer is a standalone process that indexes Aperod
// blockchain data into PostgreSQL by polling the Go node REST API.
//
// Environment variables:
//
//	DATABASE_URL  — PostgreSQL connection string (required)
//	NODE_API_URL  — Go node REST base URL (default: http://127.0.0.1:8545)
//	POLL_INTERVAL — seconds between tip polls after catch-up (default: 5)
//	BATCH_SIZE    — blocks per batch during initial catch-up (default: 50)
//
// On startup the indexer reads chain_stats.last_indexed_height to resume where
// it left off.  After the initial catch-up it polls for new blocks at the
// configured interval.  All block indexing is idempotent — restarting the
// process is always safe.
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

	nodeURL := os.Getenv("NODE_API_URL")
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

	log.Info("explorer-indexer starting",
		"node_url", nodeURL,
		"poll_interval", pollInterval,
		"batch_size", batchSize,
	)

	// ── Connect to PostgreSQL ──────────────────────────────────────────────────
	idx, err := explorer.New(dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer idx.Close()
	log.Info("connected to PostgreSQL")

	client := &nodeClient{base: nodeURL, http: &http.Client{Timeout: 30 * time.Second}}

	// ── Initial catch-up ───────────────────────────────────────────────────────
	// Resume from where we left off.  Retries indefinitely on node-unavailable.
	if err := catchUp(idx, client, batchSize, log); err != nil {
		// catchUp only returns an unrecoverable error (not transient node errors).
		return fmt.Errorf("initial catch-up failed: %w", err)
	}

	// ── Steady-state polling ───────────────────────────────────────────────────
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Update TPS stats on every other tick (every ~10 s at default interval).
	tpsTick := 0
	for range ticker.C {
		if err := catchUp(idx, client, batchSize, log); err != nil {
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
// node tip.  Returns nil when fully caught up, or when the node is temporarily
// unreachable (logs a warning and returns nil so the caller can retry).
func catchUp(idx *explorer.Indexer, client *nodeClient, batchSize uint64, log *slog.Logger) error {
	stats, err := idx.GetStats()
	if err != nil {
		return fmt.Errorf("get chain stats: %w", err)
	}

	startHeight := uint64(0)
	if stats.LastIndexedHeight >= 0 {
		startHeight = uint64(stats.LastIndexedHeight) + 1
	}

	// Ask the node for the current tip height.
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
				return nil // let the next tick retry from this height
			}
			indexed++
		}
		// Brief yield between batches to keep the connection pool healthy.
		if end < tipHeight {
			time.Sleep(20 * time.Millisecond)
		}
	}

	if indexed > 0 {
		log.Info("catch-up complete", "indexed", indexed, "tip", tipHeight)
	}
	return nil
}

// indexBlock fetches a single block and its transactions from the Go node and
// writes them to PostgreSQL via the Indexer.
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
		})
	}

	if err := idx.IndexBlock(bd, txs, nil); err != nil {
		return fmt.Errorf("index block %d: %w", height, err)
	}

	log.Debug("indexed block", "height", height, "txs", len(txs))
	return nil
}

// ─── Node REST client ─────────────────────────────────────────────────────────

type nodeClient struct {
	base string
	http *http.Client
}

// blockAPIResponse maps the Go node's GET /api/v1/blocks/{height} JSON.
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

// blockTxsAPIResponse maps the Go node's GET /api/v1/blocks/{height}/transactions JSON.
type blockTxsAPIResponse struct {
	BlockHash   string    `json:"block_hash"`
	BlockHeight uint64    `json:"block_height"`
	TxCount     int       `json:"tx_count"`
	Transactions []txItem `json:"transactions"`
}

type txItem struct {
	Hash       string `json:"hash"`
	TxIndex    int    `json:"tx_index"`
	IsCoinbase bool   `json:"is_coinbase"`
	Inputs     int    `json:"inputs"`
	Outputs    int    `json:"outputs"`
	Fee        uint64 `json:"fee"`
	Size       int    `json:"size"`
}

type blocksListResponse struct {
	Total uint64 `json:"total"`
}

func (c *nodeClient) getTipHeight() (uint64, error) {
	resp, err := c.http.Get(c.base + "/api/v1/blocks?limit=1")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("node returned %d", resp.StatusCode)
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
	url := fmt.Sprintf("%s/api/v1/blocks/%d", c.base, height)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // beyond tip
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node returned %d for block %d", resp.StatusCode, height)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r blockAPIResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse block %d: %w", height, err)
	}
	// Convert RFC3339 timestamp to nanoseconds.
	if r.TimestampRFC != "" {
		if t, err := time.Parse(time.RFC3339, r.TimestampRFC); err == nil {
			r.TimestampNs = t.UnixNano()
		}
	}
	return &r, nil
}

func (c *nodeClient) getBlockTxs(height uint64) (*blockTxsAPIResponse, error) {
	url := fmt.Sprintf("%s/api/v1/blocks/%d/transactions", c.base, height)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node returned %d for block %d txs", resp.StatusCode, height)
	}
	var r blockTxsAPIResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse block %d txs: %w", height, err)
	}
	return &r, nil
}
