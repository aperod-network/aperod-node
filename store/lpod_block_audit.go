// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

const lpodBlockAuditVersion = 3
const lpodBlockAuditMaxBytes = 4 << 20

var lpodBlockAuditPrefix = []byte("lpod/block-audit/v3/")
var lpodBlockAuditStartKey = []byte("lpod/block-audit-start/v3")

// LPoDAuditVault records the authenticated stake basis and settlement result for
// one canonical vault. Every monetary amount is encoded as a decimal JSON string.
type LPoDAuditVault struct {
	ID               string `json:"id"`
	ValidatorStake   uint64 `json:"validator_stake_napro,string"`
	GuardianStake    uint64 `json:"guardian_stake_napro,string"`
	TotalStake       uint64 `json:"total_stake_napro,string"`
	TierStake        uint64 `json:"tier_stake_napro,string"`
	APRPercent       uint64 `json:"apr_percent"`
	LeaderPercent    uint64 `json:"leader_percent"`
	Accrued          uint64 `json:"accrued_napro,string"`
	ActualLeaderPaid uint64 `json:"actual_leader_paid_napro,string"`
	GuardianPaid     uint64 `json:"guardian_paid_napro,string"`
	Unfunded         uint64 `json:"unfunded_napro,string"`
}

// LPoDAuditPosition captures one position's block-local accrual, payment and
// principal return, without treating the projection as consensus state.
type LPoDAuditPosition struct {
	ID                  string `json:"id"`
	Beneficiary         string `json:"beneficiary"`
	SignedVault         string `json:"signed_vault"`
	EffectiveVault      string `json:"effective_vault"`
	EffectiveVaultAfter string `json:"effective_vault_after"`
	DueBefore           uint64 `json:"due_before_napro,string"`
	Accrued             uint64 `json:"accrued_napro,string"`
	Paid                uint64 `json:"paid_napro,string"`
	DueAfter            uint64 `json:"due_after_napro,string"`
	PrincipalReturned   uint64 `json:"principal_returned_napro,string"`
	APRCarryBefore      uint64 `json:"apr_carry_before,string"`
	APRCarryAfter       uint64 `json:"apr_carry_after,string"`
}

// LPoDAuditOutputRef points to an actual output of the canonical payout
// transaction. Amount is the reconstructed transparent accounting amount.
type LPoDAuditOutputRef struct {
	TransactionHash crypto.Hash32 `json:"transaction_hash"`
	OutputIndex     uint32        `json:"output_index"`
	Beneficiary     string        `json:"beneficiary"`
	Amount          uint64        `json:"amount_napro,string"`
}

// LPoDBlockAudit is explicitly auxiliary evidence. It is not part of consensus
// validation, a block digest, a checkpoint digest, or transaction encoding.
type LPoDBlockAudit struct {
	Version                    uint8                         `json:"version"`
	Available                  bool                          `json:"available"`
	UnavailableReason          string                        `json:"unavailable_reason,omitempty"`
	Height                     uint64                        `json:"height"`
	BlockHash                  crypto.Hash32                 `json:"block_hash"`
	ParentHash                 crypto.Hash32                 `json:"parent_hash"`
	FundingHeight              uint64                        `json:"funding_height"`
	FundingGenesis             crypto.Hash32                 `json:"funding_genesis"`
	FundingRoot                crypto.Hash32                 `json:"funding_root"`
	HeaderTimestamp            int64                         `json:"header_timestamp"`
	BeforeCheckpointDigest     crypto.Hash32                 `json:"before_checkpoint_digest"`
	AfterCheckpointDigest      crypto.Hash32                 `json:"after_checkpoint_digest"`
	RegistryBefore             *core.RegistrySnapshot        `json:"registry_before,omitempty"`
	RegistryAfter              *core.RegistrySnapshot        `json:"registry_after,omitempty"`
	PreviousStake              map[string]LPoDValidatorStake `json:"previous_stake,omitempty"`
	Stake                      map[string]LPoDValidatorStake `json:"stake,omitempty"`
	PayoutTransactionHash      crypto.Hash32                 `json:"payout_transaction_hash"`
	Vaults                     []LPoDAuditVault              `json:"vaults,omitempty"`
	Positions                  []LPoDAuditPosition           `json:"positions,omitempty"`
	Outputs                    []LPoDAuditOutputRef          `json:"outputs,omitempty"`
	ProtocolBaseFeeBurnNAPRO   *uint64                       `json:"protocol_base_fee_burn_napro,string,omitempty"`
	SignedIntentionalBurnNAPRO *uint64                       `json:"signed_intentional_burn_napro,string,omitempty"`
	AVMGasBurnNAPRO            *uint64                       `json:"avm_gas_burn_napro,string,omitempty"`
	TotalBurnNAPRO             *uint64                       `json:"total_burn_napro,string,omitempty"`
}

// LPoDAuditStart identifies the first block for which a forward-only audit
// record was persisted. The record may explicitly be unavailable; it does not
// imply historical coverage.
type LPoDAuditStart struct {
	Height        uint64        `json:"height"`
	Hash          crypto.Hash32 `json:"hash"`
	ParentHash    crypto.Hash32 `json:"parent_hash"`
	FundingHeight uint64        `json:"funding_height"`
	Genesis       crypto.Hash32 `json:"genesis"`
	Root          crypto.Hash32 `json:"root"`
}

