// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/syndtr/goleveldb/leveldb"
)

// Nominal public baseline before any validator-pool draw. Reconciliation uses
// the attested REMAINING validator budget, not this fixed pre-issuance baseline.
const LPoDPublicAllocationNAPRO uint64 = 7_000_000_000 * lpod.Unit

// LPoDOpening discloses a historical issuance commitment, not a wallet spend key.
// The proof must cover every coinbase output in canonical order without omissions.
type LPoDOpening struct {
	Height      uint64             `json:"height"`
	TxIndex     uint32             `json:"tx_index"`
	OutputIndex uint32             `json:"output_index"`
	Amount      uint64             `json:"amount_napro,string"`
	Blind       crypto.BlindFactor `json:"blind"`
}

type LPoDMigration struct {
	Version uint8 `json:"version"`
	// PositionLifecycleVersion is quorum-attested separately from the historical
	// accounting format. Version 1 enables deterministic effective-vault
	// reassignment and canonical automatic principal refunds.
	PositionLifecycleVersion uint8             `json:"position_lifecycle_version"`
	Height                   uint64            `json:"height"`
	Genesis                  crypto.Hash32     `json:"genesis"`
	ReconciliationRoot       crypto.Hash32     `json:"reconciliation_root"`
	Openings                 []LPoDOpening     `json:"openings"`
	BodyRoot                 crypto.Hash32     `json:"body_root"`
	HistoricalIssued         uint64            `json:"historical_issued_napro,string"`
	ValidatorRemaining       uint64            `json:"validator_remaining_napro,string"`
	Attestations             []LPoDAttestation `json:"attestations"`
	// Injected ONLY from the node's trusted genesis validator configuration.
	// Not supplied by an untrusted witness file.
	TrustedValidators []crypto.ValidatorPubKey `json:"-"`
}

type LPoDAttestation struct {
	Validator crypto.ValidatorPubKey `json:"validator"`
	Signature []byte                 `json:"signature"`
}

func (m *LPoDMigration) AttestationMessage() crypto.Hash32 {
	root := m.Root()
	return crypto.HashBytes([]byte("aperod/lpod/reconciliation-governance/v1"), root[:])
}

func (m *LPoDMigration) verifyAttestations() error {
	trusted := map[string]bool{}
	for _, p := range m.TrustedValidators {
		if len(p) != 32 {
			return fmt.Errorf("lpod: invalid trusted genesis authority")
		}
		trusted[p.Hex()] = true
	}
	if len(trusted) == 0 {
		return fmt.Errorf("lpod: no trusted genesis reconciliation authorities")
	}
	seen := map[string]bool{}
	for _, a := range m.Attestations {
		id := a.Validator.Hex()
		if !trusted[id] || seen[id] || !a.Validator.Verify(m.AttestationMessage(), a.Signature) {
			return fmt.Errorf("lpod: invalid or duplicate reconciliation attestation")
		}
		seen[id] = true
	}
	if len(seen) <= 2*len(trusted)/3 {
		return fmt.Errorf("lpod: reconciliation requires greater-than-two-thirds trusted genesis quorum")
	}
	return nil
}

func (m *LPoDMigration) VerifyAuthorization() error {
	if m == nil || m.Version != 1 || m.PositionLifecycleVersion != 1 || m.Height == 0 || m.Root() != m.ReconciliationRoot {
		return fmt.Errorf("lpod: invalid reconciliation authorization")
	}
	return m.verifyAttestations()
}

type LPoDAllocation struct {
	Version                  uint8         `json:"version"`
	PositionLifecycleVersion uint8         `json:"position_lifecycle_version"`
	Genesis                  crypto.Hash32 `json:"genesis"`
	FundingHeight            uint64        `json:"funding_height"`
	FundingBlock             crypto.Hash32 `json:"funding_block"`
	ReconciliationRoot       crypto.Hash32 `json:"reconciliation_root"`
	HistoricalIssued         uint64        `json:"historical_issued_napro,string"`
	Remaining                uint64        `json:"remaining_napro,string"`
	// Historical budget is explicitly quorum-attested and must match the
	// existing durable validator pool. Subsequent draws are checkpoint-bound.
	ValidatorRemaining        uint64 `json:"validator_remaining_napro,string"`
	InitialValidatorRemaining uint64 `json:"initial_validator_remaining_napro,string"`
	TailIssued                uint64 `json:"tail_issued_napro,string"`
}

