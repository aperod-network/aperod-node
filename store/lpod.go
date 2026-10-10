// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/syndtr/goleveldb/leveldb"
)

// LPoD checkpoints are immutable and keyed by canonical block hash. Selecting
// an earlier tip therefore selects that exact earlier reserve AND rounding
// carry, rather than an independently persisted mutable "remaining" counter.
// Initial funding requires a fully reconciled, quorum-attested allocation debit.
type LPoDCheckpoint struct {
	State              lpod.State              `json:"state"`
	Carries            map[string]lpod.Carry   `json:"carries"`
	Allocation         *LPoDAllocation         `json:"allocation,omitempty"`
	Positions          map[string]LPoDPosition `json:"positions,omitempty"`
	PrincipalDeposited uint64                  `json:"principal_deposited_napro,string"`
	PrincipalLocked    uint64                  `json:"principal_locked_napro,string"`
	PrincipalReturned  uint64                  `json:"principal_returned_napro,string"`
}

// LPoDSettlement is prepared from canonical registry/position state by consensus.
// Store commits verify its clock, operations, checkpoint and actual payout plan.
type LPoDSettlement struct {
	Parent           crypto.Hash32
	Vaults           []lpod.Vault
	Migration        *LPoDMigration
	PositionProtocol bool
	Timestamp        int64
	Proposer         string
	Leader           crypto.Address
	Stake            map[string]LPoDValidatorStake
	// PreviousStake is the established non-seeded routing projection before
	// stake operations. It is used only to rank destinations, never as a source
	// of funds.
	PreviousStake  map[string]LPoDValidatorStake
	RegistryBefore *core.RegistrySnapshot
	// RegistryAfter is captured after the canonical commit has applied block
	// stake operations. Both snapshots are auxiliary audit evidence only.
	RegistryAfter *core.RegistrySnapshot
	// AuditStake maps retain the complete registry projection, including
	// synthetic seeded entries. Stake/PreviousStake remain the established
	// non-seeded routing basis and must not change consensus eligibility.
	AuditPreviousStake map[string]LPoDValidatorStake
	AuditStake         map[string]LPoDValidatorStake
	Transactions       []core.Transaction
}

func lpodKey(hash crypto.Hash32) []byte {
	return append([]byte("lpod/checkpoint/v1/"), hash[:]...)
}

func (d *DB) lpodCheckpoint(hash crypto.Hash32) (*LPoDCheckpoint, error) {
	return d.lpodCheckpointBounded(hash, 0)
}

// LoadLPoDCheckpointAtBounded limits auxiliary decoding, without changing
// consensus checkpoint loading. LevelDB still materializes one value.
func (d *DB) LoadLPoDCheckpointAtBounded(hash crypto.Hash32, maxBytes int) (*LPoDCheckpoint, error) {
	return d.lpodCheckpointBounded(hash, maxBytes)
}

func (d *DB) lpodCheckpointBounded(hash crypto.Hash32, maxBytes int) (*LPoDCheckpoint, error) {
	return loadLPoDCheckpointBounded(
		hash, maxBytes, d.get, d.GetRawBlock, d.readLPoDCanonicalBlock,
		func(checkpointHash crypto.Hash32) (*LPoDCheckpoint, error) {
			return d.lpodCheckpoint(checkpointHash)
		},
	)
}

func loadLPoDCheckpointBounded(
	hash crypto.Hash32,
	maxBytes int,
	get func([]byte) ([]byte, error),
	getRawBlock func(crypto.Hash32) ([]byte, error),
	readCanonicalBlock func(uint64) (*core.Block, error),
	loadCheckpoint func(crypto.Hash32) (*LPoDCheckpoint, error),
) (*LPoDCheckpoint, error) {
	return loadLPoDCheckpointWithReadBudget(
		hash, maxBytes, nil, get, getRawBlock, readCanonicalBlock, loadCheckpoint,
	)
}

var errLPoDCheckpointReadBudget = errors.New("store: checkpoint exceeds auxiliary read budget")

type lpodCheckpointReadBudget struct {
	maxBytes int
	used     int
}