func lpodBlockAuditKey(hash crypto.Hash32) []byte {
	return append(append([]byte(nil), lpodBlockAuditPrefix...), hash[:]...)
}

// LoadLPoDAuditAt is a read-only hash-keyed lookup. It never scans, backfills,
// or follows the current tip.
func (d *DB) LoadLPoDAuditAt(hash crypto.Hash32) (*LPoDBlockAudit, error) {
return d.LoadLPoDAuditAtBounded(hash, 0)
}

// LoadLPoDAuditAtBounded bounds JSON decoding in auxiliary readers.
func (d *DB) LoadLPoDAuditAtBounded(hash crypto.Hash32, maxBytes int) (*LPoDBlockAudit, error) {
	raw, err := d.get(lpodBlockAuditKey(hash))
	if err != nil || raw == nil {
		return nil, err
	}
if maxBytes > 0 && len(raw) > maxBytes {
return nil, fmt.Errorf("store: block audit exceeds auxiliary read budget")
}
	var record LPoDBlockAudit
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("store: corrupt LPoD block audit: %w", err)
	}
	if record.Version != lpodBlockAuditVersion || record.BlockHash != hash {
		return nil, fmt.Errorf("store: inconsistent LPoD block audit identity")
	}
	return &record, nil
}

// LoadLPoDAuditStart returns the explicit forward-start marker, if any.
func (d *DB) LoadLPoDAuditStart() (*LPoDAuditStart, bool, error) {
	raw, err := d.get(lpodBlockAuditStartKey)
	if err != nil || raw == nil {
		return nil, false, err
	}
	const markerSize = 8 + 32 + 32 + 8 + 32 + 32
	if len(raw) != markerSize {
		return nil, false, fmt.Errorf("store: malformed LPoD audit start marker")
	}
	start := &LPoDAuditStart{Height: binary.BigEndian.Uint64(raw[:8])}
	offset := 8
	copy(start.Hash[:], raw[offset:offset+32])
	offset += 32
	copy(start.ParentHash[:], raw[offset:offset+32])
	offset += 32
	start.FundingHeight = binary.BigEndian.Uint64(raw[offset : offset+8])
	offset += 8
	copy(start.Genesis[:], raw[offset:offset+32])
	offset += 32
	copy(start.Root[:], raw[offset:offset+32])
	return start, true, nil
}

func encodeLPoDAuditStart(start LPoDAuditStart) []byte {
	const markerSize = 8 + 32 + 32 + 8 + 32 + 32
	value := make([]byte, markerSize)
	binary.BigEndian.PutUint64(value[:8], start.Height)
	offset := 8
	copy(value[offset:offset+32], start.Hash[:])
	offset += 32
	copy(value[offset:offset+32], start.ParentHash[:])
	offset += 32
	binary.BigEndian.PutUint64(value[offset:offset+8], start.FundingHeight)
	offset += 8
	copy(value[offset:offset+32], start.Genesis[:])
	offset += 32
	copy(value[offset:offset+32], start.Root[:])
	return value
}

func auditStartFromRecord(record LPoDBlockAudit) LPoDAuditStart {
	return LPoDAuditStart{
		Height: record.Height, Hash: record.BlockHash, ParentHash: record.ParentHash,
		FundingHeight: record.FundingHeight, Genesis: record.FundingGenesis, Root: record.FundingRoot,
	}
}

func sameLPoDAuditFundingIdentity(a, b LPoDBlockAudit) bool {
	return a.FundingHeight == b.FundingHeight &&
		a.FundingGenesis == b.FundingGenesis && a.FundingRoot == b.FundingRoot
}

// canonicalLPoDAuditStart verifies that a marker still points to a proven
// canonical audit row and its full canonical block body. An unavailable audit
// row is still a valid row here: it records an explicit evidence gap.
func (d *DB) canonicalLPoDAuditStart(start *LPoDAuditStart) (bool, error) {
	if start == nil {
		return false, nil
	}
	_, tipHeight, err := d.GetTip()
	if err != nil {
		return false, err
	}
	if start.Height > tipHeight {
		return false, nil
	}
	canonicalHash, found, err := d.GetCanonicalHash(start.Height)
	if err != nil || !found || canonicalHash != start.Hash {
		return false, err
	}
	record, err := d.LoadLPoDAuditAt(start.Hash)
	if err != nil {
		return false, err
	}
	if record == nil || record.Height != start.Height ||
		record.ParentHash != start.ParentHash ||
		record.FundingHeight != start.FundingHeight ||
		record.FundingGenesis != start.Genesis || record.FundingRoot != start.Root {
		return false, nil
	}
	block, err := d.readLPoDCanonicalBlock(start.Height)
	if err != nil {
		return false, nil
	}
	if block.Hash() != start.Hash || block.Header.Height != start.Height ||
		block.Header.PrevHash != start.ParentHash {
		return false, nil
	}
	if start.Height > 0 {
		parentHash, parentFound, err := d.GetCanonicalHash(start.Height - 1)
		if err != nil || !parentFound || parentHash != start.ParentHash {
			return false, err
		}
	}
	return true, nil
}

