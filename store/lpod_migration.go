// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/syndtr/goleveldb/leveldb"
)

// Nominal public baseline before any validator-pool draw. Reconciliation uses
// the attested REMAINING validator budget, not this fixed pre-issuance baseline.
const LPoDPublicAllocationNAPRO uint64 = 7_000_000_000 * lpod.Unit
const LPoDV2ActivationHeight uint64 = 2_450_000
const LPoDV2TrustAssumption = "TRUST_ATTESTED_SNAPSHOT_AND_HISTORICAL_ALLOCATION"
const LPoDV3TrustAssumption = "TRUST_ATTESTED_NOMINAL_ALLOCATION_RECLASSIFICATION"
const LPoDV3NominalAllocationMaxNAPRO uint64 = 7_000_000_000 * lpod.Unit
const LPoDV3NominalCirculationMaxNAPRO uint64 = 10_000_000_000 * lpod.Unit
const LPoDV3NominalBasis = "nominal_genesis_allocation_and_vesting"
const LPoDV3EligibilityRule = "UNISSUED_UNLOCKED_PUBLIC_ALLOCATION_EXCLUDING_ISSUED_WALLET_UTXOS_VALIDATOR_BUDGET_DEVELOPER_VESTING_GUARDIAN_AND_ALL_OTHER_RESERVES"

// This code does not coordinate a network upgrade. A signed witness does not
// establish that peers run the same binary/configuration; activation requires
// a separately authorized, coordinated rollout and must remain dormant until it.

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

	// Version 2 is an explicit trust-based checkpoint, not a historical
	// issuance proof. SnapshotRoot is an opaque, signed commitment: this code
	// does not claim it reconstructs or independently proves production state.
	// Authorities attest the declared figures under LPoDV2TrustAssumption.
	// ParentHash must be the canonical immediate parent at the signed
	// activation height.
	TrustAssumption      string        `json:"trust_assumption,omitempty"`
	ParentHash           crypto.Hash32 `json:"parent_hash,omitempty"`
	SnapshotRoot         crypto.Hash32 `json:"snapshot_root,omitempty"`
	HistoricalSaleRemain uint64        `json:"historical_sale_remaining_napro,string,omitempty"`

	// Version 3 is a trust-attested nominal allocation reclassification. It
	// explicitly declares an eligible nominal budget, not historical issuance
	// or a measured wallet balance. Exactly 1B APRO is reserved from this budget
	// in the activation block.
	NominalEligible uint64               `json:"eligible_nominal_napro,string,omitempty"`
	NominalSnapshot *LPoDNominalSnapshot `json:"nominal_snapshot,omitempty"`
}

// LPoDNominalSnapshot is an authority-attested accounting basis, not a
// historical issuance proof or a cryptographic scan of wallet UTXOs. Its
// finite enum fields make the declared eligibility rule stable across nodes.
type LPoDNominalSnapshot struct {
	Basis                          string `json:"basis"`
	NominalCirculatingBefore       uint64 `json:"nominal_circulating_before_napro,string"`
	EligibleNominal                uint64 `json:"eligible_nominal_napro,string"`
	PreexistingGuardianReservation uint64 `json:"preexisting_guardian_reservation_napro,string"`
	ValidatorRemaining             uint64 `json:"validator_remaining_napro,string"`
	EligibilityRule                string `json:"eligibility_rule"`
}

func (s LPoDNominalSnapshot) Root() crypto.Hash32 {
	raw, _ := json.Marshal(s)
	return crypto.HashBytes([]byte("aperod/lpod/nominal-reclassification/snapshot/v3"), raw)
}

func (s LPoDNominalSnapshot) valid(eligible, validatorRemaining uint64, root crypto.Hash32) bool {
	return s.Basis == LPoDV3NominalBasis &&
		s.NominalCirculatingBefore >= s.EligibleNominal &&
		s.NominalCirculatingBefore <= LPoDV3NominalCirculationMaxNAPRO &&
		s.EligibleNominal == eligible &&
		s.PreexistingGuardianReservation == 0 &&
		s.ValidatorRemaining == validatorRemaining &&
		s.EligibilityRule == LPoDV3EligibilityRule &&
		s.Root() == root
}

type LPoDAttestation struct {
	Validator crypto.ValidatorPubKey `json:"validator"`
	Signature []byte                 `json:"signature"`
}

