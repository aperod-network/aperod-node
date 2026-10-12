package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/sale"
)

// LoadSaleMigrationFile reads a LOCAL operator trust configuration, never a
// witness supplied by HTTP or a peer. The configured signers are explicit trust
// roots, not roots discovered from the payment server's response.
// This performs no writes and cannot initialize or repair the allocation ledger.
func (d *DB) LoadSaleMigrationFile(path string) (*SaleMigration, error) {
	state, err := d.LoadSaleState()
	if err != nil {
		return nil, err
	}
	if path == "" {
		if state != nil {
			return nil, fmt.Errorf("sale: activated ledger requires its signed configuration")
		}
		return nil, nil
	}
	m, err := readSignedSaleMigrationFile(path)
	if err != nil {
		return nil, err
	}
	cp := m.Checkpoint
	genesisRaw, err := d.GetRawBlockByHeight(0)
	if err != nil {
		return nil, err
	}
	var genesis core.Block
	if len(genesisRaw) == 0 || json.Unmarshal(genesisRaw, &genesis) != nil || genesis.Hash() != cp.Genesis {
		return nil, fmt.Errorf("sale: configuration does not bind the canonical genesis")
	}
	hash, height, err := d.GetTip()
	if err != nil {
		return nil, err
	}
	if state == nil {
		if height == ^uint64(0) || cp.Height != height+1 || cp.Parent != hash {
			return nil, fmt.Errorf("sale: initial activation requires the exact canonical parent barrier")
		}
		lpod, err := d.LoadLPoDCheckpoint()
		if err != nil {
			return nil, err
		}
		if err := VerifySaleReserveFloors(lpod, m.Reconciliation); err != nil {
			return nil, err
		}
		knownIssued, err := SaleKnownIssuedFloor(lpod)
		if err != nil {
			return nil, err
		}
		if err := sale.VerifyReconciledCheckpoint(cp, m.Reconciliation, genesis.Hash(), hash,
			lpod.Digest(), height+1, knownIssued, m.Trusted); err != nil {
			return nil, err
		}
	} else if state.Checkpoint != cp.Message() || state.Hash != hash || state.Height != height || height < cp.Height {
		return nil, fmt.Errorf("sale: durable ledger and signed configuration disagree")
	}
	if state != nil && (state.Sold > cp.SaleRemaining || state.Remaining != cp.SaleRemaining-state.Sold) {
		return nil, fmt.Errorf("sale: durable budget does not match its signed baseline")
	}
	return m, nil
}

func readSignedSaleMigrationFile(path string) (*SaleMigration, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1_048_577))
	if err != nil || len(raw) > 1_048_576 {
		return nil, fmt.Errorf("sale: configuration size invalid")
	}
	var m SaleMigration
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("sale: trailing configuration data")
	}
	cp := m.Checkpoint
	if cp.SnapshotRoot != m.Reconciliation.Digest() {
		return nil, fmt.Errorf("sale: unsigned reconciliation")
	}
	if err = cp.Verify(cp.Genesis, cp.Parent, cp.Height, m.Trusted); err != nil {
		return nil, err
	}
	return &m, nil
}