func (b *lpodCheckpointReadBudget) consume(size int) error {
	if b == nil || b.maxBytes <= 0 {
		return nil
	}
	if size > b.maxBytes-b.used {
		return errLPoDCheckpointReadBudget
	}
	b.used += size
	return nil
}

func loadLPoDCheckpointWithReadBudget(
	hash crypto.Hash32,
	maxBytes int,
	budget *lpodCheckpointReadBudget,
	get func([]byte) ([]byte, error),
	getRawBlock func(crypto.Hash32) ([]byte, error),
	readCanonicalBlock func(uint64) (*core.Block, error),
	loadCheckpoint func(crypto.Hash32) (*LPoDCheckpoint, error),
) (*LPoDCheckpoint, error) {
	data, err := get(lpodKey(hash))
	if err != nil || data == nil {
		return nil, err
	}
	if budget != nil {
		if err := budget.consume(len(data)); err != nil {
			return nil, err
		}
	} else if maxBytes > 0 && len(data) > maxBytes {
		return nil, fmt.Errorf("store: checkpoint exceeds auxiliary read budget")
	}
	var c LPoDCheckpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("store: corrupt LPoD checkpoint: %w", err)
	}
	if err := c.State.Validate(); err != nil {
		return nil, err
	}
	if err := c.validatePositions(); err != nil {
		return nil, err
	}
	if c.Allocation != nil {
		a := c.Allocation
		const publicAllocation = 9_000_000_000 * lpod.Unit
		const validatorAllocation = 2_000_000_000 * lpod.Unit
		if a.InitialValidatorRemaining > validatorAllocation ||
			a.InitialValidatorRemaining > publicAllocation-lpod.InitialNAPRO {
			return nil, fmt.Errorf("store: corrupt LPoD validator allocation bounds")
		}
		drawn := c.State.RewardInflow
		if drawn > a.InitialValidatorRemaining {
			drawn = a.InitialValidatorRemaining
		}
		validV1 := a.Version == 1 && a.DeclaredSaleRemaining == 0 &&
			a.SnapshotRoot == (crypto.Hash32{}) && a.FundingParent == (crypto.Hash32{}) && a.TrustAssumption == "" &&
			a.NominalEligible == 0 &&
			a.NominalSnapshot == nil &&
			a.HistoricalIssued <= publicAllocation-a.InitialValidatorRemaining-lpod.InitialNAPRO &&
			a.Remaining == publicAllocation-a.InitialValidatorRemaining-a.HistoricalIssued-lpod.InitialNAPRO
		validV2 := a.Version == 2 && a.DeclaredSaleRemaining >= lpod.InitialNAPRO &&
			a.NominalEligible == 0 &&
			a.NominalSnapshot == nil &&
			a.HistoricalIssued <= publicAllocation-a.InitialValidatorRemaining &&
			a.DeclaredSaleRemaining <= publicAllocation-a.InitialValidatorRemaining-a.HistoricalIssued &&
			a.Remaining == a.DeclaredSaleRemaining-lpod.InitialNAPRO &&
			a.SnapshotRoot != (crypto.Hash32{}) && a.FundingParent != (crypto.Hash32{}) &&
			a.Genesis != (crypto.Hash32{}) &&
			a.TrustAssumption == LPoDV2TrustAssumption
		validV3 := a.Version == 3 && a.DeclaredSaleRemaining == 0 && a.HistoricalIssued == 0 &&
			a.NominalEligible >= lpod.InitialNAPRO &&
			a.NominalEligible <= LPoDV3NominalAllocationMaxNAPRO &&
			a.NominalEligible <= publicAllocation-a.InitialValidatorRemaining &&
			a.Remaining == a.NominalEligible-lpod.InitialNAPRO &&
			a.SnapshotRoot != (crypto.Hash32{}) && a.FundingParent != (crypto.Hash32{}) &&
			a.Genesis != (crypto.Hash32{}) &&
			a.TrustAssumption == LPoDV3TrustAssumption &&
			a.NominalSnapshot != nil &&
			a.NominalSnapshot.valid(a.NominalEligible, a.InitialValidatorRemaining, a.SnapshotRoot)
		if (!validV1 && !validV2 && !validV3) || a.PositionLifecycleVersion != 1 ||
			a.FundingHeight == 0 || c.State.LastHeight < a.FundingHeight ||
			c.State.FundingDebit != lpod.InitialNAPRO ||
			a.ValidatorRemaining != a.InitialValidatorRemaining-drawn || a.TailIssued != c.State.RewardInflow-drawn {
			return nil, fmt.Errorf("store: corrupt LPoD conserved allocation")
		}
		raw, err := getRawBlock(hash)
		if err != nil {
			return nil, err
		}
		var block core.Block
		if json.Unmarshal(raw, &block) != nil || block.Hash() != hash ||
			block.Header.Height != c.State.LastHeight || len(block.Txs) < 2 {
			return nil, fmt.Errorf("store: funded checkpoint lacks its full committing block")
		}
		digest, err := block.Txs[1].LPoDCheckpointDigest()
		if err != nil || digest != c.Digest() {
			return nil, fmt.Errorf("store: checkpoint differs from signed block commitment")
		}
		if a.Version == 2 || a.Version == 3 {
			// The funding parent belongs to the original allocation block, not
			// every later checkpoint. Authenticate that original canonical block
			// and its checkpoint separately; later blocks are checked against
			// their own signed checkpoint above.
			funding, err := readCanonicalBlock(a.FundingHeight)
			if err != nil || funding.Hash() != a.FundingBlock || funding.Header.Height != a.FundingHeight ||
				funding.Header.PrevHash != a.FundingParent || len(funding.Txs) < 2 {
				return nil, fmt.Errorf("store: trusted checkpoint funding block is not canonical")
			}
			fundingCheckpoint := &c
			if hash != a.FundingBlock {
				fundingCheckpoint, err = loadCheckpoint(a.FundingBlock)
			}
			if errors.Is(err, errLPoDCheckpointReadBudget) {
				return nil, err
			}
			if err != nil || fundingCheckpoint == nil || fundingCheckpoint.Allocation == nil ||
				fundingCheckpoint.Allocation.Version != a.Version ||
				fundingCheckpoint.Allocation.FundingBlock != a.FundingBlock ||
				fundingCheckpoint.Allocation.FundingHeight != a.FundingHeight ||
				fundingCheckpoint.Allocation.FundingParent != a.FundingParent ||
				fundingCheckpoint.Allocation.ReconciliationRoot != a.ReconciliationRoot {
				return nil, fmt.Errorf("store: trusted checkpoint funding record is unavailable or inconsistent")
			}
			fundingDigest, err := funding.Txs[1].LPoDCheckpointDigest()
			if err != nil || fundingDigest != fundingCheckpoint.Digest() {
				return nil, fmt.Errorf("store: trusted funding block differs from its signed allocation commitment")
			}
		}
	}
	// Validate persisted carries even when the next block has no vault income.
	for id, carry := range c.Carries {
		if id == "" || carry.APR >= 100*lpod.YearSeconds || carry.Leader >= 100 {
			return nil, fmt.Errorf("store: corrupt LPoD rounding carry")
		}
	}
	return &c, nil
}

