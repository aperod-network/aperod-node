package store

import "fmt"

// The first sale protocol does not implement a shared-ledger reclassification
// into a newly activated genesis reserve. Fail closed instead of independently
// reusing the old nominal non-circulating bound after sales have reserved it.
func (d *DB) checkSaleGenesisFundingTransition(height uint64, request *LPoDSettlement, activatingSales bool) error {
	if request == nil || request.GenesisVestingActivationHeight == 0 ||
		height < request.GenesisVestingActivationHeight {
		return nil
	}
	c, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return err
	}
	if c != nil && c.GenesisVesting != nil {
		return nil
	}
	state, err := d.LoadSaleState()
	if err != nil {
		return err
	}
	if activatingSales || state != nil {
		return fmt.Errorf("sale: new genesis funding requires a versioned shared reserve transition")
	}
	return nil
}
