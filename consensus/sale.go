package consensus

import (
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func (e *Engine) validateVersionWithSales(tx *core.Transaction, height, activation uint64) error {
	if tx.IsSale() {
		if e.cfg.SaleMigration == nil || height < e.cfg.SaleMigration.Checkpoint.Height {
			return fmt.Errorf("sale: inactive transaction version")
		}
		_, err := tx.SaleAuthorization()
		return err
	}
	return core.ValidateTxVersionAtHeight(tx, height, activation, e.cfg.RingCTCLSAGActivationHeight, e.cfg.AVMActivationHeight)
}

func (e *Engine) prepareSale(b *core.Block) (*store.SaleSettlement, error) {
	if e.cfg.SaleMigration != nil && e.cfg.GenesisVestingActivationHeight != 0 &&
		e.cfg.GenesisVestingActivationHeight >= e.cfg.SaleMigration.Checkpoint.Height {
		return nil, fmt.Errorf("sale: scheduled new genesis reserve overlaps the sale allocation")
	}
	if e.cfg.Store == nil {
		for _, tx := range b.Txs {
			if tx.IsSale() {
				return nil, fmt.Errorf("sale: durable store missing")
			}
		}
		if e.cfg.SaleMigration != nil && b.Header.Height >= e.cfg.SaleMigration.Checkpoint.Height {
			return nil, fmt.Errorf("sale: durable store missing")
		}
		return nil, nil
	}
	return e.cfg.Store.PrepareSaleBlock(b, e.cfg.SaleMigration)
}

func (e *Engine) verifyBlockWithSales(b *core.Block) error {
	prepared, err := e.prepareSale(b)
	if err != nil {
		return err
	}
	if prepared == nil {
		return e.txVerifier.VerifyBlock(b)
	}
	// The sale-specific validator has checked opening, issuer authority,
	// checkpoint, allocation and replay fences. Keep the generic verifier
	// unchanged: arbitrary zero-input transactions still fail there.
	filtered := *b
	filtered.Txs = make([]core.Transaction, 0, len(b.Txs))
	for _, tx := range b.Txs {
		if !tx.IsSale() {
			filtered.Txs = append(filtered.Txs, tx)
		}
	}
	return e.txVerifier.VerifyBlock(&filtered)
}