// LoadLPoDCheckpoint follows the durable canonical tip, not the highest stored
// checkpoint. Absence is unfunded, never a default 1B balance.
// Canonical writers and readers must use the chain transition lock.
func (d *DB) LoadLPoDCheckpoint() (*LPoDCheckpoint, error) {
	hash, height, err := d.GetTip()
	if err != nil {
		return nil, err
	}
	c, err := d.lpodCheckpoint(hash)
	if err == nil && c != nil && c.State.LastHeight != height {
		return nil, fmt.Errorf("store: LPoD height differs from canonical tip")
	}
	return c, err
}

// Digest excludes the funding block hash to avoid a circular block/Merkle hash.
// Height, genesis, reconciliation root and the entire resulting ledger are bound.
func (c LPoDCheckpoint) Digest() crypto.Hash32 {
	if c.Allocation != nil {
		a := *c.Allocation
		a.FundingBlock = crypto.Hash32{}
		c.Allocation = &a
		if a.Version == 1 {
			// Preserve the byte-for-byte legacy v1 checkpoint encoding. New v2
			// allocation fields are not part of the historical v1 commitment.
			legacy := struct {
				State              lpod.State              `json:"state"`
				Carries            map[string]lpod.Carry   `json:"carries"`
				Allocation         *legacyLPoDAllocation   `json:"allocation,omitempty"`
				Positions          map[string]LPoDPosition `json:"positions,omitempty"`
				PrincipalDeposited uint64                  `json:"principal_deposited_napro,string"`
				PrincipalLocked    uint64                  `json:"principal_locked_napro,string"`
				PrincipalReturned  uint64                  `json:"principal_returned_napro,string"`
			}{
				State: c.State, Carries: c.Carries, Positions: c.Positions,
				PrincipalDeposited: c.PrincipalDeposited, PrincipalLocked: c.PrincipalLocked,
				PrincipalReturned: c.PrincipalReturned,
				Allocation: &legacyLPoDAllocation{
					Version: a.Version, PositionLifecycleVersion: a.PositionLifecycleVersion, Genesis: a.Genesis,
					FundingHeight: a.FundingHeight, FundingBlock: a.FundingBlock,
					ReconciliationRoot: a.ReconciliationRoot, HistoricalIssued: a.HistoricalIssued,
					Remaining: a.Remaining, ValidatorRemaining: a.ValidatorRemaining,
					InitialValidatorRemaining: a.InitialValidatorRemaining, TailIssued: a.TailIssued,
				},
			}
			b, _ := json.Marshal(legacy)
			return crypto.HashBytes([]byte("aperod/lpod/checkpoint/v1"), b)
		}
	}
	b, _ := json.Marshal(c)
	return crypto.HashBytes([]byte("aperod/lpod/checkpoint/v1"), b)
}