func (m *LPoDMigration) AttestationMessage() crypto.Hash32 {
	if m != nil && m.Version == 2 {
		root := m.Root()
		return crypto.HashBytes([]byte("aperod/lpod/trusted-checkpoint/authorization/v2"), root[:])
	}
	if m != nil && m.Version == 3 {
		root := m.Root()
		return crypto.HashBytes([]byte("aperod/lpod/nominal-reclassification/authorization/v3"), root[:])
	}
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
	if m == nil || (m.Version != 1 && m.Version != 2 && m.Version != 3) || m.PositionLifecycleVersion != 1 || m.Height == 0 {
		return fmt.Errorf("lpod: invalid reconciliation authorization")
	}
	if m.Version == 2 {
		if _, err := m.canonicalV2Authorities(); err != nil {
			return err
		}
		if m.Root() != m.ReconciliationRoot {
			return fmt.Errorf("lpod: invalid trusted checkpoint commitment")
		}
		if m.Genesis == (crypto.Hash32{}) ||
			m.TrustAssumption != LPoDV2TrustAssumption ||
			m.ParentHash == (crypto.Hash32{}) || m.SnapshotRoot == (crypto.Hash32{}) ||
			m.HistoricalSaleRemain < lpod.InitialNAPRO ||
			m.HistoricalIssued > 9_000_000_000*lpod.Unit ||
			m.NominalEligible != 0 ||
			m.NominalSnapshot != nil ||
			m.ValidatorRemaining > 2_000_000_000*lpod.Unit ||
			m.HistoricalSaleRemain > 9_000_000_000*lpod.Unit-m.HistoricalIssued ||
			m.ValidatorRemaining > 9_000_000_000*lpod.Unit-m.HistoricalIssued-m.HistoricalSaleRemain {
			return fmt.Errorf("lpod: invalid trusted checkpoint declaration")
		}
		if len(m.Openings) != 0 || m.BodyRoot != (crypto.Hash32{}) {
			return fmt.Errorf("lpod: version 2 cannot contain version 1 history proof fields")
		}
		return m.verifyV2Attestations()
	}
	if m.Version == 3 {
		if _, err := m.canonicalV2Authorities(); err != nil {
			return err
		}
		if m.Root() != m.ReconciliationRoot {
			return fmt.Errorf("lpod: invalid nominal reclassification commitment")
		}
		publicAllocation := uint64(9_000_000_000) * lpod.Unit
		if m.Genesis == (crypto.Hash32{}) ||
			m.TrustAssumption != LPoDV3TrustAssumption ||
			m.ParentHash == (crypto.Hash32{}) || m.SnapshotRoot == (crypto.Hash32{}) ||
			m.NominalEligible < lpod.InitialNAPRO ||
			m.NominalEligible > LPoDV3NominalAllocationMaxNAPRO ||
			m.ValidatorRemaining > 2_000_000_000*lpod.Unit ||
			m.ValidatorRemaining > publicAllocation ||
			m.NominalEligible > publicAllocation-m.ValidatorRemaining ||
			m.HistoricalIssued != 0 || m.HistoricalSaleRemain != 0 ||
			len(m.Openings) != 0 || m.BodyRoot != (crypto.Hash32{}) ||
			m.NominalSnapshot == nil ||
			!m.NominalSnapshot.valid(m.NominalEligible, m.ValidatorRemaining, m.SnapshotRoot) {
			return fmt.Errorf("lpod: invalid nominal reclassification declaration")
		}
		return m.verifyV3Attestations()
	}
	if m.TrustAssumption != "" || m.ParentHash != (crypto.Hash32{}) ||
		m.SnapshotRoot != (crypto.Hash32{}) || m.HistoricalSaleRemain != 0 || m.NominalEligible != 0 ||
		m.NominalSnapshot != nil {
		return fmt.Errorf("lpod: version 1 authorization contains version 2 fields")
	}
	if m.Root() != m.ReconciliationRoot {
		return fmt.Errorf("lpod: invalid reconciliation authorization")
	}
	return m.verifyAttestations()
}

func (m *LPoDMigration) canonicalV2Authorities() ([]crypto.ValidatorPubKey, error) {
	authorities := append([]crypto.ValidatorPubKey(nil), m.TrustedValidators...)
	for _, p := range authorities {
		if len(p) != 32 || isZeroMigrationAuthority(p) {
			return nil, fmt.Errorf("lpod: missing or zero trusted checkpoint authority")
		}
	}
	sort.Slice(authorities, func(i, j int) bool {
		return bytes.Compare(authorities[i], authorities[j]) < 0
	})
	for i := 1; i < len(authorities); i++ {
		if bytes.Equal(authorities[i-1], authorities[i]) {
			return nil, fmt.Errorf("lpod: duplicate trusted checkpoint authority")
		}
	}
	if len(authorities) == 0 {
		return nil, fmt.Errorf("lpod: no trusted checkpoint authorities")
	}
	return authorities, nil
}