// PersistLPoDAuditAt writes a non-consensus forward projection after a block
// has committed. Callers should treat its error as a warning: canonical commit
// has already succeeded and is intentionally not modified by this API.
//
// The checkpoint and block are fetched by their accepted hashes, and every
// amount is recomputed from the parent checkpoint and prepared settlement.
// avmGasBurnActivationHeight is supplied from node configuration.
func (d *DB) PersistLPoDAuditAt(block *core.Block, settlement *LPoDSettlement, avmGasBurnActivationHeight uint64) error {
	if block == nil || settlement == nil {
		return fmt.Errorf("lpod audit: block and prepared settlement are required")
	}
	height, hash := block.Header.Height, block.Hash()
	canonical, found, err := d.GetCanonicalHash(height)
	if err != nil {
		return err
	}
	if !found || canonical != hash {
		return fmt.Errorf("lpod audit: block hash is not canonical at height %d", height)
	}
	rawBlock, err := d.GetRawBlock(hash)
	if err != nil {
		return err
	}
	canonicalBlock, err := decodeLPoDAuditBlock(rawBlock)
	if err != nil {
		return fmt.Errorf("lpod audit: canonical full block body unavailable: %w", err)
	}
	if canonicalBlock.Hash() != hash || !reflect.DeepEqual(canonicalBlock, block) {
		return fmt.Errorf("lpod audit: supplied block differs from canonical full body")
	}
	if block.Header.MerkleRoot != core.MerkleRoot(block.Txs) ||
		block.Header.Height == 0 || block.Header.PrevHash != settlement.Parent ||
		block.Header.Timestamp != settlement.Timestamp {
		return fmt.Errorf("lpod audit: malformed block or settlement identity")
	}
	if settlement.Proposer != block.Header.ValidatorPub.Hex() {
		return fmt.Errorf("lpod audit: prepared proposer does not match canonical header")
	}
	parentBlock, err := d.readLPoDCanonicalBlock(height - 1)
	if err != nil {
		return err
	}
	if parentBlock.Hash() != block.Header.PrevHash {
		return fmt.Errorf("lpod audit: parent block hash mismatch")
	}
	parentHash, parentFound, err := d.GetCanonicalHash(height - 1)
	if err != nil || !parentFound || parentHash != block.Header.PrevHash {
		return fmt.Errorf("lpod audit: parent hash is not canonical")
	}
	after, err := d.LoadLPoDCheckpointAt(hash)
	if err != nil {
		return err
	}
	if after == nil || after.State.LastHeight != height || len(block.Txs) < 2 {
		return fmt.Errorf("lpod audit: canonical checkpoint or commitment transaction missing")
	}
	afterDigest, err := block.Txs[1].LPoDCheckpointDigest()
	if err != nil || afterDigest != after.Digest() {
		return fmt.Errorf("lpod audit: after-checkpoint digest mismatch")
	}
	before, err := d.LoadLPoDCheckpointAt(block.Header.PrevHash)
	if err != nil {
		return err
	}
	if before == nil && settlement.Migration != nil {
		before, err = lpodAuditInitialCheckpoint(after, height, settlement.Migration)
		if err != nil {
			return err
		}
	}
	if before == nil {
		return fmt.Errorf("lpod audit: canonical parent checkpoint unavailable")
	}

	record := LPoDBlockAudit{
		Version: lpodBlockAuditVersion, Height: height, BlockHash: hash,
		ParentHash: block.Header.PrevHash, HeaderTimestamp: block.Header.Timestamp,
		BeforeCheckpointDigest: before.Digest(), AfterCheckpointDigest: after.Digest(),
		RegistryBefore: settlement.RegistryBefore, RegistryAfter: settlement.RegistryAfter,
		PreviousStake: settlement.AuditPreviousStake, Stake: settlement.AuditStake,
	}
	if after.Allocation != nil {
		record.FundingHeight = after.Allocation.FundingHeight
		record.FundingGenesis = after.Allocation.Genesis
		record.FundingRoot = after.Allocation.ReconciliationRoot
	}
	if settlement.RegistryBefore == nil || settlement.RegistryAfter == nil {
		record.UnavailableReason = "trusted validator registry snapshots unavailable"
		return d.persistLPoDBlockAudit(record)
	}
	if err := validateLPoDRegistryEvidence(settlement); err != nil {
		record.UnavailableReason = "trusted validator registry evidence inconsistent: " + err.Error()
		return d.persistLPoDBlockAudit(record)
	}
	if record.FundingHeight == 0 || record.FundingGenesis == (crypto.Hash32{}) ||
		record.FundingRoot == (crypto.Hash32{}) {
		record.UnavailableReason = "trusted funding baseline unavailable"
		return d.persistLPoDBlockAudit(record)
	}
	if settlement.Migration != nil &&
		(record.FundingHeight != height || settlement.Migration.Height != height ||
			record.FundingGenesis != settlement.Migration.Genesis ||
			record.FundingRoot != settlement.Migration.Root()) {
		record.UnavailableReason = "first trusted funding baseline does not match canonical activation"
		return d.persistLPoDBlockAudit(record)
	}
	burns, burnErr := deriveLPoDBlockBurns(block, avmGasBurnActivationHeight)
	if burnErr != nil {
		record.UnavailableReason = "burn evidence unavailable: " + burnErr.Error()
		return d.persistLPoDBlockAudit(record)
	}
	record.ProtocolBaseFeeBurnNAPRO = auditAmountPointer(burns.ProtocolBaseFee)
	record.SignedIntentionalBurnNAPRO = auditAmountPointer(burns.SignedIntentional)
	record.AVMGasBurnNAPRO = auditAmountPointer(burns.AVMGas)
	record.TotalBurnNAPRO = auditAmountPointer(burns.Total)
	// Non-position protocol settlements remain visible as explicitly unavailable
	// evidence instead of being mistaken for an empty, complete projection.
	if !settlement.PositionProtocol || before.Allocation == nil || after.Allocation == nil {
		record.UnavailableReason = "unsupported or unfunded settlement protocol"
		return d.persistLPoDBlockAudit(record)
	}
	if len(before.Positions) > LPoDMaxPositions || len(after.Positions) > LPoDMaxPositions {
		record.UnavailableReason = "position count exceeds projection limit"
		return d.persistLPoDBlockAudit(record)
	}

	accrued, derivedVaults, entitlementVault, err := d.positionTransition(before, height, settlement)
	if err != nil {
		return fmt.Errorf("lpod audit: derive pre-payment positions: %w", err)
	}
	if len(settlement.Vaults) != 0 && !reflect.DeepEqual(settlement.Vaults, derivedVaults) {
		return fmt.Errorf("lpod audit: supplied vault amounts differ from authenticated derivation")
	}
	nextState, nextCarries, payments, err := lpod.Settle(before.State, before.Carries, height, derivedVaults)
	if err != nil {
		return err
	}
	nextAllocation, err := lpodRewardDebit(before.Allocation, derivedVaults)
	if err != nil {
		return err
	}
	expectedAfter := *accrued
	expectedAfter.Positions = make(map[string]LPoDPosition, len(accrued.Positions))
	for id, position := range accrued.Positions {
		expectedAfter.Positions[id] = position
	}
	expectedAfter.State, expectedAfter.Carries, expectedAfter.Allocation = nextState, nextCarries, nextAllocation
	// Match the deterministic proportional distribution used by the canonical
	// position payout transition, without calling PayoutLPoD (which reads tip).
	ids := make([]string, 0, len(expectedAfter.Positions))
	for id := range expectedAfter.Positions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, payment := range payments {
		var due uint64
		for _, id := range ids {
			p := expectedAfter.Positions[id]
			if entitlementVault[id] == payment.VaultID {
				due, err = lpodAdd(due, p.Due)
				if err != nil {
					return err
				}
			}
		}
		left := payment.Angels
		for _, id := range ids {
			p := expectedAfter.Positions[id]
			if entitlementVault[id] != payment.VaultID || p.Due == 0 {
				continue
			}
			partBig := new(big.Int).Mul(new(big.Int).SetUint64(left), new(big.Int).SetUint64(p.Due))
			partBig.Div(partBig, new(big.Int).SetUint64(due))
			if !partBig.IsUint64() {
				return fmt.Errorf("lpod audit: proportional payment overflows")
			}
			part := partBig.Uint64()
			if part > left {
				return fmt.Errorf("lpod audit: invalid proportional payment")
			}
			left -= part
			due -= p.Due
			p.Due -= part
			expectedAfter.Positions[id] = p
		}
		if left != 0 {
			return fmt.Errorf("lpod audit: position payment does not conserve")
		}
	}
	if err := expectedAfter.validatePositions(); err != nil {
		return err
	}
	if expectedAfter.Digest() != after.Digest() {
		return fmt.Errorf("lpod audit: recomputed checkpoint differs from canonical after checkpoint")
	}

	vaultPayments := make(map[string]lpod.Payment, len(payments))
	var sumLeader, sumGuardian, sumUnfunded, sumAccrued, sumArrears uint64
	for _, p := range payments {
		vaultPayments[p.VaultID] = p
		sumLeader, err = lpodAdd(sumLeader, p.Leader)
		if err != nil {
			return err
		}
		sumGuardian, err = lpodAdd(sumGuardian, p.Angels)
		if err != nil {
			return err
		}
		sumUnfunded, err = lpodAdd(sumUnfunded, p.Unfunded)
		if err != nil {
			return err
		}
	}
	for _, v := range derivedVaults {
		sumAccrued, err = lpodAdd(sumAccrued, derefAccrual(v.ExactAccrual))
		if err != nil {
			return err
		}
		sumArrears, err = lpodAdd(sumArrears, v.Arrears)
		if err != nil {
			return err
		}
		tier, tierErr := lpod.TierFor(v.TotalStake)
		if tierErr != nil {
			return tierErr
		}
		stake := settlement.AuditStake[v.ID]
		if prior := settlement.AuditPreviousStake[v.ID]; !stake.Active && prior.Active {
			stake = prior
		}
		totalStake, err := lpodAdd(stake.Amount, v.GuardianStake)
		if err != nil {
			return err
		}
		pay := vaultPayments[v.ID]
		record.Vaults = append(record.Vaults, LPoDAuditVault{
			ID: v.ID, ValidatorStake: stake.Amount, GuardianStake: v.GuardianStake,
			TotalStake: totalStake, TierStake: v.TotalStake, APRPercent: tier.APRPercent, LeaderPercent: tier.LeaderPercent,
			Accrued: derefAccrual(v.ExactAccrual), ActualLeaderPaid: pay.Leader,
			GuardianPaid: pay.Angels, Unfunded: pay.Unfunded,
		})
	}
	if after.State.AccruedLiability < before.State.AccruedLiability ||
		after.State.AngelPaid < before.State.AngelPaid || after.State.LeaderPaid < before.State.LeaderPaid ||
		sumAccrued != after.State.AccruedLiability-before.State.AccruedLiability ||
		sumGuardian != after.State.AngelPaid-before.State.AngelPaid ||
		sumLeader != after.State.LeaderPaid-before.State.LeaderPaid {
		return fmt.Errorf("lpod audit: per-vault payments do not reconcile state deltas")
	}
	leftUnfunded, err := lpodAdd(after.State.UnfundedLiability, sumArrears)
	if err != nil {
		return err
	}
	rightUnfunded, err := lpodAdd(before.State.UnfundedLiability, sumUnfunded)
	if err != nil {
		return err
	}
	if leftUnfunded != rightUnfunded {
		return fmt.Errorf("lpod audit: unfunded vault totals do not reconcile")
	}

	positionIDs := make(map[string]bool)
	for id := range before.Positions {
		positionIDs[id] = true
	}
	for id := range accrued.Positions {
		positionIDs[id] = true
	}
	for id := range after.Positions {
		positionIDs[id] = true
	}
	orderedPositionIDs := make([]string, 0, len(positionIDs))
	for id := range positionIDs {
		orderedPositionIDs = append(orderedPositionIDs, id)
	}
	sort.Strings(orderedPositionIDs)
	var positionAccrued, positionPaid, positionPrincipal uint64
	beneficiaryAmounts := make(map[crypto.Address]uint64)
	addAmount := func(address crypto.Address, amount uint64) error {
		var addErr error
		beneficiaryAmounts[address], addErr = lpodAdd(beneficiaryAmounts[address], amount)
		return addErr
	}
	for _, id := range orderedPositionIDs {
		prior, priorOK := before.Positions[id]
		pre, preOK := accrued.Positions[id]
		post, postOK := after.Positions[id]
		if !preOK && !postOK {
			continue
		}
		if !preOK {
			pre = post
		}
		if !postOK {
			post.Due = 0
			post.Withdrawn = pre.Withdrawn
			post.APRCarry = pre.APRCarry
			post.Deposit = pre.Deposit
		}
		dueBefore := uint64(0)
		carryBefore := uint64(0)
		if priorOK {
			dueBefore, carryBefore = prior.Due, prior.APRCarry
		}
		if pre.Due < dueBefore || post.Due > pre.Due || post.Withdrawn < func() uint64 {
			if priorOK {
				return prior.Withdrawn
			}
			return 0
		}() {
			return fmt.Errorf("lpod audit: invalid position accounting delta")
		}
		accrualAmount, paidAmount := pre.Due-dueBefore, pre.Due-post.Due
		principal := post.Withdrawn
		if priorOK {
			principal -= prior.Withdrawn
		}
		positionAccrued, err = lpodAdd(positionAccrued, accrualAmount)
		if err != nil {
			return err
		}
		positionPaid, err = lpodAdd(positionPaid, paidAmount)
		if err != nil {
			return err
		}
		positionPrincipal, err = lpodAdd(positionPrincipal, principal)
		if err != nil {
			return err
		}
		vaultAfter := lpodPositionVaultID(post)
		vaultBefore := vaultAfter
		if priorOK {
			vaultBefore = lpodPositionVaultID(prior)
		}
		record.Positions = append(record.Positions, LPoDAuditPosition{
			ID: id, Beneficiary: string(post.Deposit.Beneficiary), SignedVault: lpodVaultID(post.Deposit),
			EffectiveVault: vaultBefore, EffectiveVaultAfter: vaultAfter,
			DueBefore: dueBefore, Accrued: accrualAmount, Paid: paidAmount, DueAfter: post.Due,
			PrincipalReturned: principal, APRCarryBefore: carryBefore, APRCarryAfter: pre.APRCarry,
		})
		if paidAmount+principal < paidAmount {
			return fmt.Errorf("lpod audit: output amount overflow")
		}
		if err := addAmount(post.Deposit.Beneficiary, paidAmount+principal); err != nil {
			return err
		}
	}
	if after.State.AccruedLiability < before.State.AccruedLiability ||
		after.State.AngelPaid < before.State.AngelPaid ||
		positionAccrued != after.State.AccruedLiability-before.State.AccruedLiability ||
		positionPaid != after.State.AngelPaid-before.State.AngelPaid {
		return fmt.Errorf("lpod audit: position totals do not reconcile checkpoint state (accrued %d/%d paid %d/%d)",
			positionAccrued, after.State.AccruedLiability-before.State.AccruedLiability,
			positionPaid, after.State.AngelPaid-before.State.AngelPaid)
	}
	if after.PrincipalReturned < before.PrincipalReturned ||
		positionPrincipal != after.PrincipalReturned-before.PrincipalReturned {
		return fmt.Errorf("lpod audit: position principal does not reconcile checkpoint delta")
	}
	for _, payment := range payments {
		if payment.Leader > 0 {
			if payment.VaultID != settlement.Proposer {
				return fmt.Errorf("lpod audit: non-proposer leader payment")
			}
			if err := addAmount(settlement.Leader, payment.Leader); err != nil {
				return err
			}
		}
	}

	payout := core.Transaction{Version: core.TxVersionLPoDPayout}
	payout.Extra, err = json.Marshal(core.LPoDPayoutAuthorization{Leader: settlement.Leader, Digest: after.Digest()})
	if err != nil {
		return err
	}
	addresses := make([]string, 0, len(beneficiaryAmounts))
	for address, amount := range beneficiaryAmounts {
		if amount > 0 {
			addresses = append(addresses, string(address))
		}
	}
	sort.Strings(addresses)
	for _, text := range addresses {
		address := crypto.Address(text)
		out, buildErr := core.BuildLPoDPayoutOutput(address, beneficiaryAmounts[address], height, settlement.Parent, after.Digest())
		if buildErr != nil {
			return buildErr
		}
		payout.Outputs = append(payout.Outputs, out)
	}
	if !reflect.DeepEqual(block.Txs[0], payout) {
		return fmt.Errorf("lpod audit: actual payout transaction differs from reconstructed outputs")
	}
	record.PayoutTransactionHash = block.Txs[0].Hash()
	for i, address := range addresses {
		record.Outputs = append(record.Outputs, LPoDAuditOutputRef{
			TransactionHash: record.PayoutTransactionHash, OutputIndex: uint32(i),
			Beneficiary: address, Amount: beneficiaryAmounts[crypto.Address(address)],
		})
	}
	record.Available = true
	return d.persistLPoDBlockAudit(record)
}