type legacyLPoDAllocation struct {
	Version                   uint8         `json:"version"`
	PositionLifecycleVersion  uint8         `json:"position_lifecycle_version"`
	Genesis                   crypto.Hash32 `json:"genesis"`
	FundingHeight             uint64        `json:"funding_height"`
	FundingBlock              crypto.Hash32 `json:"funding_block"`
	ReconciliationRoot        crypto.Hash32 `json:"reconciliation_root"`
	HistoricalIssued          uint64        `json:"historical_issued_napro,string"`
	Remaining                 uint64        `json:"remaining_napro,string"`
	ValidatorRemaining        uint64        `json:"validator_remaining_napro,string"`
	InitialValidatorRemaining uint64        `json:"initial_validator_remaining_napro,string"`
	TailIssued                uint64        `json:"tail_issued_napro,string"`
}

func (d *DB) PreviewLPoD(height uint64, request *LPoDSettlement) (*LPoDCheckpoint, []lpod.Payment, error) {
	tip, _, err := d.GetTip()
	if err != nil || request == nil || request.Parent != tip {
		return nil, nil, fmt.Errorf("lpod: invalid preview parent")
	}
	before, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return nil, nil, err
	}
	if before == nil {
		before, err = d.appendLPoDMigration(new(leveldb.Batch), crypto.Hash32{}, height, request.Migration)
		if err != nil {
			return nil, nil, err
		}
	} else if request.Migration != nil {
		return nil, nil, fmt.Errorf("lpod: duplicate migration")
	}
	if request.PositionProtocol {
		return d.previewPositions(before, height, request)
	}
	next, carries, payments, err := lpod.Settle(before.State, before.Carries, height, request.Vaults)
	if err != nil {
		return nil, nil, err
	}
	allocation, err := lpodRewardDebit(before.Allocation, request.Vaults)
	if err != nil {
		return nil, nil, err
	}
	return &LPoDCheckpoint{State: next, Carries: carries, Allocation: allocation}, payments, nil
}

func lpodRewardDebit(before *LPoDAllocation, vaults []lpod.Vault) (*LPoDAllocation, error) {
	if before == nil {
		return nil, nil
	} // non-production arithmetic fixtures
	a := *before
	var income uint64
	for _, v := range vaults {
		if v.ActualIncome > ^uint64(0)-income {
			return nil, fmt.Errorf("lpod: income overflow")
		}
		income += v.ActualIncome
	}
	expected := LPoDRewardFor(a.ValidatorRemaining)
	if income != expected {
		return nil, fmt.Errorf("lpod: reward differs from authorized pool/tail schedule")
	}
	if a.ValidatorRemaining > 0 {
		a.ValidatorRemaining -= income
	} else {
		if income > ^uint64(0)-a.TailIssued {
			return nil, fmt.Errorf("lpod: tail issuance overflow")
		}
		a.TailIssued += income
	}
	return &a, nil
}