func (m *LPoDMigration) verifyV2Attestations() error {
	authorities, err := m.canonicalV2Authorities()
	if err != nil {
		return err
	}
	trusted := make(map[string]bool, len(authorities))
	for _, p := range authorities {
		trusted[string(p)] = true
	}
	seen := make(map[string]bool, len(m.Attestations))
	for _, a := range m.Attestations {
		id := string(a.Validator)
		if !trusted[id] || seen[id] || isZeroMigrationAuthority(a.Validator) ||
			!a.Validator.Verify(m.AttestationMessage(), a.Signature) {
			return fmt.Errorf("lpod: invalid or duplicate trusted checkpoint attestation")
		}
		seen[id] = true
	}
	if len(seen) <= 2*len(trusted)/3 {
		return fmt.Errorf("lpod: trusted checkpoint requires greater-than-two-thirds authority quorum")
	}
	return nil
}

func (m *LPoDMigration) verifyV3Attestations() error {
	authorities, err := m.canonicalV2Authorities()
	if err != nil {
		return err
	}
	trusted := make(map[string]bool, len(authorities))
	for _, p := range authorities {
		trusted[string(p)] = true
	}
	seen := make(map[string]bool, len(m.Attestations))
	for _, a := range m.Attestations {
		id := string(a.Validator)
		if !trusted[id] || seen[id] || isZeroMigrationAuthority(a.Validator) ||
			!a.Validator.Verify(m.AttestationMessage(), a.Signature) {
			return fmt.Errorf("lpod: invalid or duplicate nominal reclassification attestation")
		}
		seen[id] = true
	}
	if len(seen) <= 2*len(authorities)/3 {
		return fmt.Errorf("lpod: nominal reclassification requires greater-than-two-thirds authority quorum")
	}
	return nil
}

func isZeroMigrationAuthority(p crypto.ValidatorPubKey) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
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
	ValidatorRemaining        uint64               `json:"validator_remaining_napro,string"`
	InitialValidatorRemaining uint64               `json:"initial_validator_remaining_napro,string"`
	TailIssued                uint64               `json:"tail_issued_napro,string"`
	DeclaredSaleRemaining     uint64               `json:"declared_sale_remaining_napro,string,omitempty"`
	SnapshotRoot              crypto.Hash32        `json:"snapshot_root,omitempty"`
	FundingParent             crypto.Hash32        `json:"funding_parent,omitempty"`
	TrustAssumption           string               `json:"trust_assumption,omitempty"`
	NominalEligible           uint64               `json:"eligible_nominal_napro,string,omitempty"`
	NominalSnapshot           *LPoDNominalSnapshot `json:"nominal_snapshot,omitempty"`
}

