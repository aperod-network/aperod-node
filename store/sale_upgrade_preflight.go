package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// SaleUpgradeReport is an offline consistency check, not authorization to
// initialize an allocation or proof of global historical issuance.
type SaleUpgradeReport struct {
	Schema    int    `json:"schema"`
	Status    string `json:"status"`
	Mode      string `json:"mode"`
	TipHeight uint64 `json:"tip_height"`
	TipHash   string `json:"tip_hash"`
}

// CheckSaleUpgradeCopy never opens the running database and never writes to the
// copy. expectedHash/Height bind a completed backup manifest to the actual DB.
func CheckSaleUpgradeCopy(copyPath, livePath, migrationPath string, expectedHash crypto.Hash32, expectedHeight uint64) (report SaleUpgradeReport, err error) {
	target, err := filepath.EvalSymlinks(copyPath)
	if err != nil {
		return report, fmt.Errorf("sale preflight: copy unavailable: %w", err)
	}
	live, liveErr := filepath.EvalSymlinks(livePath)
	if liveErr != nil && !os.IsNotExist(liveErr) {
		return report, fmt.Errorf("sale preflight: cannot resolve live database")
	}
	if liveErr == nil {
		a, aErr := os.Stat(target)
		b, bErr := os.Stat(live)
		if aErr != nil || bErr != nil || target == live || os.SameFile(a, b) {
			return report, fmt.Errorf("sale preflight: refusing live database or its alias")
		}
	}
	db, err := OpenReadOnly(target)
	if err != nil {
		return report, err
	}
	defer func() {
		if closeErr := db.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	// GetTip historically treats absent metadata as zero; reject it explicitly.
	heightRaw, err := db.GetMeta("tip/height")
	if err != nil || len(heightRaw) != 8 {
		return report, fmt.Errorf("sale preflight: canonical tip height missing or malformed")
	}
	hashRaw, err := db.GetMeta("tip/hash")
	if err != nil || len(hashRaw) != 32 {
		return report, fmt.Errorf("sale preflight: canonical tip hash missing or malformed")
	}
	var hash crypto.Hash32
	copy(hash[:], hashRaw)
	height := binary.LittleEndian.Uint64(heightRaw)
	if hash == (crypto.Hash32{}) || hash != expectedHash || height != expectedHeight {
		return report, fmt.Errorf("sale preflight: completed copy manifest does not match canonical tip")
	}
	raw, err := db.GetRawBlockByHeight(height)
	var block core.Block
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &block) != nil ||
		block.Header.Height != height || block.Hash() != hash || block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
		return report, fmt.Errorf("sale preflight: canonical tip body/index invalid")
	}
	// This validates signatures, genesis binding, reconciliation and, for an
	// active ledger, checkpoint/height/hash and remaining + sold conservation.
	migration, err := db.LoadSaleMigrationFile(migrationPath)
	if err != nil {
		return report, fmt.Errorf("sale preflight: %w", err)
	}
	state, err := db.LoadSaleState()
	if err != nil {
		return report, err
	}
	mode := "inactive"
	if migration != nil {
		mode = "activation_pending"
		if state != nil {
			mode = "active"
		}
	}
	return SaleUpgradeReport{Schema: 1, Status: "ok", Mode: mode, TipHeight: height, TipHash: fmt.Sprintf("%x", hash[:])}, nil
}