func LPoDRewardFor(remaining uint64) uint64 {
	if remaining == 0 {
		return lpod.Unit
	} // existing 1 APRO tail, separately recorded
	if remaining < 3*lpod.Unit {
		return remaining
	}
	return 3 * lpod.Unit
}

// CheckLPoDConfig prevents disabling/changing a committed fork after a restart,
// including after rolling back across the allocation block.
func (d *DB) CheckLPoDConfig(m *LPoDMigration) error {
	bound, err := d.get([]byte("lpod/activation/v1"))
	if err != nil {
		return err
	}
	boundV2, err := d.get([]byte("lpod/activation/v2"))
	if err != nil {
		return err
	}
	boundV3, err := d.get([]byte("lpod/activation/v3"))
	if err != nil {
		return err
	}
	if (bound != nil && boundV2 != nil) ||
		(bound != nil && boundV3 != nil) ||
		(boundV2 != nil && boundV3 != nil) {
		return fmt.Errorf("lpod: conflicting durable migration versions are bound")
	}
	if boundV3 != nil {
		if m == nil || m.Version != 3 {
			return fmt.Errorf("lpod: cannot disable or downgrade a bound version 3 activation")
		}
		root := m.Root()
		if string(boundV3) != string(root[:]) {
			return fmt.Errorf("lpod: cannot change a bound version 3 activation")
		}
		return nil
	}
	if boundV2 != nil {
		if m == nil || m.Version != 2 {
			return fmt.Errorf("lpod: cannot disable or downgrade a bound version 2 activation")
		}
		root := m.Root()
		if string(boundV2) != string(root[:]) {
			return fmt.Errorf("lpod: cannot change a bound version 2 activation")
		}
		return nil
	}
	if bound == nil {
		checkpoint, err := d.LoadLPoDCheckpoint()
		if err != nil {
			return err
		}
		if checkpoint != nil && checkpoint.Allocation != nil {
			if m == nil || checkpoint.Allocation.Version != m.Version {
				return fmt.Errorf("lpod: funded checkpoint has no matching durable activation binding")
			}
			root := m.Root()
			if checkpoint.Allocation.ReconciliationRoot != root {
				return fmt.Errorf("lpod: funded checkpoint has no matching durable activation binding")
			}
			return fmt.Errorf("lpod: funded checkpoint is missing its durable activation binding")
		}
		return nil
	}
	if m == nil || m.Version != 1 {
		return fmt.Errorf("lpod: cannot disable a bound activation")
	}
	root := m.Root()
	if string(bound) != string(root[:]) {
		return fmt.Errorf("lpod: cannot change a bound activation")
	}
	return nil
}