// Root commits the complete reconciliation witness, activation height and actual
// genesis. A scheduled validator cannot choose a different witness on activation.
func (m *LPoDMigration) Root() crypto.Hash32 {
	if m != nil && m.Version == 3 {
		authorities, err := m.canonicalV2Authorities()
		if err != nil {
			return crypto.Hash32{}
		}
		b, _ := json.Marshal(struct {
			Version                  uint8
			PositionLifecycleVersion uint8
			Height                   uint64
			Genesis                  crypto.Hash32
			TrustedAuthorities       []crypto.ValidatorPubKey
			ParentHash               crypto.Hash32
			SnapshotRoot             crypto.Hash32
			TrustAssumption          string
			NominalEligible          uint64
			ValidatorRemaining       uint64
		}{m.Version, m.PositionLifecycleVersion, m.Height, m.Genesis, authorities,
			m.ParentHash, m.SnapshotRoot, m.TrustAssumption, m.NominalEligible, m.ValidatorRemaining})
		return crypto.HashBytes([]byte("aperod/lpod/nominal-reclassification/root/v3"), b)
	}
	if m != nil && m.Version == 2 {
		authorities, err := m.canonicalV2Authorities()
		if err != nil {
			return crypto.Hash32{}
		}
		b, _ := json.Marshal(struct {
			Version                  uint8
			PositionLifecycleVersion uint8
			Height                   uint64
			Genesis                  crypto.Hash32
			TrustedAuthorities       []crypto.ValidatorPubKey
			ParentHash               crypto.Hash32
			SnapshotRoot             crypto.Hash32
			TrustAssumption          string
			HistoricalIssued         uint64
			HistoricalSaleRemain     uint64
			ValidatorRemaining       uint64
		}{m.Version, m.PositionLifecycleVersion, m.Height, m.Genesis, authorities, m.ParentHash, m.SnapshotRoot,
			m.TrustAssumption, m.HistoricalIssued, m.HistoricalSaleRemain, m.ValidatorRemaining})
		return crypto.HashBytes([]byte("aperod/lpod/trusted-checkpoint/root/v2"), b)
	}
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
	if m != nil && m.Version == 2 {
		return 0, crypto.Hash32{}, fmt.Errorf("lpod: version 2 uses trusted checkpoint verification, not historical reconciliation")
	}
	if m != nil && m.Version == 3 {
		return 0, crypto.Hash32{}, fmt.Errorf("lpod: version 3 uses nominal reclassification verification, not historical reconciliation")
	}
	if m == nil || m.Version != 1 || m.PositionLifecycleVersion != 1 || m.Height == 0 || m.Genesis == (crypto.Hash32{}) ||
		m.Root() != m.ReconciliationRoot {
		return 0, crypto.Hash32{}, fmt.Errorf("lpod: invalid version, activation, genesis or reconciliation root")
	}
	if m.TrustAssumption != "" || m.ParentHash != (crypto.Hash32{}) ||
		m.SnapshotRoot != (crypto.Hash32{}) || m.HistoricalSaleRemain != 0 || m.NominalEligible != 0 ||
		m.NominalSnapshot != nil {
		return 0, crypto.Hash32{}, fmt.Errorf("lpod: version 1 reconciliation contains version 2 fields")
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

// verifyLPoDV2Genesis accepts the signed pre-oracle genesis identity without
// applying the modern header codec retroactively. This exception is confined
// to height zero and to the explicitly attested v2 genesis hash; it does not
// relax the v1 historical-body reconciliation.
func (d *DB) verifyLPoDV2Genesis(expected crypto.Hash32) error {
	if b, err := d.readLPoDCanonicalBlock(0); err == nil {
		if b.Hash() != expected {
			return fmt.Errorf("lpod: trusted checkpoint genesis differs from canonical genesis")
		}
		return nil
	}
	indexed, found, err := d.GetCanonicalHash(0)
	if err != nil || !found {
		return fmt.Errorf("lpod: trusted checkpoint genesis index unavailable: %v", err)
	}
	raw, err := d.GetRawBlock(indexed)
	if err != nil || raw == nil {
		return fmt.Errorf("lpod: trusted checkpoint genesis body unavailable: %v", err)
	}
	var b core.Block
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("lpod: trusted checkpoint genesis body invalid: %w", err)
	}
	h := &b.Header
	if h.Height != 0 || h.PrevHash != (crypto.Hash32{}) ||
		h.OraclePrice != 0 || h.BaseFee != 0 ||
		b.Hash() != expected || core.MerkleRoot(b.Txs) != h.MerkleRoot {
		return fmt.Errorf("lpod: trusted checkpoint genesis body does not match attested genesis")
	}
	height, timestamp, round := make([]byte, 8), make([]byte, 8), make([]byte, 4)
	binary.LittleEndian.PutUint64(height, h.Height)
	binary.LittleEndian.PutUint64(timestamp, uint64(h.Timestamp))
	binary.LittleEndian.PutUint32(round, h.Round)
	historical := crypto.HashBytes(height, h.PrevHash[:], h.MerkleRoot[:],
		timestamp, round, h.ValidatorPub)
	if historical != indexed || !h.ValidatorPub.Verify(historical, h.Signature) {
		return fmt.Errorf("lpod: trusted checkpoint genesis legacy index or signature invalid")
	}
	return nil
}

// VerifyLPoDMigration validates a proof without writing any funding state.
func (d *DB) VerifyLPoDMigration(m *LPoDMigration) (uint64, crypto.Hash32, error) {
	if m != nil && (m.Version == 2 || m.Version == 3) {
		if err := m.VerifyAuthorization(); err != nil {
			return 0, crypto.Hash32{}, err
		}
		parent, parentHeight, err := d.GetTip()
		if err != nil || parentHeight != m.Height-1 || parent != m.ParentHash {
			return 0, parent, fmt.Errorf("lpod: trusted checkpoint parent is not the canonical immediate parent")
		}
		b, err := d.readLPoDCanonicalBlock(parentHeight)
		if err != nil || b.Hash() != m.ParentHash {
			return 0, parent, fmt.Errorf("lpod: trusted checkpoint parent body is unavailable or noncanonical")
		}
		if err := d.verifyLPoDV2Genesis(m.Genesis); err != nil {
			return 0, parent, err
		}
		if m.Version == 3 {
			return 0, parent, nil
		}
		return m.HistoricalIssued, parent, nil
	}
	return ReconcileLPoD(m, d.readLPoDCanonicalBlock)
}

// appendLPoDMigration is called only in the same batch as the activation block.
// The binding survives rollback so a changed config cannot mint another reserve.
func (d *DB) appendLPoDMigration(batch *leveldb.Batch, hash crypto.Hash32, height uint64, m *LPoDMigration) (*LPoDCheckpoint, error) {
	if m == nil || height != m.Height {
		return nil, fmt.Errorf("lpod: migration at wrong height")
	}
	bindingKey := []byte("lpod/activation/v1")
	if m.Version == 2 {
		bindingKey = []byte("lpod/activation/v2")
	} else if m.Version == 3 {
		bindingKey = []byte("lpod/activation/v3")
	}
	binding, err := d.get(bindingKey)
	if err != nil {
		return nil, err
	}
	root := m.Root()
	for _, key := range [][]byte{[]byte("lpod/activation/v1"), []byte("lpod/activation/v2"), []byte("lpod/activation/v3")} {
		if string(key) == string(bindingKey) {
			continue
		}
		otherBinding, err := d.get(key)
		if err != nil {
			return nil, err
		}
		if otherBinding != nil {
			return nil, fmt.Errorf("lpod: conflicting migration version already bound")
		}
	}
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
	heightKey := []byte("lpod/activation_height/v1")
	poolKey := []byte("lpod/activation_validator_budget/v1")
	if m.Version == 2 {
		heightKey = []byte("lpod/activation_height/v2")
		poolKey = []byte("lpod/activation_validator_budget/v2")
	} else if m.Version == 3 {
		heightKey = []byte("lpod/activation_height/v3")
		poolKey = []byte("lpod/activation_validator_budget/v3")
	}
	batch.Put(heightKey, activation[:])
	var initialPool [8]byte
	binary.LittleEndian.PutUint64(initialPool[:], m.ValidatorRemaining)
	batch.Put(poolKey, initialPool[:])
	saleRemaining := 9_000_000_000*lpod.Unit - m.ValidatorRemaining - issued
	if m.Version == 2 {
		saleRemaining = m.HistoricalSaleRemain
	} else if m.Version == 3 {
		saleRemaining = m.NominalEligible
	}
	return &LPoDCheckpoint{
		State:   lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: lpod.InitialNAPRO, LastHeight: height - 1},
		Carries: map[string]lpod.Carry{},
		Allocation: &LPoDAllocation{
			Version: m.Version, PositionLifecycleVersion: m.PositionLifecycleVersion, Genesis: m.Genesis, FundingHeight: height, FundingBlock: hash,
			ReconciliationRoot: root, HistoricalIssued: issued,
			Remaining:                 saleRemaining - lpod.InitialNAPRO,
			ValidatorRemaining:        m.ValidatorRemaining,
			InitialValidatorRemaining: m.ValidatorRemaining,
			DeclaredSaleRemaining:     m.HistoricalSaleRemain,
			SnapshotRoot:              m.SnapshotRoot,
			FundingParent:             m.ParentHash,
			TrustAssumption:           m.TrustAssumption,
			NominalEligible:           m.NominalEligible,
			NominalSnapshot:           nominalSnapshotForAllocation(m),
		},
	}, nil
}

func nominalSnapshotForAllocation(m *LPoDMigration) *LPoDNominalSnapshot {
	if m == nil || m.Version != 3 {
		return nil
	}
	snapshot := *m.NominalSnapshot
	return &snapshot
}

func (d *DB) restoreLPoDPool(batch *leveldb.Batch, hash crypto.Hash32, height uint64) error {
	activation, err := d.get([]byte("lpod/activation_height/v1"))
	if err == nil && activation == nil {
		activation, err = d.get([]byte("lpod/activation_height/v2"))
	}
	if err == nil && activation == nil {
		activation, err = d.get([]byte("lpod/activation_height/v3"))
	}
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
		if err == nil && raw == nil {
			raw, err = d.get([]byte("lpod/activation_validator_budget/v2"))
		}
		if err == nil && raw == nil {
			raw, err = d.get([]byte("lpod/activation_validator_budget/v3"))
		}
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
