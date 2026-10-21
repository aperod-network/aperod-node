package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const (
	maxAddressHistoryScanBlocks      = 1_000
	maxAddressHistoryBlockBytes      = 8 << 20
	maxAddressHistoryPageBytes       = 64 << 20
	maxConcurrentAddressHistoryScans = 2
)

var addressHistoryScanSlots = make(chan struct{}, maxConcurrentAddressHistoryScans)

func tryAcquireAddressHistoryScanSlot() bool {
	select {
	case addressHistoryScanSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseAddressHistoryScanSlot() {
	<-addressHistoryScanSlots
}

type addressHistoryQuery struct {
	limit        int
	startHeight  *uint64
	anchorHeight *uint64
	anchorHash   *crypto.Hash32
}

func parseAddressHistoryQuery(r *http.Request) (addressHistoryQuery, error) {
	query := addressHistoryQuery{limit: 50}
	values := r.URL.Query()

	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return query, fmt.Errorf("limit must be between 1 and 200")
		}
		query.limit = limit
	}
	if raw := values.Get("start_height"); raw != "" {
		height, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return query, fmt.Errorf("invalid start_height")
		}
		query.startHeight = &height
	}

	rawAnchorHeight := values.Get("anchor_height")
	rawAnchorHash := values.Get("anchor_hash")
	if (rawAnchorHeight == "") != (rawAnchorHash == "") {
		return query, fmt.Errorf("anchor_height and anchor_hash must be provided together")
	}
	if rawAnchorHeight != "" {
		height, err := strconv.ParseUint(rawAnchorHeight, 10, 64)
		if err != nil {
			return query, fmt.Errorf("invalid anchor_height")
		}
		hashBytes, err := hex.DecodeString(rawAnchorHash)
		if err != nil || len(hashBytes) != len(crypto.Hash32{}) {
			return query, fmt.Errorf("invalid anchor_hash")
		}
		var hash crypto.Hash32
		copy(hash[:], hashBytes)
		query.anchorHeight = &height
		query.anchorHash = &hash
	}
	return query, nil
}

func (s *Server) restAddressTransactionsPage(w http.ResponseWriter, r *http.Request, addr string, spendPub crypto.Point32) {
	query, err := parseAddressHistoryQuery(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !tryAcquireAddressHistoryScanSlot() {
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "address history scan capacity reached; retry shortly")
		return
	}
	defer releaseAddressHistoryScanSlot()

	if s.blockStore != nil {
		s.restAddressTransactionsFromStore(w, addr, spendPub, query)
		return
	}
	s.restAddressTransactionsFromMemory(w, addr, spendPub, query)
}