func (d *DB) validateLPoDBlock(data []byte, hash crypto.Hash32, height uint64, req []*LPoDSettlement) error {
	if len(req) == 0 {
		return nil
	}
	before, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return err
	}
	// Historical unit fixtures have no allocation; no production initializer
	// creates those. All real migrated checkpoints require full block evidence.
	if len(req) != 1 || req[0] == nil {
		return fmt.Errorf("lpod: invalid settlement request")
	}
	if req[0].Migration == nil && (before == nil || before.Allocation == nil) {
		return nil
	}
	var b core.Block
	if err := json.Unmarshal(data, &b); err != nil {
		return fmt.Errorf("lpod: full canonical block required: %w", err)
	}
	if b.Hash() != hash || b.Header.Height != height || b.Header.PrevHash != req[0].Parent ||
		b.Header.MerkleRoot != core.MerkleRoot(b.Txs) {
		return fmt.Errorf("lpod: block/settlement identity mismatch")
	}
	c, payments, err := d.PreviewLPoD(height, req[0])
	if err != nil {
		return err
	}
	if req[0].PositionProtocol {
		r := req[0]
		if r.Timestamp != b.Header.Timestamp || r.Proposer != b.Header.ValidatorPub.Hex() || len(b.Txs) < 2 {
			return fmt.Errorf("lpod: settlement does not bind canonical proposer and clock")
		}
		var operations []core.Transaction
		for i := 2; i < len(b.Txs); i++ {
			tx := b.Txs[i]
			if (tx.IsCoinbase() && !tx.IsStake()) || tx.IsLPoD() || tx.IsLPoDPayout() || tx.IsGuardianFund() {
				return fmt.Errorf("lpod: extra protocol payout or mint")
			}
			operations = append(operations, tx)
		}
		want, _ := json.Marshal(operations)
		got, _ := json.Marshal(r.Transactions)
		if string(want) != string(got) {
			return fmt.Errorf("lpod: operations omitted from settlement")
		}
		payout, err := d.PayoutLPoD(height, r, c, payments)
		if err != nil {
			return err
		}
		want, _ = json.Marshal(payout)
		got, _ = json.Marshal(b.Txs[0])
		if string(want) != string(got) {
			return fmt.Errorf("lpod: real payout outputs differ from canonical plan")
		}
		digest, err := b.Txs[1].LPoDCheckpointDigest()
		if err != nil || digest != c.Digest() {
			return fmt.Errorf("lpod: checkpoint does not commit position transition")
		}
		return nil
	}
	return fmt.Errorf("lpod: funded blocks require the authenticated position protocol")
}

func (d *DB) LoadLPoDCheckpointAt(hash crypto.Hash32) (*LPoDCheckpoint, error) {
	return d.lpodCheckpoint(hash)
}

// appendLPoDSettlement initializes only through the attested allocation
// migration, and otherwise extends the exact canonical parent's checkpoint.
func (d *DB) appendLPoDSettlement(batch *leveldb.Batch, hash crypto.Hash32, height uint64, settlement []*LPoDSettlement) error {
	if len(settlement) > 1 {
		return fmt.Errorf("store: multiple LPoD settlements")
	}
	parent, _, err := d.GetTip()
	if err != nil {
		return err
	}
	before, err := d.LoadLPoDCheckpoint()
	if err != nil {
		return err
	}
	if len(settlement) == 0 {
		if before != nil {
			return fmt.Errorf("store: funded LPoD ledger requires settlement on every block")
		}
		return nil
	}
	if settlement[0] == nil || settlement[0].Parent != parent {
		return fmt.Errorf("store: LPoD parent does not match durable tip")
	}
	if before == nil {
		before, err = d.appendLPoDMigration(batch, hash, height, settlement[0].Migration)
		if err != nil {
			return err
		}
	} else if settlement[0].Migration != nil {
		return fmt.Errorf("store: duplicate LPoD initial allocation")
	}
	next, carries, _, err := lpod.Settle(before.State, before.Carries, height, settlement[0].Vaults)
	if settlement[0].PositionProtocol {
		next, carries = before.State, before.Carries
		err = nil
	}
	if err != nil {
		return err
	}
	allocation, err := lpodRewardDebit(before.Allocation, settlement[0].Vaults)
	if settlement[0].PositionProtocol {
		allocation = before.Allocation
		err = nil
	}
	if err != nil {
		return err
	}
	checkpoint := &LPoDCheckpoint{State: next, Carries: carries, Allocation: allocation}
	if settlement[0].PositionProtocol {
		checkpoint, _, err = d.previewPositions(before, height, settlement[0])
		if err != nil {
			return err
		}
		allocation = checkpoint.Allocation
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	previous, err := d.get(lpodKey(hash))
	if err != nil {
		return err
	}
	if previous != nil && string(previous) != string(data) {
		return fmt.Errorf("store: conflicting LPoD checkpoint for block")
	}
	batch.Put(lpodKey(hash), data)
	if allocation != nil {
		var remaining [8]byte
		binary.LittleEndian.PutUint64(remaining[:], allocation.ValidatorRemaining)
		batch.Put(append(append([]byte{}, prefixMeta...), []byte("staking_pool_remaining")...), remaining[:])
	}
	return nil
}