type lpodBlockBurns struct {
	ProtocolBaseFee   uint64
	SignedIntentional uint64
	AVMGas            uint64
	Total             uint64
}

// deriveLPoDBlockBurns mirrors the block-response accounting algorithm, but
// checks uint64 aggregation overflow so an auxiliary evidence gap is explicit.
func deriveLPoDBlockBurns(block *core.Block, avmGasBurnActivationHeight uint64) (lpodBlockBurns, error) {
	if block == nil {
		return lpodBlockBurns{}, fmt.Errorf("missing full block body")
	}
	baseFee := block.Header.BaseFee
	baseFeeUnproven := false
	if baseFee == 0 {
		for i := range block.Txs {
			tx := &block.Txs[i]
			if !tx.IsCoinbase() && !tx.IsStake() && tx.Fee > 0 {
				baseFeeUnproven = true
				break
			}
		}
	}
	burnAVMGas := avmGasBurnActivationHeight > 0 && block.Header.Height >= avmGasBurnActivationHeight
	burnMarker := []byte("APRO-BURN\x01")
	for i := range block.Txs {
		tx := &block.Txs[i]
		hasBurnMarker := bytes.HasPrefix(tx.Extra, burnMarker)
		_, isBurn := tx.BurnAmount()
		if hasBurnMarker && !isBurn {
			return lpodBlockBurns{}, fmt.Errorf("transaction %d has invalid intentional burn marker", i)
		}
		if tx.IsCoinbase() || tx.IsStake() {
			if hasBurnMarker {
				return lpodBlockBurns{}, fmt.Errorf("transaction %d carries an invalid intentional burn marker", i)
			}
			continue
		}
		if burnAVMGas && tx.IsAVM() {
			if tx.AVM == nil {
				return lpodBlockBurns{}, fmt.Errorf("transaction %d has missing AVM payload", i)
			}
			if _, err := core.AVMGasFee(tx.AVM.GasLimit); err != nil {
				return lpodBlockBurns{}, fmt.Errorf("transaction %d has invalid AVM gas fee: %w", i, err)
			}
		}
	}
	if baseFeeUnproven {
		return lpodBlockBurns{}, fmt.Errorf("zero header base fee and canonical parent-derived fee is unavailable")
	}

	var burns lpodBlockBurns
	for i := range block.Txs {
		tx := &block.Txs[i]
		if tx.IsCoinbase() || tx.IsStake() {
			continue
		}
		intentional, isBurn := tx.BurnAmount()
		size := tx.Size()
		if size < 0 {
			return lpodBlockBurns{}, fmt.Errorf("transaction %d has invalid negative size", i)
		}
		minimumBig := new(big.Int).Mul(new(big.Int).SetUint64(baseFee), big.NewInt(int64(size)))
		feeBig := new(big.Int).SetUint64(tx.Fee)
		protocolFee := tx.Fee
		var err error
		if minimumBig.Cmp(feeBig) <= 0 {
			protocolFee = minimumBig.Uint64()
			if isBurn {
				burns.SignedIntentional, err = lpodAdd(burns.SignedIntentional, intentional)
				if err != nil {
					return lpodBlockBurns{}, fmt.Errorf("signed intentional burn sum: %w", err)
				}
			}
		}
		burns.ProtocolBaseFee, err = lpodAdd(burns.ProtocolBaseFee, protocolFee)
		if err != nil {
			return lpodBlockBurns{}, fmt.Errorf("protocol base fee sum: %w", err)
		}
		if burnAVMGas && tx.IsAVM() {
			gasFee, gasErr := core.AVMGasFee(tx.AVM.GasLimit)
			if gasErr != nil {
				return lpodBlockBurns{}, fmt.Errorf("transaction %d has invalid AVM gas fee: %w", i, gasErr)
			}
			burns.AVMGas, err = lpodAdd(burns.AVMGas, gasFee)
			if err != nil {
				return lpodBlockBurns{}, fmt.Errorf("AVM gas burn sum: %w", err)
			}
		}
	}
	total, err := lpodAdd(burns.ProtocolBaseFee, burns.SignedIntentional)
	if err != nil {
		return lpodBlockBurns{}, fmt.Errorf("total burn sum: %w", err)
	}
	burns.Total, err = lpodAdd(total, burns.AVMGas)
	if err != nil {
		return lpodBlockBurns{}, fmt.Errorf("total burn sum: %w", err)
	}
	return burns, nil
}

