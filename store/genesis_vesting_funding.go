// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/aperod/aperod/crypto"
)

const GenesisVestingTrust = "operator-attested-exclusive-unissued-development-genesis-reserve/v1"
const GenesisLiquidityTrust = "operator-attested-exclusive-unissued-initial-liquidity-genesis-allocation/v1"
const GenesisLiquidityAmount uint64 = 300_000_000 * 100_000_000

// Circulation arithmetic only supplies an upper bound, NOT source eligibility.
// This distinct, purpose-bound governance attestation explicitly declares that
// the Development Fund reserve is unissued and excludes Guardian, validator,
// user balances and every other allocation. Historical proof remains false.
type GenesisVestingFunding struct {
	Version            uint32            `json:"version"`
	Genesis            crypto.Hash32     `json:"genesis"`
	Snapshot           crypto.Hash32     `json:"nominal_snapshot"`
	Height             uint64            `json:"activation_height"`
	Recipient          crypto.Address    `json:"recipient"`
	Reserve            uint64            `json:"exclusive_unissued_development_napro,string"`
	OtherAllocations   uint64            `json:"other_genesis_allocations_napro,string"`
	Trust              string            `json:"trust_assumption"`
	LiquidityRecipient crypto.Address    `json:"liquidity_recipient,omitempty"`
	LiquidityAmount    uint64            `json:"liquidity_napro,string,omitempty"`
	LiquidityTrust     string            `json:"liquidity_trust_assumption,omitempty"`
	Attestations       []LPoDAttestation `json:"attestations"`
}

func (f *GenesisVestingFunding) Message() crypto.Hash32 {
	if f == nil {
		return crypto.Hash32{}
	}
	c := *f
	c.Attestations = nil
	raw, _ := json.Marshal(c)
	return crypto.HashBytes(append([]byte("aperod/genesis-development-funding/v1\x00"), raw...))
}

func (f *GenesisVestingFunding) validate(a *LPoDAllocation, height uint64) error {
	if f == nil || a == nil || (f.Version != 1 && f.Version != 2) || f.Genesis != a.Genesis || f.Snapshot != a.SnapshotRoot ||
		f.Height != height || f.Recipient != GenesisVestingRecipient || f.Reserve != GenesisVestingReserve ||
		f.Trust != GenesisVestingTrust ||
		len(f.Attestations) == 0 || len(f.Attestations) > 512 {
		return fmt.Errorf("genesis vesting: separate purpose-bound funding attestation required")
	}
	if f.Version == 1 {
		if f.OtherAllocations != GenesisNominalTotal-GenesisVestingReserve ||
			f.LiquidityAmount != 0 || f.LiquidityRecipient != "" || f.LiquidityTrust != "" {
			return fmt.Errorf("genesis vesting: legacy declaration cannot authorize liquidity")
		}
	} else {
		if f.OtherAllocations != GenesisNominalTotal-GenesisVestingReserve-GenesisLiquidityAmount ||
			f.LiquidityAmount != GenesisLiquidityAmount || f.LiquidityRecipient == "" ||
			f.LiquidityRecipient == GenesisVestingRecipient || f.LiquidityTrust != GenesisLiquidityTrust {
			return fmt.Errorf("genesis vesting: distinct exclusive liquidity declaration required")
		}
		if network, _, _, err := crypto.DecodeAddress(f.LiquidityRecipient); err != nil || network != crypto.MainnetByte {
			return fmt.Errorf("genesis vesting: invalid liquidity address: %w", err)
		}
		// Liquidity was already nominally available at the original snapshot;
		// it is not taken from Guardian's fully earmarked eligible allocation.
		if a.NominalSnapshot == nil || a.NominalSnapshot.NominalCirculatingBefore < a.NominalEligible ||
			a.NominalSnapshot.NominalCirculatingBefore-a.NominalEligible < GenesisLiquidityAmount {
			return fmt.Errorf("genesis vesting: liquidity exceeds distinct nominal available bound")
		}
	}
	seen := map[string]bool{}
	for _, attestation := range f.Attestations {
		id := attestation.Validator.Hex()
		if seen[id] || !attestation.Validator.Verify(f.Message(), attestation.Signature) {
			return fmt.Errorf("genesis vesting: invalid or duplicate funding signature")
		}
		seen[id] = true
	}
	return nil
}

func (f *GenesisVestingFunding) verifyQuorum(a *LPoDAllocation, height uint64, stake map[string]LPoDValidatorStake) error {
	if err := f.validate(a, height); err != nil {
		return err
	}
	total, signed := new(big.Int), new(big.Int)
	for _, s := range stake {
		if s.Active {
			total.Add(total, new(big.Int).SetUint64(s.Amount))
		}
	}
	for _, attestation := range f.Attestations {
		s, ok := stake[attestation.Validator.Hex()]
		if !ok || !s.Active || s.Amount == 0 {
			return fmt.Errorf("genesis vesting: funding signer is not an active validator")
		}
		signed.Add(signed, new(big.Int).SetUint64(s.Amount))
	}
	if total.Sign() == 0 || new(big.Int).Mul(signed, big.NewInt(3)).Cmp(new(big.Int).Mul(total, big.NewInt(2))) <= 0 {
		return fmt.Errorf("genesis vesting: funding lacks canonical two-thirds quorum")
	}
	return nil
}

func LoadGenesisVestingFunding(path string, height uint64) (*GenesisVestingFunding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 256*1024 {
		return nil, fmt.Errorf("genesis vesting: funding witness too large")
	}
	var f GenesisVestingFunding
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Height != height || height == 0 {
		return nil, fmt.Errorf("genesis vesting: funding height mismatch")
	}
	return &f, nil
}

// Reject missed activations and disabling an already committed fund before
// starting consensus; do not silently run until the first production failure.
func (d *DB) CheckGenesisVestingConfig(height uint64, proof *GenesisVestingFunding) error {
	c, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return err
	}
	if c != nil && c.GenesisVesting != nil {
		if height != c.GenesisVesting.FundingHeight || proof == nil ||
			proof.Message() != c.GenesisVesting.FundingProof.Message() {
			return fmt.Errorf("genesis vesting: committed funding configuration cannot change")
		}
		return proof.validate(c.Allocation, height)
	}
	if height == 0 {
		if proof != nil {
			return fmt.Errorf("genesis vesting: funding file without activation")
		}
		return nil
	}
	saleState, err := d.LoadSaleState()
	if err != nil {
		return err
	}
	if saleState != nil {
		return fmt.Errorf("genesis vesting: active sales require a versioned shared reserve transition")
	}
	_, tipHeight, err := d.GetTip()
	if err != nil {
		return err
	}
	if tipHeight >= height {
		return fmt.Errorf("genesis vesting: activation already missed")
	}
	if c == nil {
		return fmt.Errorf("genesis vesting: canonical v3 funding prerequisite missing")
	}
	if err := proof.validate(c.Allocation, height); err != nil {
		return err
	}
	bound, err := nominalNonCirculatingBound(c.Allocation)
	if err != nil {
		return err
	}
	if bound < GenesisVestingReserve {
		return fmt.Errorf("genesis vesting: original nominal cap exhausted")
	}
	return nil
}
