package api

import (
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// A UTXO/index hit alone is not evidence that its recorded transaction hash
// occurs in a canonical block. Pruned/missing bodies deliberately yield false.
// Queries use the SAME pinned view as the owner's key-image status.
func walletHistoryOutputProof(read *store.WalletReadSnapshot, refs []struct {
	Hash  string `json:"tx_hash"`
	Index uint32 `json:"out_idx"`
}) map[string]bool {
	evidence := walletHistoryOutputEvidence(read, refs)
	result := make(map[string]bool, len(refs))
	for id, proof := range evidence {
		result[id] = proof != nil
	}
	return result
}

type walletCanonicalEvidence struct {
	Height    uint64 `json:"block_height"`
	BlockHash string `json:"block_hash"`
	Fee       string `json:"fee_napro"`
	Origin    string `json:"origin"`
}

func walletHistoryOutputEvidence(read *store.WalletReadSnapshot, refs []struct {
	Hash  string `json:"tx_hash"`
	Index uint32 `json:"out_idx"`
}) map[string]*walletCanonicalEvidence {
	result := make(map[string]*walletCanonicalEvidence, len(refs))
	blocks := make(map[uint64]*core.Block)
	budget := 8 << 20
	for _, ref := range refs {
		id := fmt.Sprintf("%s:%d", ref.Hash, ref.Index)
		result[id] = nil
		raw, err := hex.DecodeString(ref.Hash)
		if err != nil || len(raw) != 32 {
			continue
		}
		var hash crypto.Hash32
		copy(hash[:], raw)
		output, err := read.GetUTXO(hash, ref.Index)
		if err != nil || output == nil {
			continue
		}
		block, cached := blocks[output.BlockHeight]
		if !cached {
			var size int
			if budget <= 0 {
				continue
			}
			block, size, err = read.ReadCanonicalBlockBoundedWithSize(output.BlockHeight, budget)
			budget -= size
			if err != nil || block == nil || core.MerkleRoot(block.Txs) != block.Header.MerkleRoot {
				block = nil
			}
			blocks[output.BlockHeight] = block
		}
		if block == nil {
			continue
		}
		for txIndex, tx := range block.Txs {
			if tx.Hash() != hash || int(ref.Index) >= len(tx.Outputs) {
				continue
			}
			actual := tx.Outputs[ref.Index]
			if actual.OneTimePub == output.OneTimePub &&
				actual.TxPubKey == output.TxPubKey && actual.AmountCommit == output.AmountCommit &&
				actual.EncAmount == output.EncAmount {
				bh := block.Hash()
				origin := "transfer"
				if txIndex == 0 && block.Header.Height > 0 && tx.IsCoinbase() && !tx.IsStake() {
					origin = "validator_reward"
				}
				if tx.IsSale() {
					origin = "protocol_sale"
				}
				result[id] = &walletCanonicalEvidence{Height: block.Header.Height,
					BlockHash: hex.EncodeToString(bh[:]), Fee: strconv.FormatUint(tx.Fee, 10), Origin: origin}
			}
		}
	}
	return result
}