func auditAmountPointer(value uint64) *uint64 {
	return &value
}

// validateLPoDRegistryEvidence proves the prepared pre-operation maps are the
// exact projection of the full trusted snapshot, then independently replays
// only block withdrawals into Stake. Deposits/top-ups are deliberately not
// projected: the settlement map is a pre-operation ranking basis for those.
func validateLPoDRegistryEvidence(settlement *LPoDSettlement) error {
	if settlement == nil || settlement.RegistryBefore == nil || settlement.RegistryAfter == nil {
		return fmt.Errorf("missing registry snapshot")
	}
	before := settlement.RegistryBefore
	after := settlement.RegistryAfter
	if before.Validators == nil || after.Validators == nil {
		return fmt.Errorf("registry snapshot has no validator map")
	}
	projected := make(map[string]LPoDValidatorStake, len(before.Validators))
	for _, entry := range before.Validators {
		if entry == nil {
			return fmt.Errorf("registry snapshot contains a nil validator")
		}
		id := entry.PubKey.Hex()
		if _, exists := projected[id]; exists {
			return fmt.Errorf("registry snapshot contains duplicate validator identity")
		}
		projected[id] = LPoDValidatorStake{Amount: entry.StakeNAPR, Active: entry.Status == core.ValidatorActive}
	}
	if !reflect.DeepEqual(projected, settlement.AuditPreviousStake) {
		return fmt.Errorf("previous stake map differs from pre-operation registry")
	}
	expectedStake := make(map[string]LPoDValidatorStake, len(projected))
	for id, stake := range projected {
		expectedStake[id] = stake
	}
	for _, tx := range settlement.Transactions {
		if !tx.IsStake() {
			continue
		}
		var action core.StakeAction
		var pub crypto.ValidatorPubKey
		var amount uint64
		switch len(tx.Extra) {
		case core.StakePayloadSize:
			var err error
			action, pub, amount, _, err = core.DecodeStakeExtra(tx.Extra)
			if err != nil {
				return fmt.Errorf("decode stake operation: %w", err)
			}
		case core.StakeWithdrawalPayloadSizeV2:
			authorization, err := core.DecodeStakeWithdrawalExtraV2(tx.Extra)
			if err != nil {
				return fmt.Errorf("decode stake withdrawal: %w", err)
			}
			action, pub, amount = authorization.Action, authorization.PubKey, authorization.Amount
		default:
			continue // deposit/top-up payload; not manually adjusted in LPoD
		}
		if action != core.StakeWithdraw && action != core.StakePartialWithdraw {
			continue // a stake deposit/top-up is reflected only in RegistryAfter
		}
		id := pub.Hex()
		stake, exists := expectedStake[id]
		if !exists {
			return fmt.Errorf("withdrawal validator %s is absent from pre-operation registry", id)
		}
		switch action {
		case core.StakeWithdraw:
			stake.Active = false
		case core.StakePartialWithdraw:
			if amount > stake.Amount {
				return fmt.Errorf("partial withdrawal exceeds projected stake")
			}
			stake.Amount -= amount
			if stake.Amount < core.MinStakeNAPR {
				stake.Active = false
			}
		}
		expectedStake[id] = stake
	}
	if !reflect.DeepEqual(expectedStake, settlement.AuditStake) {
		return fmt.Errorf("stake map differs from manually projected withdrawals")
	}
	return nil
}

