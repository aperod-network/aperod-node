package consensus

// CanSettleSale checks the actual current producer, not merely possession of a
// working signing key. Admission must close while halted, paused or ineligible.
func (e *Engine) CanSettleSale() bool {
	if e.cfg.SaleMigration == nil || e.cfg.SaleTransactions == nil ||
		e.cfg.MyKey == nil || e.cfg.Store == nil || e.cfg.Registry == nil || e.halted.Load() {
		return false
	}
	if !e.cfg.MyKey.Public().Equals(e.cfg.SaleMigration.Checkpoint.SaleAuthority) {
		return false
	}
	hash, height, err := e.cfg.Store.GetTip()
	if err != nil || height == ^uint64(0) ||
		(e.cfg.StopProducingAtHeight != 0 && height+1 >= e.cfg.StopProducingAtHeight) {
		return false
	}
	state, err := e.cfg.Store.LoadSaleState()
	if err != nil || state == nil || state.Hash != hash || state.Height != height ||
		state.Checkpoint != e.cfg.SaleMigration.Checkpoint.Message() {
		return false
	}
	for _, member := range e.cfg.Registry.GetActiveValidators() {
		if member.Equals(e.localIdentity(height + 1)) {
			return true
		}
	}
	return false
}