// Root commits the complete reconciliation witness, activation height and actual
// genesis. A scheduled validator cannot choose a different witness on activation.
func (m *LPoDMigration) Root() crypto.Hash32 {
	b, _ := json.Marshal(struct {
		Version                              uint8
		PositionLifecycleVersion             uint8
		Height                               uint64
		Genesis                              crypto.Hash32
		Openings                             []LPoDOpening
		BodyRoot                             crypto.Hash32
		HistoricalIssued, ValidatorRemaining uint64
	}{m.Version, m.PositionLifecycleVersion, m.Height, m.Genesis, m.Openings, m.BodyRoot, m.HistoricalIssued, m.ValidatorRemaining})
	return crypto.HashBytes([]byte("aperod/lpod/reconciliation/v1"), b)
}

// ReconcileLPoD scans FULL canonical history. Missing openings or pruned blocks are
// hard errors. No estimate from the API, supply display or amount index is accepted.
func ReconcileLPoD(m *LPoDMigration, read func(uint64) (*core.Block, error)) (uint64, crypto.Hash32, error) {
	if m == nil || m.Version != 1 || m.PositionLifecycleVersion != 1 || m.Height == 0 || m.Genesis == (crypto.Hash32{}) ||
		m.Root() != m.ReconciliationRoot {
		return 0, crypto.Hash32{}, fmt.Errorf("lpod: invalid version, activation, genesis or reconciliation root")
	}
	var issued uint64
	if err := m.verifyAttestations(); err != nil {
		return 0, crypto.Hash32{}, err
	}
	if m.ValidatorRemaining > 2_000_000_000*lpod.Unit {
		return 0, crypto.Hash32{}, fmt.Errorf("lpod: attested validator budget exceeds original allocation")
	}
	bodyRoot := crypto.HashBytes([]byte("aperod/lpod/historical-bodies/v1"))
	var parent crypto.Hash32
	pos := 0
	for h := uint64(0); h < m.Height; h++ {
		b, err := read(h)
		if err != nil || b == nil {
			return 0, parent, fmt.Errorf("lpod: reconciliation requires full canonical block %d: %v", h, err)
		}
		if b.Header.Height != h || b.Header.MerkleRoot != core.MerkleRoot(b.Txs) ||
			(h == 0 && b.Hash() != m.Genesis) || (h > 0 && b.Header.PrevHash != parent) {
			return 0, parent, fmt.Errorf("lpod: invalid canonical reconciliation block %d", h)
		}
		parent = b.Hash()
		bodyRoot = LPoDBodyRootStep(bodyRoot, b)
		for ti := range b.Txs {
			tx := &b.Txs[ti]
			if tx.IsGuardianFund() {
				return 0, parent, fmt.Errorf("lpod: legacy Guardian allocation requires explicit migration, not double funding")
			}
			if !tx.IsCoinbase() {
				continue
			}
			for oi, out := range tx.Outputs {
				if pos >= len(m.Openings) {
					return 0, parent, fmt.Errorf("lpod: missing issuance opening at %d/%d/%d", h, ti, oi)
				}
				o := m.Openings[pos]
				pos++
				c, err := crypto.Commit(o.Amount, o.Blind)
				if o.Height != h || uint64(o.TxIndex) != uint64(ti) || uint64(o.OutputIndex) != uint64(oi) ||
					err != nil || c != out.AmountCommit {
					return 0, parent, fmt.Errorf("lpod: unproven issuance opening at %d/%d/%d", h, ti, oi)
				}
				if o.Amount > 9_000_000_000*lpod.Unit-issued {
					return 0, parent, fmt.Errorf("lpod: historical issuance exceeds conservative public allocation")
				}
				issued += o.Amount
			}
		}
	}
	if pos != len(m.Openings) || bodyRoot != m.BodyRoot || issued != m.HistoricalIssued ||
		issued > 9_000_000_000*lpod.Unit-m.ValidatorRemaining-lpod.InitialNAPRO {
		return 0, parent, fmt.Errorf("lpod: surplus proof entries or insufficient reconciled allocation")
	}
	return issued, parent, nil
}

// Length-framed canonical full-body serialization is independently attested.
// Legacy transaction hashes are NOT unambiguous body commitments.
func LPoDBodyRootStep(parent crypto.Hash32, b *core.Block) crypto.Hash32 {
	raw, _ := json.Marshal(b)
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(raw)))
	return crypto.HashBytes([]byte("aperod/lpod/historical-body/v1"), parent[:], size[:], raw)
}

func (d *DB) readLPoDCanonicalBlock(height uint64) (*core.Block, error) {
	raw, err := d.GetRawBlockByHeight(height)
	if err != nil || raw == nil {
		return nil, fmt.Errorf("missing canonical block %d: %v", height, err)
	}
	var b core.Block
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	// Raw full-block and pruned StoredBlock encodings differ. Verify the full
	// body hashes to the durable canonical index, not just a matching height.
	index, err := d.get(heightKey(height))
	if err != nil || len(index) != 32 || string(index) != string(b.Hash().Bytes()) {
		return nil, fmt.Errorf("lpod: pruned or noncanonical block %d", height)
	}
	return &b, nil
}