func lpodAuditInitialCheckpoint(after *LPoDCheckpoint, height uint64, migration *LPoDMigration) (*LPoDCheckpoint, error) {
	if after == nil || after.Allocation == nil || migration == nil || height == 0 || migration.Height != height {
		return nil, fmt.Errorf("lpod audit: migration baseline unavailable")
	}
	a := *after.Allocation
	var saleRemaining uint64
	switch migration.Version {
	case 1:
		const publicAllocation = uint64(9_000_000_000 * lpod.Unit)
		if migration.ValidatorRemaining > publicAllocation ||
			migration.HistoricalIssued > publicAllocation-migration.ValidatorRemaining {
			return nil, fmt.Errorf("lpod audit: invalid version 1 migration allocation")
		}
		saleRemaining = publicAllocation - migration.ValidatorRemaining - migration.HistoricalIssued
	case 2:
		saleRemaining = migration.HistoricalSaleRemain
	case 3:
		saleRemaining = migration.NominalEligible
	default:
		return nil, fmt.Errorf("lpod audit: unsupported migration version")
	}
	if saleRemaining < lpod.InitialNAPRO {
		return nil, fmt.Errorf("lpod audit: migration allocation below funding debit")
	}
	if a.FundingHeight != height || a.FundingBlock == (crypto.Hash32{}) ||
		a.Version != migration.Version || a.PositionLifecycleVersion != migration.PositionLifecycleVersion ||
		a.Genesis != migration.Genesis || a.ReconciliationRoot != migration.Root() ||
		a.HistoricalIssued != migration.HistoricalIssued ||
		a.InitialValidatorRemaining != migration.ValidatorRemaining ||
		a.Remaining != saleRemaining-lpod.InitialNAPRO ||
		a.DeclaredSaleRemaining != migration.HistoricalSaleRemain ||
		a.SnapshotRoot != migration.SnapshotRoot || a.FundingParent != migration.ParentHash ||
		a.TrustAssumption != migration.TrustAssumption || a.NominalEligible != migration.NominalEligible ||
		!reflect.DeepEqual(a.NominalSnapshot, nominalSnapshotForAllocation(migration)) {
		return nil, fmt.Errorf("lpod audit: migration does not match committed initial allocation")
	}
	a.ValidatorRemaining = a.InitialValidatorRemaining
	a.TailIssued = 0
	return &LPoDCheckpoint{
		State:   lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: lpod.InitialNAPRO, LastHeight: height - 1},
		Carries: map[string]lpod.Carry{}, Allocation: &a,
		Positions: map[string]LPoDPosition{},
	}, nil
}