func (s *Server) restAddressTransactionsFromStore(w http.ResponseWriter, addr string, spendPub crypto.Point32, query addressHistoryQuery) {
	read, err := s.blockStore.NewWalletReadSnapshot()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "canonical history snapshot unavailable")
		return
	}
	defer read.Release()

	tipHash, tipHeight, err := read.GetTip()
	if err != nil || tipHash == (crypto.Hash32{}) {
		writeJSONError(w, http.StatusServiceUnavailable, "canonical history tip unavailable")
		return
	}
	anchorHeight, anchorHash := tipHeight, tipHash
	if query.anchorHeight == nil {
		canonicalHash, found, hashErr := read.GetCanonicalHash(tipHeight)
		if hashErr != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "canonical history tip lookup failed")
			return
		}
		if !found || canonicalHash != tipHash {
			writeJSONError(w, http.StatusServiceUnavailable, "canonical history tip metadata is inconsistent")
			return
		}
	}
	if query.anchorHeight != nil {
		anchorHeight, anchorHash = *query.anchorHeight, *query.anchorHash
		if anchorHeight > tipHeight {
			writeJSONError(w, http.StatusConflict, "history anchor is above the canonical tip")
			return
		}
		canonicalHash, found, hashErr := read.GetCanonicalHash(anchorHeight)
		if hashErr != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "canonical history anchor lookup failed")
			return
		}
		if !found || canonicalHash != anchorHash {
			writeJSONError(w, http.StatusConflict, "history anchor is no longer canonical; restart pagination")
			return
		}
	}

	startHeight := anchorHeight
	if query.startHeight != nil {
		startHeight = *query.startHeight
		if startHeight > anchorHeight {
			writeJSONError(w, http.StatusBadRequest, "start_height must not exceed anchor_height")
			return
		}
	}

	page := scanAddressHistoryBlocks(startHeight, spendPub, query.limit, func(height uint64) (*core.Block, int, error) {
		return read.ReadCanonicalBlockBoundedWithSize(height, maxAddressHistoryBlockBytes)
	}, func(height uint64) (bool, error) {
		_, found, hashErr := read.GetCanonicalHash(height)
		if hashErr == nil && !found {
			return false, fmt.Errorf("missing canonical history height %d", height)
		}
		return found, hashErr
	}, pageEndForHeight(startHeight))
	if page.err != nil {
		s.log.Warn("address history canonical page read failed", "address", addr, "error", page.err)
		writeJSONError(w, http.StatusServiceUnavailable, "canonical address history page unavailable")
		return
	}

	response := map[string]interface{}{
		"address":       addr,
		"transactions":  page.transactions,
		"scan_limited":  page.scanLimited,
		"has_more":      page.hasMore,
		"anchor_height": anchorHeight,
		"anchor_hash":   fmt.Sprintf("%x", anchorHash[:]),
		"history_scope": "canonical_public_key_outputs",
		"note":          "Only public-key-matched outputs are listed; stealth outputs require wallet-local view-key scanning.",
	}
	if page.hasMore {
		response["next_start_height"] = page.nextStartHeight
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) restAddressTransactionsFromMemory(w http.ResponseWriter, addr string, spendPub crypto.Point32, query addressHistoryQuery) {
	tip := s.chain.Tip()
	if tip == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "canonical history tip unavailable")
		return
	}
	anchorHeight, anchorHash := tip.Header.Height, tip.Hash()
	if query.anchorHeight != nil {
		anchorHeight, anchorHash = *query.anchorHeight, *query.anchorHash
		canonical := s.chain.GetByHeight(anchorHeight)
		if canonical == nil || canonical.Hash() != anchorHash {
			writeJSONError(w, http.StatusConflict, "history anchor is no longer retained; restart pagination")
			return
		}
	}

	startHeight := anchorHeight
	if query.startHeight != nil {
		startHeight = *query.startHeight
		if startHeight > anchorHeight {
			writeJSONError(w, http.StatusBadRequest, "start_height must not exceed anchor_height")
			return
		}
	}
	blocks, nextStart, pageHasMore, oldest := s.chain.BlocksNewestFirstPage(startHeight, false, maxAddressHistoryScanBlocks)
	if len(blocks) == 0 && startHeight < oldest {
		writeJSONError(w, http.StatusServiceUnavailable, "historical block body is outside the in-memory window")
		return
	}
	if len(blocks) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "address history block is not retained")
		return
	}
	pageEnd := blocks[len(blocks)-1].Header.Height

	page := scanAddressHistoryBlocks(startHeight, spendPub, query.limit, func(height uint64) (*core.Block, int, error) {
		block := s.chain.GetByHeight(height)
		if block == nil {
			return nil, 0, fmt.Errorf("block %d is not retained in memory", height)
		}
		raw, marshalErr := json.Marshal(block)
		if marshalErr != nil {
			return nil, 0, marshalErr
		}
		return block, len(raw), nil
	}, func(height uint64) (bool, error) {
		return s.chain.GetByHeight(height) != nil, nil
	}, pageEnd)
	if page.err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "in-memory address history page unavailable")
		return
	}
	if !page.hasMore && pageHasMore {
		page.hasMore = true
		page.nextStartHeight = nextStart
		page.scanLimited = true
	}
	if !page.hasMore && oldest > 0 && startHeight >= oldest {
		page.historyWindowEnded = true
	}

	response := map[string]interface{}{
		"address":              addr,
		"transactions":         page.transactions,
		"scan_limited":         page.scanLimited,
		"has_more":             page.hasMore,
		"anchor_height":        anchorHeight,
		"anchor_hash":          fmt.Sprintf("%x", anchorHash[:]),
		"history_scope":        "in_memory_public_key_outputs",
		"history_window_ended": page.historyWindowEnded,
		"note":                 "Only public-key-matched outputs in the in-memory block window are available; stealth outputs require wallet-local view-key scanning.",
	}
	if page.hasMore {
		response["next_start_height"] = page.nextStartHeight
	}
	writeJSON(w, http.StatusOK, response)
}