// VerifyLPoDMigration validates a proof without writing any funding state.
func (d *DB) VerifyLPoDMigration(m *LPoDMigration) (uint64, crypto.Hash32, error) {
	return ReconcileLPoD(m, d.readLPoDCanonicalBlock)
}

// appendLPoDMigration is called only in the same batch as the activation block.
// The binding survives rollback so a changed config cannot mint another reserve.
func (d *DB) appendLPoDMigration(batch *leveldb.Batch, hash crypto.Hash32, height uint64, m *LPoDMigration) (*LPoDCheckpoint, error) {
	if m == nil || height != m.Height {
		return nil, fmt.Errorf("lpod: migration at wrong height")
	}
	bindingKey := []byte("lpod/activation/v1")
	binding, err := d.get(bindingKey)
	if err != nil {
		return nil, err
	}
	root := m.Root()
	if binding != nil && string(binding) != string(root[:]) {
		return nil, fmt.Errorf("lpod: immutable activation binding mismatch")
	}
	issued, parent, err := d.VerifyLPoDMigration(m)
	if err != nil {
		return nil, err
	}
	tip, tipHeight, err := d.GetTip()
	if err != nil || tip != parent || tipHeight != height-1 {
		return nil, fmt.Errorf("lpod: migration does not extend reconciled canonical tip")
	}
	pool, found, err := d.LoadStakingPoolRemaining()
	if err != nil || !found || pool != m.ValidatorRemaining {
		return nil, fmt.Errorf("lpod: historical validator-pool reconciliation mismatch; refusing to change existing validator entitlement")
	}
	batch.Put(bindingKey, root[:])
	var activation [8]byte
	binary.LittleEndian.PutUint64(activation[:], height)
	batch.Put([]byte("lpod/activation_height/v1"), activation[:])
	var initialPool [8]byte
	binary.LittleEndian.PutUint64(initialPool[:], m.ValidatorRemaining)
	batch.Put([]byte("lpod/activation_validator_budget/v1"), initialPool[:])
	return &LPoDCheckpoint{
		State:   lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: lpod.InitialNAPRO, LastHeight: height - 1},
		Carries: map[string]lpod.Carry{},
		Allocation: &LPoDAllocation{
			Version: 1, PositionLifecycleVersion: m.PositionLifecycleVersion, Genesis: m.Genesis, FundingHeight: height, FundingBlock: hash,
			ReconciliationRoot: root, HistoricalIssued: issued,
			Remaining:                 9_000_000_000*lpod.Unit - m.ValidatorRemaining - issued - lpod.InitialNAPRO,
			ValidatorRemaining:        m.ValidatorRemaining,
			InitialValidatorRemaining: m.ValidatorRemaining,
		},
	}, nil
}

func (d *DB) restoreLPoDPool(batch *leveldb.Batch, hash crypto.Hash32, height uint64) error {
	activation, err := d.get([]byte("lpod/activation_height/v1"))
	if err != nil || activation == nil {
		return err
	}
	if len(activation) != 8 {
		return fmt.Errorf("lpod: corrupt activation height")
	}
	fundingHeight := binary.LittleEndian.Uint64(activation)
	var remaining uint64
	if height >= fundingHeight {
		c, err := d.lpodCheckpoint(hash)
		if err != nil || c == nil || c.Allocation == nil || c.State.LastHeight != height {
			return fmt.Errorf("lpod: cannot select tip without matching conserved checkpoint")
		}
		remaining = c.Allocation.ValidatorRemaining
	} else {
		if height+1 != fundingHeight {
			return fmt.Errorf("lpod: rollback before attested funding parent requires historical validator-budget evidence")
		}
		raw, err := d.get([]byte("lpod/activation_validator_budget/v1"))
		if err != nil || len(raw) != 8 {
			return fmt.Errorf("lpod: missing attested funding-parent validator budget")
		}
		remaining = binary.LittleEndian.Uint64(raw)
	}
	var value [8]byte
	binary.LittleEndian.PutUint64(value[:], remaining)
	batch.Put(append(append([]byte{}, prefixMeta...), []byte("staking_pool_remaining")...), value[:])
	return nil
}

func lpodValidatorRemaining(height uint64) uint64 {
	const initial = 2_000_000_000 * lpod.Unit
	const reward = 3 * lpod.Unit
	if height > initial/reward {
		return 0
	}
	return initial - height*reward
}