func derefAccrual(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

func (d *DB) persistLPoDBlockAudit(record LPoDBlockAudit) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > lpodBlockAuditMaxBytes {
		record.Available = false
		record.UnavailableReason = "projection exceeds 4 MiB storage limit"
		record.Vaults, record.Positions, record.Outputs = nil, nil, nil
		record.RegistryBefore, record.RegistryAfter = nil, nil
		record.PreviousStake, record.Stake = nil, nil
		record.PayoutTransactionHash = crypto.Hash32{}
		raw, err = json.Marshal(record)
		if err != nil {
			return err
		}
		if len(raw) > lpodBlockAuditMaxBytes {
			return fmt.Errorf("lpod audit: minimal unavailable record exceeds storage limit")
		}
	}
	key := lpodBlockAuditKey(record.BlockHash)
	existing, err := d.get(key)
	if err != nil {
		return err
	}
	if existing != nil {
		if !bytes.Equal(existing, raw) {
			return fmt.Errorf("lpod audit: conflicting record already exists for block hash")
		}
	}
	batch := new(leveldb.Batch)
	if existing == nil {
		batch.Put(key, raw)
	}
	start, err := d.get(lpodBlockAuditStartKey)
	if err != nil {
		return err
	}
	needsMarker := start == nil
	if start != nil {
		currentStart, found, err := d.LoadLPoDAuditStart()
		if err != nil {
			// A malformed marker cannot establish any prior coverage. The
			// current record is the only safe point from which to resume.
			needsMarker = true
		} else {
			markerValid := false
			if found {
				markerValid, err = d.canonicalLPoDAuditStart(currentStart)
				if err != nil {
					return err
				}
			}
			if markerValid {
				markerRecord := LPoDBlockAudit{
					FundingHeight:  currentStart.FundingHeight,
					FundingGenesis: currentStart.Genesis,
					FundingRoot:    currentStart.Root,
				}
				needsMarker = !sameLPoDAuditFundingIdentity(markerRecord, record)
			} else {
				needsMarker = true
			}
		}
	}
	if needsMarker {
		// Never walk the chain from the funding height here: this callback runs
		// on every canonical commit. A lost or orphaned marker means earlier
		// coverage is no longer claimed; forward coverage restarts at this
		// successfully committed record and retains its funding identity.
		batch.Put(lpodBlockAuditStartKey, encodeLPoDAuditStart(auditStartFromRecord(record)))
	}
	if len(batch.Dump()) == 0 {
		return nil
	}
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}
