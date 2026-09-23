// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func (e *Engine) lpodActive(height uint64) bool {
	return e.cfg.LPoDMigration != nil && height >= e.cfg.LPoDMigration.Height
}

func (e *Engine) prepareLPoD(block *core.Block, leader crypto.Address) (*store.LPoDSettlement, *store.LPoDCheckpoint, []lpod.Payment, error) {
	height := block.Header.Height
	if !e.lpodActive(height) {
		return nil, nil, nil, nil
	}
	m := e.cfg.LPoDMigration
	if e.cfg.Store == nil || e.cfg.OnCanonicalBlock == nil || e.cfg.Registry == nil {
		return nil, nil, nil, fmt.Errorf("lpod: canonical store, atomic commit and validator registry required")
	}
	if e.cfg.GuardianFundActivationHeight != 0 || e.cfg.RewardAuthorizationActivationHeight == 0 ||
		e.cfg.RewardAuthorizationActivationHeight > m.Height || e.cfg.StakingPoolNAPR != 2_000_000_000*lpod.Unit ||
		(e.cfg.BlockRewardNAPR != 0 && e.cfg.BlockRewardNAPR != 3*lpod.Unit) ||
		(e.cfg.TailRewardNAPR != 0 && e.cfg.TailRewardNAPR != lpod.Unit) {
		return nil, nil, nil, fmt.Errorf("lpod: incompatible legacy reward schedule or Guardian allocation")
	}
	g := e.chain.Genesis()
	if g == nil || g.Hash() != m.Genesis {
		return nil, nil, nil, fmt.Errorf("lpod: wrong genesis")
	}
	if height == m.Height {
		if m.Root() != m.ReconciliationRoot {
			return nil, nil, nil, fmt.Errorf("lpod: wrong reconciliation commitment")
		}
		if err := e.cfg.Store.CheckLPoDConfig(m); err != nil {
			return nil, nil, nil, err
		}
	}
	r := &store.LPoDSettlement{Parent: block.Header.PrevHash, PositionProtocol: true, Timestamp: block.Header.Timestamp,
		Proposer: block.Header.ValidatorPub.Hex(), Leader: leader, Stake: map[string]store.LPoDValidatorStake{}}
	if height == m.Height {
		r.Migration = m
	}
	start := 0
	if leader == "" {
		if len(block.Txs) < 2 {
			return nil, nil, nil, fmt.Errorf("lpod: missing payout and checkpoint")
		}
		auth, err := block.Txs[0].LPoDPayoutAuthorization()
		if err != nil {
			return nil, nil, nil, err
		}
		r.Leader = auth.Leader
		start = 2
	}
	if _, _, _, err := crypto.DecodeAddress(r.Leader); err != nil {
		return nil, nil, nil, err
	}
	vaults := map[string]bool{r.Proposer: true}
	before, err := e.cfg.Store.LoadLPoDCheckpoint()
	if err != nil {
		return nil, nil, nil, err
	}
	if before != nil && (before.Allocation == nil || before.Allocation.ReconciliationRoot != m.ReconciliationRoot ||
		before.Allocation.FundingHeight != m.Height) {
		return nil, nil, nil, fmt.Errorf("lpod: configuration differs from funded fork")
	}
	if before != nil {
		for _, p := range before.Positions {
			vaults[hex.EncodeToString(p.Deposit.Vault[:])] = true
		}
	}
	for i := start; i < len(block.Txs); i++ {
		tx := block.Txs[i]
		if (tx.IsCoinbase() && !tx.IsStake()) || tx.IsGuardianFund() || tx.IsLPoD() || tx.IsLPoDPayout() {
			return nil, nil, nil, fmt.Errorf("lpod: extra mint or protocol payout")
		}
		if tx.IsLPoDPosition() {
			a, err := tx.LPoDPositionAction()
			if err != nil {
				return nil, nil, nil, err
			}
			vaults[hex.EncodeToString(a.Vault[:])] = true
			if e.txVerifier == nil || e.utxos == nil {
				return nil, nil, nil, fmt.Errorf("lpod: position input verifier unavailable")
			}
			if err := e.txVerifier.VerifyTx(&tx); err != nil {
				return nil, nil, nil, err
			}
		}
		r.Transactions = append(r.Transactions, tx)
	}
	for id := range vaults {
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 32 {
			return nil, nil, nil, fmt.Errorf("lpod: invalid vault identity")
		}
		entry, found := e.cfg.Registry.GetEntry(crypto.ValidatorPubKey(raw))
		if found && !entry.Seeded {
			r.Stake[id] = store.LPoDValidatorStake{Amount: entry.StakeNAPR, Active: entry.Status == core.ValidatorActive}
		}
	}
	c, payments, err := e.cfg.Store.PreviewLPoD(height, r)
	return r, c, payments, err
}

