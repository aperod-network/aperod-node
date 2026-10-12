package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type walletHistoryTransactionRef struct {
	Hash      string   `json:"tx_hash"`
	KeyImages []string `json:"key_images"`
}

// A successful broadcast or index lookup is not confirmation. Match the
// locally journalled transaction hash AND its input images in a real body.
// This also works for exact-budget sends with no output owned by the sender.
func walletHistoryTransactionEvidence(read *store.WalletReadSnapshot, refs []walletHistoryTransactionRef) map[string]*walletCanonicalEvidence {
	result := make(map[string]*walletCanonicalEvidence, len(refs))
	bodies := make(map[uint64]*core.Block)
	budget := 8 << 20
	for _, ref := range refs {
		result[ref.Hash] = nil
		raw, err := hex.DecodeString(ref.Hash)
		if err != nil || len(raw) != 32 || len(ref.KeyImages) == 0 {
			continue
		}
		var hash crypto.Hash32
		copy(hash[:], raw)
		locator, err := read.LookupTxIdx(hash)
		if err != nil {
			continue
		}
		var height uint64
		if locator != nil {
			height = locator.Height
		} else {
			// Old canonical outputs can precede the transaction locator index.
			output, err := read.GetUTXO(hash, 0)
			if err != nil || output == nil {
				continue
			}
			height = output.BlockHeight
		}
		block, cached := bodies[height]
		if !cached {
			if budget <= 0 {
				continue
			}
			var size int
			block, size, err = read.ReadCanonicalBlockBoundedWithSize(height, budget)
			budget -= size
			if err != nil || block == nil {
				block = nil
			}
			bodies[height] = block
		}
		if block == nil {
			continue
		}
		for _, tx := range block.Txs {
			if tx.Hash() != hash || len(tx.Inputs) != len(ref.KeyImages) {
				continue
			}
			expected := make(map[string]bool, len(ref.KeyImages))
			valid := true
			for _, image := range ref.KeyImages {
				raw, err := hex.DecodeString(image)
				if err != nil || len(raw) != 32 || expected[image] {
					valid = false
					break
				}
				expected[image] = true
			}
			for _, input := range tx.Inputs {
				image := hex.EncodeToString(input.KeyImage[:])
				if !expected[image] {
					valid = false
					break
				}
				delete(expected, image)
			}
			if !valid || len(expected) != 0 {
				continue
			}
			bh := block.Hash()
			result[ref.Hash] = &walletCanonicalEvidence{Height: block.Header.Height,
				BlockHash: hex.EncodeToString(bh[:]), Fee: strconv.FormatUint(tx.Fee, 10), Origin: "transfer"}
		}
	}
	return result
}

func (s *Server) restWalletHistoryTransactions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, 405, "POST required")
		return
	}
	var req struct {
		SnapshotID   string                        `json:"snapshot_id"`
		Address      string                        `json:"address"`
		Checkpoint   string                        `json:"checkpoint_hash"`
		Transactions []walletHistoryTransactionRef `json:"transactions"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 100000)).Decode(&req) != nil ||
		len(req.Transactions) == 0 || len(req.Transactions) > 128 {
		writeJSONError(w, 400, "bounded transaction references required")
		return
	}
	total := 0
	seen := make(map[string]bool)
	for _, ref := range req.Transactions {
		if len(ref.Hash) != 64 || seen[ref.Hash] || len(ref.KeyImages) == 0 || len(ref.KeyImages) > 256 {
			writeJSONError(w, 400, "invalid transaction reference")
			return
		}
		seen[ref.Hash] = true
		total += len(ref.KeyImages)
	}
	if total > 1024 {
		writeJSONError(w, 400, "transaction input limit exceeded")
		return
	}
	address, err := validSnapshotAddress(req.Address)
	if err != nil {
		writeJSONError(w, 400, "valid address required")
		return
	}
	lease := s.acquireWalletSnapshot(w, req.SnapshotID, address)
	if lease == nil {
		return
	}
	defer lease.Release()
	if req.Checkpoint != fmt.Sprintf("%x", lease.entry.hash[:]) {
		writeJSONError(w, 409, "snapshot checkpoint mismatch")
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"checkpoint_hash": req.Checkpoint,
		"transactions":    walletHistoryTransactionEvidence(lease.entry.read, req.Transactions),
	})
}
