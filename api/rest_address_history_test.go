package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestREST_AddressTxsPagesCanonicalDiskHistoryBeyondReportedHeight(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	priv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	targetKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	targetSpend := targetKeys.Spend.Public
	otherSpend := crypto.Point32(validatorPub)

	const firstHeight = uint64(1_171_635)
	const tipHeight = firstHeight + 2*maxTestAddressHistoryScanBlocks - 1
	const firstStoredHeight = firstHeight - 1
	var previousHash crypto.Hash32
	var tipParentHash crypto.Hash32
	var tipBlock *core.Block

	for height := firstStoredHeight; height <= tipHeight; height++ {
		recipient := otherSpend
		if height == firstHeight || height == tipHeight {
			recipient = targetSpend
		}
		txs := []core.Transaction{core.CoinbaseTx(recipient, 1_000_000+height)}
		header := core.BlockHeader{
			Height:       height,
			PrevHash:     previousHash,
			Timestamp:    time.Now().UnixNano() + int64(height),
			ValidatorPub: validatorPub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if err := header.Sign(priv); err != nil {
			t.Fatalf("Sign block %d: %v", height, err)
		}
		block := &core.Block{Header: header, Txs: txs}
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatalf("marshal block %d: %v", height, err)
		}
		hash := block.Hash()
		if err := db.PutRawBlock(hash, height, raw); err != nil {
			t.Fatalf("PutRawBlock(%d): %v", height, err)
		}
		if height == tipHeight-1 {
			tipParentHash = hash
		}
		previousHash = hash
		tipBlock = block
	}
	tipHash := tipBlock.Hash()
	if err := db.PutTip(tipHash, tipHeight); err != nil {
		t.Fatalf("PutTip: %v", err)
	}

	srv := api.NewServer(":0", core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()), core.NewUTXOSet(), testLogger())
	srv.SetStore(db)
	address := crypto.EncodeAddress(crypto.MainnetByte, targetSpend, targetSpend)
	path := "/api/v1/address/" + string(address) + "/transactions?limit=10"

	code, first := restGet(t, srv, path)
	if code != http.StatusOK {
		t.Fatalf("first page status = %d, want 200: %v", code, first)
	}
	if first["scan_limited"] != true || first["has_more"] != true {
		t.Fatalf("first page must expose its bounded continuation: %v", first)
	}
	firstRows, ok := first["transactions"].([]interface{})
	if !ok || len(firstRows) != 1 {
		t.Fatalf("first page transactions = %v, want one high-height match", first["transactions"])
	}
	firstRow, ok := firstRows[0].(map[string]interface{})
	if !ok || firstRow["block_height"] != float64(tipHeight) {
		t.Fatalf("first-page transaction = %v, want height %d", firstRows[0], tipHeight)
	}
	if first["anchor_height"] != float64(tipHeight) || first["anchor_hash"] != fmt.Sprintf("%x", tipHash[:]) {
		t.Fatalf("page anchor = (%v, %v), want (%d, %x)", first["anchor_height"], first["anchor_hash"], tipHeight, tipHash[:])
	}
	nextStartHeight := firstHeight + maxTestAddressHistoryScanBlocks - 1
	if first["next_start_height"] != float64(nextStartHeight) {
		t.Fatalf("next_start_height = %v, want %d", first["next_start_height"], nextStartHeight)
	}

	nextPath := fmt.Sprintf(
		"/api/v1/address/%s/transactions?limit=10&start_height=%d&anchor_height=%d&anchor_hash=%x",
		address, nextStartHeight, tipHeight, tipHash[:],
	)
	code, second := restGet(t, srv, nextPath)
	if code != http.StatusOK {
		t.Fatalf("second page status = %d, want 200: %v", code, second)
	}
	secondRows, ok := second["transactions"].([]interface{})
	if !ok || len(secondRows) != 1 {
		t.Fatalf("second-page transactions = %v, want one older match", second["transactions"])
	}
	secondRow, ok := secondRows[0].(map[string]interface{})
	if !ok || secondRow["block_height"] != float64(firstHeight) {
		t.Fatalf("second-page transaction = %v, want height %d", secondRows[0], firstHeight)
	}
	if second["has_more"] != true || second["scan_limited"] != true ||
		second["next_start_height"] != float64(firstStoredHeight) {
		t.Fatalf("second-page continuation metadata = %v, want next height %d", second, firstStoredHeight)
	}

	replacementHeader := tipBlock.Header
	replacementHeader.Timestamp++
	replacementHeader.PrevHash = tipParentHash
	if err := replacementHeader.Sign(priv); err != nil {
		t.Fatalf("sign replacement tip: %v", err)
	}
	replacement := &core.Block{Header: replacementHeader, Txs: tipBlock.Txs}
	replacementRaw, err := json.Marshal(replacement)
	if err != nil {
		t.Fatalf("marshal replacement tip: %v", err)
	}
	replacementHash := replacement.Hash()
	if err := db.PutRawBlock(replacementHash, tipHeight, replacementRaw); err != nil {
		t.Fatalf("PutRawBlock replacement: %v", err)
	}
	if err := db.PutTip(replacementHash, tipHeight); err != nil {
		t.Fatalf("PutTip replacement: %v", err)
	}
	if code, stale := restGet(t, srv, nextPath); code != http.StatusConflict {
		t.Fatalf("stale anchor status = %d, want 409: %v", code, stale)
	}
}

const maxTestAddressHistoryScanBlocks = uint64(1_000)