// Pre-screen stateful position requests without re-running O(n²) cryptographic
// verification. The producer's normal verifier has already authenticated every
// payload. Invalid/stale operations are evicted individually, not allowed to
// stall blocks or discard other users' valid deposits.
func (e *Engine) filterLPoDPositions(txs []core.Transaction, height uint64) ([]core.Transaction, error) {
	positions := map[string]store.LPoDPosition{}
	totals := map[string]uint64{}
	if e.lpodActive(height) {
		if e.cfg.Store == nil || e.cfg.Registry == nil {
			return nil, fmt.Errorf("lpod: position state unavailable")
		}
		c, err := e.cfg.Store.LoadLPoDCheckpoint()
		if err != nil {
			return nil, err
		}
		if c != nil {
			for id, p := range c.Positions {
				if p.Returned && p.Due == 0 {
					continue
				}
				positions[id] = p
				if p.UnlockHeight == 0 {
					totals[hex.EncodeToString(p.Deposit.Vault[:])] += p.Deposit.Amount - p.Withdrawn
				}
			}
		}
	}
	out := make([]core.Transaction, 0, len(txs))
	for _, tx := range txs {
		if !tx.IsLPoDPosition() {
			out = append(out, tx)
			continue
		}
		valid := e.lpodActive(height)
		var a core.LPoDPositionAction
		if json.Unmarshal(tx.Extra, &a) != nil {
			valid = false
		}
		id, v := hex.EncodeToString(a.PositionID[:]), hex.EncodeToString(a.Vault[:])
		if valid && a.Genesis != e.cfg.LPoDMigration.Genesis {
			valid = false
		}
		if valid {
			switch a.Action {
			case core.LPoDDeposit:
				_, exists := positions[id]
				stake, found := e.cfg.Registry.GetEntry(crypto.ValidatorPubKey(a.Vault[:]))
				max := uint64(100_000_000 * lpod.Unit)
				if exists || len(positions) >= store.LPoDMaxPositions || !found || stake.Seeded ||
					stake.Status != core.ValidatorActive || stake.StakeNAPR > max || totals[v] > max-stake.StakeNAPR ||
					a.Amount > max-stake.StakeNAPR-totals[v] {
					valid = false
					break
				}
				tier, err := lpod.TierFor(stake.StakeNAPR + totals[v] + a.Amount)
				if err != nil || a.Amount < tier.MinimumDeposit {
					valid = false
					break
				}
				positions[id] = store.LPoDPosition{Deposit: a}
				totals[v] += a.Amount
			case core.LPoDWithdraw:
				p, exists := positions[id]
				amount, err := store.LPoDWithdrawalAmount(p, a)
				if !exists || err != nil {
					valid = false
					break
				}
				p.Nonce++
				p.Withdrawn += amount
				p.Returned = p.Withdrawn == p.Deposit.Amount
				if p.Returned {
					p.UnlockHeight = height
				}
				totals[v] -= amount
				positions[id] = p
			default:
				valid = false
			}
		}
		if !valid {
			e.pool.Remove(tx.Hash())
			e.log.Warn("evicted stale or unauthorized LPoD position request", "tx", tx.Hash(), "height", height)
			continue
		}
		out = append(out, tx)
	}
	return out, nil
}

func (e *Engine) validateLPoD(block *core.Block) error {
	if !e.lpodActive(block.Header.Height) {
		for i := range block.Txs {
			tx := &block.Txs[i]
			if tx.IsLPoD() || tx.IsLPoDPosition() || tx.IsLPoDPayout() {
				return fmt.Errorf("lpod: transaction before activation")
			}
		}
		return nil
	}
	r, c, payments, err := e.prepareLPoD(block, "")
	if err != nil {
		return err
	}
	seen := map[crypto.Point32]bool{}
	for _, tx := range block.Txs {
		for _, out := range tx.Outputs {
			if seen[out.OneTimePub] {
				return fmt.Errorf("lpod: duplicate output key would make payouts unspendable")
			}
			seen[out.OneTimePub] = true
		}
	}
	payout, err := e.cfg.Store.PayoutLPoD(block.Header.Height, r, c, payments)
	if err != nil {
		return err
	}
	want, _ := json.Marshal(payout)
	got, _ := json.Marshal(block.Txs[0])
	if string(want) != string(got) {
		return fmt.Errorf("lpod: payout outputs differ from authenticated entitlement plan")
	}
	digest, err := block.Txs[1].LPoDCheckpointDigest()
	if err != nil || digest != c.Digest() {
		return fmt.Errorf("lpod: invalid position/accounting checkpoint")
	}
	return nil
}

func (e *Engine) IsFinalizedHash(height uint64, hash crypto.Hash32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lpodFinalizedHashes[height] == hash && hash != (crypto.Hash32{})
}