type addressHistoryPage struct {
	transactions       []AddressTx
	nextStartHeight    uint64
	hasMore            bool
	scanLimited        bool
	historyWindowEnded bool
	err                error
}

func scanAddressHistoryBlocks(
	startHeight uint64,
	spendPub crypto.Point32,
	limit int,
	readBlock func(uint64) (*core.Block, int, error),
	heightExists func(uint64) (bool, error),
	pageEnd uint64,
) addressHistoryPage {
	page := addressHistoryPage{transactions: make([]AddressTx, 0, 16)}
	pageBytes := 0
	scannedBlocks := 0

	for height := startHeight; ; height-- {
		block, bodyBytes, err := readBlock(height)
		if err != nil {
			page.err = err
			return page
		}
		if bodyBytes < 0 || bodyBytes > maxAddressHistoryBlockBytes ||
			bodyBytes > maxAddressHistoryPageBytes-pageBytes {
			page.err = fmt.Errorf("address history page decode budget exceeded")
			return page
		}
		pageBytes += bodyBytes
		scannedBlocks++

		if block.Header.Height != height || block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
			page.err = fmt.Errorf("canonical block body mismatch at height %d", height)
			return page
		}
		heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(height))
		if err != nil {
			page.err = fmt.Errorf("derive mint pub at height %d: %w", height, err)
			return page
		}
		expectedMintPub, err := crypto.AddPoints(spendPub, heightPub)
		if err != nil {
			page.err = fmt.Errorf("derive expected mint pub at height %d: %w", height, err)
			return page
		}

		for txIndex, tx := range block.Txs {
			txHash := tx.Hash()
			for outputIndex, output := range tx.Outputs {
				if output.OneTimePub == spendPub || output.OneTimePub == expectedMintPub {
					page.transactions = append(page.transactions, AddressTx{
						TxHash:      fmt.Sprintf("%x", txHash[:]),
						BlockHeight: height,
						TxIndex:     txIndex,
						OutputIndex: outputIndex,
						IsCoinbase:  tx.IsCoinbase(),
					})
				}
			}
		}

		if len(page.transactions) >= limit {
			if height > 0 {
				exists, existsErr := heightExists(height - 1)
				if existsErr != nil {
					page.err = existsErr
					return page
				}
				if exists {
					page.hasMore = true
					page.nextStartHeight = height - 1
				}
			}
			return page
		}

		if height == pageEnd || scannedBlocks >= maxAddressHistoryScanBlocks || height == 0 {
			if height > 0 {
				exists, existsErr := heightExists(height - 1)
				if existsErr != nil {
					page.err = existsErr
					return page
				}
				if exists {
					page.hasMore = true
					page.scanLimited = true
					page.nextStartHeight = height - 1
				}
			}
			return page
		}
	}
}

func pageEndForHeight(startHeight uint64) uint64 {
	if startHeight >= maxAddressHistoryScanBlocks {
		return startHeight - maxAddressHistoryScanBlocks + 1
	}
	return 0
}
