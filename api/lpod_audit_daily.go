// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

const (
	lpodDailyMaxBlocks  = uint64(40000)
	lpodDailyMaxEntries = 100000
	lpodDailyWorkBudget = 20 * time.Second
)

type lpodDailyPosition struct {
	ID                  string `json:"id"`
	EffectiveVault      string `json:"effective_vault"`
	EffectiveVaultAfter string `json:"effective_vault_after"`
	Accrued             string `json:"accrued_napro"`
	Paid                string `json:"paid_napro"`
	PrincipalReturned   string `json:"principal_returned_napro"`
}

type lpodDailyVault struct {
	ID               string `json:"id"`
	Accrued          string `json:"accrued_napro"`
	ActualLeaderPaid string `json:"actual_leader_paid_napro"`
	GuardianPaid     string `json:"guardian_paid_napro"`
	Unfunded         string `json:"unfunded_napro"`
}

type lpodDailyPositionSum struct {
	id, vault, vaultAfter    string
	accrued, paid, principal uint64
}
type lpodDailyVaultSum struct {
	id                                  string
	accrued, leader, guardian, unfunded uint64
}

func (s *Server) restLPoDAuditDaily(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	dateText := q.Get("date")
	if len(q) != 1 || len(q["date"]) != 1 || strings.TrimSpace(dateText) != dateText {
		writeJSONError(w, http.StatusBadRequest, "exactly one date=YYYY-MM-DD query parameter is required")
		return
	}
	date, err := time.Parse("2006-01-02", dateText)
	if err != nil || date.Format("2006-01-02") != dateText {
		writeJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	if !s.lpodAuditDailyMu.TryLock() {
		writeJSONError(w, http.StatusTooManyRequests, "daily LPoD audit is already running")
		return
	}
	defer s.lpodAuditDailyMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), lpodDailyWorkBudget)
	defer cancel()
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Europe/Moscow timezone unavailable")
		return
	}
	end := time.Date(date.Year(), date.Month(), date.Day(), 20, 0, 0, 0, loc)
	startDate := date.AddDate(0, 0, -1)
	start := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 20, 0, 0, 0, loc)
	out := map[string]interface{}{
		"version": 1, "date": dateText, "status": "incomplete", "reason": "",
		"verification_level": "operator_attested",
		"trust_assumption":   "Validator registry snapshots are operator-attested auxiliary evidence, not an independent on-chain proof; the first included block's pre-operation snapshot is trusted as the opening baseline, with no claim about pre-interval registry history.",
		"interval_start":     start.Format(time.RFC3339Nano), "interval_end": end.Format(time.RFC3339Nano),
		"opening_anchor": nil, "closing_witness": nil, "certificate": nil, "funding": nil,
		"coverage": map[string]interface{}{"blocks": 0, "vaults": 0, "positions": 0},
		"vaults":   []lpodDailyVault{}, "positions": []lpodDailyPosition{},
		"burns_napro": map[string]string{
			"protocol_base_fee": "0", "signed_intentional": "0", "avm_gas": "0", "total": "0",
		},
	}
	fail := func(reason string) {
out["status"] = "incomplete"
		out["reason"] = reason
		writeJSON(w, http.StatusOK, out)
	}
	if err := ctx.Err(); err != nil {
		fail(dailyAuditContextReason(err))
		return
	}
	if s.blockStore == nil {
		fail("block store unavailable")
		return
	}
	tipHash, tipHeight, err := s.blockStore.GetTip()
	if err != nil {
		fail("canonical tip unavailable")
		return
	}
	cert, err := s.blockStore.LoadFinalityCertificate()
	if err != nil || cert == nil {
		fail("durable finality certificate unavailable")
		return
	}
progress, background := r.Context().Value(dailyProgressKey{}).(*dailyProgress)
if background && progress.Certificate != nil {
if !s.dailyBindingValid(progress) {
fail("saved canonical certificate or finality binding invalid")
return
}
// Continue through today's latest verified certificate. On restart the
// oracle need not retain the old certificate; the resumed signed ancestry
// must connect the saved predecessor all the way to the current oracle.
}
	if s.lpodFinalized == nil || !s.lpodFinalized(cert.Height, cert.BlockHash) {
		fail("finality certificate is not verified by the configured oracle")
		return
	}
	if cert.Height > tipHeight {
		fail("certificate height is beyond the canonical tip")
		return
	}
	canonicalCert, found, err := s.blockStore.GetCanonicalHash(cert.Height)
	if err != nil || !found || canonicalCert != cert.BlockHash {
		fail("certificate block is not canonical")
		return
	}
	marker, markerFound, err := s.blockStore.LoadLPoDAuditStart()
	if err != nil {
		fail("LPoD audit coverage marker unavailable")
		return
	}
	if !markerFound || marker == nil {
		fail("LPoD audit coverage marker missing")
		return
	}
markerBytes, markerErr := json.Marshal(marker)
if markerErr != nil { fail("coverage marker cannot be encoded"); return }
markerDigest := crypto.HashBytes(markerBytes)

	readBlock := func(height uint64) (*core.Block, crypto.Hash32, error) {
		if err := ctx.Err(); err != nil {
			return nil, crypto.Hash32{}, err
		}
		hash, found, readErr := s.blockStore.GetCanonicalHash(height)
		if readErr != nil || !found {
			return nil, crypto.Hash32{}, fmt.Errorf("canonical hash missing at height %d", height)
		}
		raw, readErr := s.blockStore.GetRawBlock(hash)
		if readErr != nil || len(raw) == 0 {
			return nil, crypto.Hash32{}, fmt.Errorf("full block body unavailable at height %d", height)
		}
if len(raw) > dailyJobMaxBytes { return nil, crypto.Hash32{}, fmt.Errorf("block exceeds daily verifier byte bound") }
		var block core.Block
		if readErr = json.Unmarshal(raw, &block); readErr != nil {
			return nil, crypto.Hash32{}, fmt.Errorf("invalid block body at height %d", height)
		}
		if block.Header.Height != height || block.Hash() != hash || block.Header.MerkleRoot != core.MerkleRoot(block.Txs) ||
			!block.Header.VerifySignature() {
			return nil, crypto.Hash32{}, fmt.Errorf("canonical block identity or Merkle proof invalid at height %d", height)
		}
		return &block, hash, nil
	}
	readTimestampProbe := func(height uint64) (int64, error) {
		hash, found, err := s.blockStore.GetCanonicalHash(height)
		if err != nil || !found {
			return 0, fmt.Errorf("canonical hash missing at timestamp probe height %d", height)
		}
		raw, err := s.blockStore.GetRawBlock(hash)
		if err != nil || len(raw) == 0 {
			return 0, fmt.Errorf("canonical block unavailable at timestamp probe height %d", height)
		}
if len(raw) > dailyJobMaxBytes { return 0, fmt.Errorf("timestamp probe exceeds daily verifier byte bound") }
		var block core.Block
		if err := json.Unmarshal(raw, &block); err == nil &&
			block.Header.Height == height && block.Hash() == hash &&
			block.Header.MerkleRoot == core.MerkleRoot(block.Txs) && block.Header.VerifySignature() {
			return block.Header.Timestamp, nil
		}
		// Pruned StoredBlock metadata is unsigned and is only a binary-search
		// hint. It must identify the canonical indexed block and carry an explicit
		// timestamp; signed full bodies at the selected bounds and throughout the
		// audited range remain mandatory.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return 0, fmt.Errorf("timestamp probe height %d has malformed stored-block metadata", height)
		}
		present := func(want string) bool {
			for key, value := range fields {
				if strings.EqualFold(key, want) &&
					len(bytes.TrimSpace(value)) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return true
				}
			}
			return false
		}
		if !present("height") || !present("hash") || !present("timestamp") {
			return 0, fmt.Errorf("timestamp probe height %d has incomplete stored-block metadata", height)
		}
		var stored store.StoredBlock
		if err := json.Unmarshal(raw, &stored); err != nil ||
			stored.Height != height || stored.Hash != hash {
			return 0, fmt.Errorf("timestamp probe height %d stored-block identity differs from canonical index", height)
		}
		return stored.Timestamp, nil
	}

	// Signed block timestamps are strictly increasing on accepted ancestry.
	// Use canonical, signature-verified probes to locate each lower bound, then
	// independently verify every body, link and timestamp from the opening
	// anchor through the certificate below. Restrict probes to the most recent
	// certified 40,000-height window; an older interval cannot be proven here.
	pruneCursor := uint64(0)
	pruneCursorRaw, cursorErr := s.blockStore.GetMeta("prune_cursor")
	if cursorErr != nil {
		fail("prune cursor metadata unavailable")
		return
	}
	if pruneCursorRaw != nil {
		if len(pruneCursorRaw) != 8 {
			fail("prune cursor metadata is malformed")
			return
		}
		pruneCursor = binary.LittleEndian.Uint64(pruneCursorRaw)
	}
	if pruneCursor == ^uint64(0) {
		fail("prune cursor overflows searchable height")
		return
	}
	if pruneCursorRaw != nil {
		if pruneCursor > cert.Height {
			fail("prune cursor is beyond the certified range")
			return
		}
		cursorTimestamp, timestampErr := readTimestampProbe(pruneCursor)
		if timestampErr != nil {
			fail(fmt.Sprintf("prune cursor timestamp metadata unavailable: %s", timestampErr))
			return
		}
		if !time.Unix(0, cursorTimestamp).Before(start) {
			fail("prune cursor has reached the opening interval; its anchor may be pruned")
			return
		}
	}
	searchFloor := pruneCursor + 1
	if cert.Height >= lpodDailyMaxBlocks {
		certifiedFloor := cert.Height - lpodDailyMaxBlocks + 2
		if certifiedFloor > searchFloor {
			searchFloor = certifiedFloor
		}
	}
	if searchFloor > cert.Height {
		fail("pruned retention window does not overlap the certified range")
		return
	}
	firstAtOrAfter := func(target time.Time) (uint64, *core.Block, bool, error) {
		low, high := searchFloor, cert.Height
		for low < high {
			if err := ctx.Err(); err != nil {
				return 0, nil, false, err
			}
			mid := low + (high-low)/2
			probeTimestamp, readErr := readTimestampProbe(mid)
			if readErr != nil {
				return 0, nil, false, fmt.Errorf(
					"timestamp boundary probe at height %d cannot be authenticated: standalone signed-header metadata is unavailable (%v)",
					mid, readErr)
			}
			if time.Unix(0, probeTimestamp).Before(target) {
				low = mid + 1
			} else {
				high = mid
			}
		}
		if err := ctx.Err(); err != nil {
			return 0, nil, false, err
		}
		block, _, readErr := readBlock(low)
		if readErr != nil {
			return 0, nil, false, readErr
		}
		if time.Unix(0, block.Header.Timestamp).Before(target) {
			return 0, nil, false, nil
		}
		return low, block, true, nil
	}
	openingHeight, openingBlock, openingFound, boundaryErr := firstAtOrAfter(start)
	if boundaryErr != nil {
		if ctx.Err() != nil {
			fail(dailyAuditContextReason(ctx.Err()))
		} else {
			fail(boundaryErr.Error())
		}
		return
	}
	if !openingFound || openingHeight == 0 {
		fail("opening anchor is unavailable")
		return
	}
	anchorHeight := openingHeight - 1
	anchorBlock, anchorHash, readErr := readBlock(anchorHeight)
	if readErr != nil {
		if ctx.Err() != nil {
			fail(dailyAuditContextReason(ctx.Err()))
		} else {
			fail(readErr.Error())
		}
		return
	}
	if !time.Unix(0, anchorBlock.Header.Timestamp).Before(start) ||
		openingBlock.Header.PrevHash != anchorHash {
		fail("opening boundary is discontinuous")
		return
	}
	witnessHeight, witnessBlock, witnessFound, boundaryErr := firstAtOrAfter(end)
	if boundaryErr != nil {
		if ctx.Err() != nil {
			fail(dailyAuditContextReason(ctx.Err()))
		} else {
			fail(boundaryErr.Error())
		}
		return
	}
	if !witnessFound {
		tipBlock, _, tipReadErr := readBlock(tipHeight)
		if tipReadErr == nil && !time.Unix(0, tipBlock.Header.Timestamp).Before(end) {
			fail("verified certificate predates the closing witness")
		} else {
			fail("closing witness not yet present")
		}
		return
	}
	witnessHash := witnessBlock.Hash()
	witnessTimestamp := witnessBlock.Header.Timestamp
	anchorTimestamp := anchorBlock.Header.Timestamp
batchBinding := &dailyProgress{
Certificate: cert, Anchor: anchorHash, Witness: witnessHash,
AnchorHeight: anchorHeight, WitnessHeight: witnessHeight, Marker: markerDigest, Prune: pruneCursorRaw,
}
	out["opening_anchor"] = map[string]interface{}{
		"height": anchorHeight, "hash": fmt.Sprintf("%x", anchorHash[:]),
		"header_timestamp": anchorTimestamp, "timestamp": time.Unix(0, anchorTimestamp).Format(time.RFC3339Nano),
	}
	out["closing_witness"] = map[string]interface{}{
		"height": witnessHeight, "hash": fmt.Sprintf("%x", witnessHash[:]),
		"header_timestamp": witnessTimestamp, "timestamp": time.Unix(0, witnessTimestamp).Format(time.RFC3339Nano),
	}
	if markerFound && marker != nil && marker.Height > anchorHeight+1 {
		fail("LPoD audit coverage starts after the interval opening")
		return
	}
	if markerFound && marker != nil {
		markerHash, markerCanonical, markerErr := s.blockStore.GetCanonicalHash(marker.Height)
		if markerErr != nil || !markerCanonical || markerHash != marker.Hash {
			fail("LPoD audit coverage marker is not canonical")
			return
		}
	}
	if cert.Height < witnessHeight {
		fail("verified certificate predates the closing witness")
		return
	}
	if cert.Height-anchorHeight+1 > lpodDailyMaxBlocks {
		fail("certified ancestry exceeds 40000-block bound")
		return
	}

	// Forward verify the opening-to-certificate ancestry, auditing every block
	// in the requested half-open interval, including blocks with no activity.
	prevHash := crypto.Hash32{}
	var priorTS int64
	var previousBlock *core.Block
	if anchorHeight > 0 {
		parentHash, found, hashErr := s.blockStore.GetCanonicalHash(anchorHeight - 1)
		if hashErr != nil || !found {
			fail("canonical opening-anchor parent hash unavailable")
			return
		}
		prevHash = parentHash
	}
	checkpointCache := make(map[crypto.Hash32]*store.LPoDCheckpoint, 2)
	vaults := make(map[string]*lpodDailyVaultSum)
	positions := make(map[string]*lpodDailyPositionSum)
	var burnBase, burnIntentional, burnAVM, burnTotal uint64
	var included uint64
	var lastAuditDigest crypto.Hash32
	var haveLastAuditDigest bool
	firstIncluded, lastIncluded := uint64(0), uint64(0)
	var firstTimestamp, lastTimestamp int64
	var expectedNextRegistry *core.RegistrySnapshot
nextHeight := anchorHeight
if background && progress.Next != 0 {
if progress.Anchor != anchorHash || progress.Witness != witnessHash || progress.Marker != markerDigest ||
!bytes.Equal(progress.Prune, pruneCursorRaw) || progress.Next <= anchorHeight || cert.Height == ^uint64(0) || progress.Next > cert.Height+1 {
fail("saved boundary or prune binding changed")
return
}
previousHash, found, err := s.blockStore.GetCanonicalHash(progress.Next-1)
if err != nil || !found || previousHash != progress.Previous {
fail("saved progress predecessor is no longer canonical")
return
}
nextHeight, prevHash, priorTS = progress.Next, progress.Previous, progress.PriorTS
previousBlock, _, err = readBlock(nextHeight-1)
if err != nil { fail("saved predecessor body unavailable"); return }
for id, v := range progress.Vaults {
vaults[id] = &lpodDailyVaultSum{id: id, accrued: v[0], leader: v[1], guardian: v[2], unfunded: v[3]}
}
for id, p := range progress.Positions {
positions[id] = &lpodDailyPositionSum{id: id, vault: p.Vault, vaultAfter: p.After, accrued: p.Accrued, paid: p.Paid, principal: p.Principal}
}
burnBase, burnIntentional, burnAVM, burnTotal = progress.Burns[0], progress.Burns[1], progress.Burns[2], progress.Burns[3]
included, firstIncluded, lastIncluded = progress.Included, progress.First, progress.Last
firstTimestamp, lastTimestamp = progress.FirstTS, progress.LastTS
lastAuditDigest, haveLastAuditDigest, expectedNextRegistry = progress.Digest, progress.HaveDigest, progress.Registry
}
checkpointProgress := func(next uint64) {
progress.Certificate, progress.Anchor, progress.Witness = cert, anchorHash, witnessHash
progress.AnchorHeight, progress.WitnessHeight, progress.Marker = anchorHeight, witnessHeight, markerDigest
progress.Prune = append([]byte(nil), pruneCursorRaw...)
progress.Next, progress.Previous, progress.PriorTS = next, prevHash, priorTS
progress.Vaults = make(map[string][4]uint64, len(vaults))
for id, v := range vaults { progress.Vaults[id] = [4]uint64{v.accrued, v.leader, v.guardian, v.unfunded} }
progress.Positions = make(map[string]dailyPositionProgress, len(positions))
for id, p := range positions { progress.Positions[id] = dailyPositionProgress{p.vault, p.vaultAfter, p.accrued, p.paid, p.principal} }
progress.Burns = [4]uint64{burnBase, burnIntentional, burnAVM, burnTotal}
progress.Included, progress.First, progress.Last = included, firstIncluded, lastIncluded
progress.FirstTS, progress.LastTS = firstTimestamp, lastTimestamp
progress.Digest, progress.HaveDigest, progress.Registry = lastAuditDigest, haveLastAuditDigest, expectedNextRegistry
}
batchStarted := time.Now()
for h := nextHeight; h <= cert.Height; h++ {
// Cooperative budget at verified block boundaries. A single block is not
// preemptible; the outer deadline cancels between verifier operations.
if background && h > nextHeight && (h-nextHeight >= dailyBatchBlocks || time.Since(batchStarted) >= dailyBatchTime) {
if !s.dailyBindingValid(batchBinding) {
fail("canonical certificate or finality changed during batch"); return
}
checkpointProgress(h)
out["coverage"] = map[string]interface{}{"blocks": included, "vaults": len(vaults), "positions": len(positions)}
fail("bounded background verification in progress")
return
}
		if err := ctx.Err(); err != nil {
			fail(dailyAuditContextReason(err))
			return
		}
		block, hash, readErr := readBlock(h)
		if readErr != nil {
			if ctx.Err() != nil {
				fail(dailyAuditContextReason(ctx.Err()))
				return
			}
			fail(readErr.Error())
			return
		}
		if err := ctx.Err(); err != nil {
			fail(dailyAuditContextReason(err))
			return
		}
		if h == anchorHeight && h > 0 && block.Header.PrevHash != prevHash {
			fail("opening anchor parent hash is discontinuous")
			return
		}
		if h > anchorHeight && (block.Header.PrevHash != prevHash || block.Header.Timestamp < priorTS) {
			fail("certified canonical ancestry is discontinuous")
			return
		}
		prevHash, priorTS = hash, block.Header.Timestamp
		ts := time.Unix(0, block.Header.Timestamp)
		if !ts.Before(start) && ts.Before(end) {
			if included == 0 {
				firstIncluded, firstTimestamp = h, block.Header.Timestamp
			}
			lastIncluded, lastTimestamp = h, block.Header.Timestamp
			included++
			if included > lpodDailyMaxBlocks {
				fail("interval exceeds 40000-block bound")
				return
			}
record, auditErr := s.blockStore.LoadLPoDAuditAtBounded(hash, dailyJobMaxBytes)
			if auditErr != nil || record == nil {
				fail(fmt.Sprintf("exact block audit record missing at height %d", h))
				return
			}
if len(record.Vaults)+len(record.Positions) > lpodDailyMaxEntries {
fail("block audit entry count exceeds bound"); return
}
			if h == firstIncluded && marker.FundingHeight != 0 &&
				(record.FundingHeight != marker.FundingHeight ||
					record.FundingGenesis != marker.Genesis || record.FundingRoot != marker.Root) {
				fail("LPoD audit funding identity differs from the trusted coverage marker")
				return
			}
			if !record.Available {
				reason := record.UnavailableReason
				if reason == "" {
					reason = "record marked unavailable"
				}
				fail(fmt.Sprintf("block audit unavailable at height %d: %s", h, reason))
				return
			}
			if haveLastAuditDigest && record.BeforeCheckpointDigest != lastAuditDigest {
				fail(fmt.Sprintf("checkpoint digest chain breaks at height %d", h))
				return
			}
			projectedStake, nextRegistry, registryErr := s.verifyDailyRegistryEvidence(ctx, block, record, expectedNextRegistry)
			if registryErr != nil {
				if ctx.Err() != nil {
					fail(dailyAuditContextReason(ctx.Err()))
				} else {
					fail(fmt.Sprintf("registry snapshot verification failed at height %d: %s", h, registryErr))
				}
				return
			}
			if err := s.verifyDailyLPoDAuditWithCache(block, record, checkpointCache, previousBlock, projectedStake); err != nil {
				fail(fmt.Sprintf("block audit verification failed at height %d: %s", h, err))
				return
			}
			expectedNextRegistry = nextRegistry
			if err := ctx.Err(); err != nil {
				fail(dailyAuditContextReason(err))
				return
			}
			lastAuditDigest, haveLastAuditDigest = record.AfterCheckpointDigest, true
			if len(vaults)+len(positions)+len(record.Vaults)+len(record.Positions) > lpodDailyMaxEntries {
				fail("response entry count exceeds bound")
				return
			}
			for _, v := range record.Vaults {
				if err := ctx.Err(); err != nil {
					fail(dailyAuditContextReason(err))
					return
				}
				a := vaults[v.ID]
				if a == nil {
					a = &lpodDailyVaultSum{id: v.ID}
					vaults[v.ID] = a
				}
				if !sumDaily(&a.accrued, v.Accrued) || !sumDaily(&a.leader, v.ActualLeaderPaid) ||
					!sumDaily(&a.guardian, v.GuardianPaid) || !sumDaily(&a.unfunded, v.Unfunded) {
					fail("vault aggregate overflow")
					return
				}
			}
			for _, p := range record.Positions {
				if err := ctx.Err(); err != nil {
					fail(dailyAuditContextReason(err))
					return
				}
				a := positions[p.ID]
				if a == nil {
					a = &lpodDailyPositionSum{id: p.ID, vault: p.EffectiveVault, vaultAfter: p.EffectiveVaultAfter}
					positions[p.ID] = a
				}
				a.vaultAfter = p.EffectiveVaultAfter
				if !sumDaily(&a.accrued, p.Accrued) || !sumDaily(&a.paid, p.Paid) ||
					!sumDaily(&a.principal, p.PrincipalReturned) {
					fail("position aggregate overflow")
					return
				}
			}
			if !sumDaily(&burnBase, *record.ProtocolBaseFeeBurnNAPRO) ||
				!sumDaily(&burnIntentional, *record.SignedIntentionalBurnNAPRO) ||
				!sumDaily(&burnAVM, *record.AVMGasBurnNAPRO) || !sumDaily(&burnTotal, *record.TotalBurnNAPRO) {
				fail("burn aggregate overflow")
				return
			}
		}
		if h == cert.Height {
			break
		}
		previousBlock = block
	}
	if included == 0 {
		fail("interval has no included canonical blocks")
		return
	}
	if err := ctx.Err(); err != nil {
		fail(dailyAuditContextReason(err))
		return
	}
	var checkedBurnTotal uint64
	if !sumDaily(&checkedBurnTotal, burnBase) || !sumDaily(&checkedBurnTotal, burnIntentional) ||
		!sumDaily(&checkedBurnTotal, burnAVM) || checkedBurnTotal != burnTotal {
		fail("daily burn components do not reconcile")
		return
	}
	if firstIncluded != anchorHeight+1 || lastIncluded+1 != witnessHeight {
		fail("timestamp boundary gap or incomplete interval coverage")
		return
	}
	if markerFound && marker != nil && marker.Height > firstIncluded {
		fail("LPoD audit coverage marker is after the first included block")
		return
	}

	// Recheck both independently observed branch identities after all reads.
	afterTip, afterTipHeight, tipErr := s.blockStore.GetTip()
	afterCert, certFound, certErr := s.blockStore.GetCanonicalHash(cert.Height)
	afterPruneCursor, pruneCursorErr := s.blockStore.GetMeta("prune_cursor")
if tipErr != nil || (!background && (afterTip != tipHash || afterTipHeight != tipHeight)) ||
		certErr != nil || !certFound || afterCert != cert.BlockHash ||
		pruneCursorErr != nil || !bytes.Equal(afterPruneCursor, pruneCursorRaw) {
		fail("canonical tip, certificate, or prune cursor changed during audit")
		return
	}
	if s.lpodFinalized == nil || !s.lpodFinalized(cert.Height, cert.BlockHash) {
		fail("finality verification was lost during audit")
		return
	}
	if err := ctx.Err(); err != nil {
		fail(dailyAuditContextReason(err))
		return
	}
	latestCert, latestCertErr := s.blockStore.LoadFinalityCertificate()
if latestCertErr != nil || latestCert == nil ||
(!background && (latestCert.Height != cert.Height || latestCert.BlockHash != cert.BlockHash)) ||
(background && !s.dailyBindingValid(batchBinding)) {
		fail("durable certificate changed during audit")
		return
	}
finalCheckpoint, checkpointErr := s.blockStore.LoadLPoDCheckpointAtBounded(cert.BlockHash, dailyJobMaxBytes)
	if checkpointErr != nil || finalCheckpoint == nil || finalCheckpoint.Allocation == nil {
		fail("certified LPoD funding identity unavailable")
		return
	}
	if err := finalCheckpoint.State.Validate(); err != nil {
		fail("certified checkpoint supply conservation invalid")
		return
	}
	lastAuditHash, lastAuditFound, lastAuditErr := s.blockStore.GetCanonicalHash(lastIncluded)
lastAuditCheckpoint, lastAuditCPError := s.blockStore.LoadLPoDCheckpointAtBounded(lastAuditHash, dailyJobMaxBytes)
	if lastAuditErr != nil || !lastAuditFound || lastAuditCPError != nil ||
		lastAuditCheckpoint == nil || verifyDailyFundingIdentity(lastAuditCheckpoint, finalCheckpoint) != nil {
		fail("funding identity does not continue from interval through certificate")
		return
	}
	allocation := finalCheckpoint.Allocation
	out["funding"] = map[string]interface{}{
		"version": allocation.Version, "funding_height": allocation.FundingHeight,
		"funding_block_hash":  fmt.Sprintf("%x", allocation.FundingBlock[:]),
		"genesis":             fmt.Sprintf("%x", allocation.Genesis[:]),
		"reconciliation_root": fmt.Sprintf("%x", allocation.ReconciliationRoot[:]),
	}
	vaultList := make([]lpodDailyVault, 0, len(vaults))
	for _, v := range vaults {
		vaultList = append(vaultList, lpodDailyVault{v.id, strconv.FormatUint(v.accrued, 10),
			strconv.FormatUint(v.leader, 10), strconv.FormatUint(v.guardian, 10), strconv.FormatUint(v.unfunded, 10)})
	}
	sort.Slice(vaultList, func(i, j int) bool { return vaultList[i].ID < vaultList[j].ID })
	positionList := make([]lpodDailyPosition, 0, len(positions))
	for _, p := range positions {
		positionList = append(positionList, lpodDailyPosition{p.id, p.vault, p.vaultAfter,
			strconv.FormatUint(p.accrued, 10), strconv.FormatUint(p.paid, 10), strconv.FormatUint(p.principal, 10)})
	}
	sort.Slice(positionList, func(i, j int) bool { return positionList[i].ID < positionList[j].ID })
	out["vaults"], out["positions"] = vaultList, positionList
	out["burns_napro"] = map[string]string{
		"protocol_base_fee": strconv.FormatUint(burnBase, 10), "signed_intentional": strconv.FormatUint(burnIntentional, 10),
		"avm_gas": strconv.FormatUint(burnAVM, 10), "total": strconv.FormatUint(burnTotal, 10),
	}
	out["certificate"] = map[string]interface{}{"height": cert.Height, "block_hash": fmt.Sprintf("%x", cert.BlockHash[:])}
	out["coverage"] = map[string]interface{}{
		"blocks": included, "first_height": firstIncluded, "last_height": lastIncluded,
		"first_timestamp": time.Unix(0, firstTimestamp).Format(time.RFC3339Nano),
		"last_timestamp":  time.Unix(0, lastTimestamp).Format(time.RFC3339Nano),
		"vaults":          len(vaultList), "positions": len(positionList),
	}
	if err := ctx.Err(); err != nil {
		fail(dailyAuditContextReason(err))
		return
	}
	out["status"], out["reason"] = "complete", ""
if background {
if cert.Height == ^uint64(0) { fail("certificate height overflows progress"); return }
checkpointProgress(cert.Height+1)
}
	writeJSON(w, http.StatusOK, out)
}

func dailyAuditContextReason(err error) string {
	if err == context.DeadlineExceeded {
		return "daily audit exceeded its 20-second work budget"
	}
	return "daily audit request was cancelled"
}

func sumDaily(dst *uint64, value uint64) bool {
	if ^uint64(0)-*dst < value {
		return false
	}
	*dst += value
	return true
}

// verifyDailyRegistryEvidence replays the canonical block's stake transactions
// from its operator-attested pre-operation snapshot. This is explicitly not an
// independent proof of registry history: only continuity after the opening
// snapshot is checked.
func (s *Server) verifyDailyRegistryEvidence(
	ctx context.Context,
	block *core.Block,
	record *store.LPoDBlockAudit,
	expectedBefore *core.RegistrySnapshot,
) (map[string]store.LPoDValidatorStake, *core.RegistrySnapshot, error) {
	fail := func(err error) (map[string]store.LPoDValidatorStake, *core.RegistrySnapshot, error) {
		return nil, nil, err
	}
	if record.RegistryBefore == nil || record.RegistryAfter == nil {
		return fail(fmt.Errorf("pre- or post-operation registry snapshot missing"))
	}
	before, after := record.RegistryBefore, record.RegistryAfter
	if before.Validators == nil || after.Validators == nil {
		return fail(fmt.Errorf("registry snapshot has no validator map"))
	}
	if expectedBefore != nil && !reflect.DeepEqual(*expectedBefore, *before) {
		return fail(fmt.Errorf("pre-operation snapshot does not continue the prior epoch transition"))
	}
	validateSnapshot := func(snapshot *core.RegistrySnapshot) error {
		for key, entry := range snapshot.Validators {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry == nil || len(entry.PubKey) != 32 || entry.PubKey.Hex() != key {
				return fmt.Errorf("registry snapshot contains an invalid validator identity")
			}
			switch entry.Status {
			case core.ValidatorPending, core.ValidatorActive, core.ValidatorUnbonding, core.ValidatorExited:
			default:
				return fmt.Errorf("registry snapshot contains an invalid validator status")
			}
		}
		return nil
	}
	if err := validateSnapshot(before); err != nil {
		return fail(err)
	}
	if err := validateSnapshot(after); err != nil {
		return fail(err)
	}

	projectedPrevious := make(map[string]store.LPoDValidatorStake, len(before.Validators))
	expectedStake := make(map[string]store.LPoDValidatorStake, len(before.Validators))
	for key, entry := range before.Validators {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		stake := store.LPoDValidatorStake{Amount: entry.StakeNAPR, Active: entry.Status == core.ValidatorActive}
		projectedPrevious[key] = stake
		expectedStake[key] = stake
	}
	if !sameDailyStakeMap(projectedPrevious, record.PreviousStake) {
		return fail(fmt.Errorf("previous stake map differs from trusted pre-operation snapshot"))
	}
	withdrawalV2 := false
	var withdrawalGenesis crypto.Hash32
	var withdrawalActivation uint64
	for _, tx := range block.Txs {
		if tx.IsStake() && len(tx.Extra) == core.StakeWithdrawalPayloadSizeV2 {
			withdrawalV2 = true
			break
		}
	}
	if withdrawalV2 {
		if s.registry == nil {
			return fail(fmt.Errorf("v2 withdrawal binding unavailable"))
		}
		var configured bool
		withdrawalGenesis, withdrawalActivation, configured = s.registry.StakeWithdrawalV2Config()
		if !configured {
			return fail(fmt.Errorf("v2 withdrawal binding unavailable"))
		}
	}
	for _, tx := range block.Txs {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
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
				return fail(fmt.Errorf("decode stake operation: %w", err))
			}
		case core.StakeWithdrawalPayloadSizeV2:
			authorization, err := core.DecodeStakeWithdrawalExtraV2(tx.Extra)
			if err != nil {
				return fail(fmt.Errorf("decode v2 stake withdrawal: %w", err))
			}
			action, pub, amount = authorization.Action, authorization.PubKey, authorization.Amount
		default:
			continue
		}
		if action != core.StakeWithdraw && action != core.StakePartialWithdraw {
			continue
		}
		key := pub.Hex()
		stake, exists := expectedStake[key]
		if !exists {
			return fail(fmt.Errorf("withdrawal validator %s is absent from pre-operation registry", key))
		}
		switch action {
		case core.StakeWithdraw:
			stake.Active = false
		case core.StakePartialWithdraw:
			if amount > stake.Amount {
				return fail(fmt.Errorf("partial withdrawal exceeds projected stake"))
			}
			stake.Amount -= amount
			if stake.Amount < core.MinStakeNAPR {
				stake.Active = false
			}
		}
		expectedStake[key] = stake
	}
	if !sameDailyStakeMap(expectedStake, record.Stake) {
		return fail(fmt.Errorf("stake map differs from projected block withdrawals"))
	}

	snapshotBytes, err := json.Marshal(*before)
	if err != nil {
		return fail(fmt.Errorf("encode pre-operation registry snapshot: %w", err))
	}
	var replayStart core.RegistrySnapshot
	if err := json.Unmarshal(snapshotBytes, &replayStart); err != nil {
		return fail(fmt.Errorf("copy pre-operation registry snapshot: %w", err))
	}
	replay := core.NewValidatorRegistry()
	replay.RestoreFromSnapshot(replayStart)
	if withdrawalV2 {
		if err := replay.ConfigureStakeWithdrawalV2(withdrawalGenesis, withdrawalActivation); err != nil {
			return fail(fmt.Errorf("configure v2 withdrawal replay: %w", err))
		}
	}
	if err := replay.ReplayBlockStakeTxs(block.Txs, block.Header.Height); err != nil {
		return fail(fmt.Errorf("replay canonical stake transactions: %w", err))
	}
	postApply := replay.TakeSnapshot()
	if !reflect.DeepEqual(postApply, *after) {
		return fail(fmt.Errorf("replayed post-operation registry differs from stored snapshot"))
	}

	effectiveStake := make(map[string]store.LPoDValidatorStake, len(expectedStake))
	for key, stake := range expectedStake {
		if prior := projectedPrevious[key]; !stake.Active && prior.Active {
			stake = prior
		}
		effectiveStake[key] = stake
	}
	nextBytes, err := json.Marshal(postApply)
	if err != nil {
		return fail(fmt.Errorf("encode post-operation registry snapshot: %w", err))
	}
	var nextStart core.RegistrySnapshot
	if err := json.Unmarshal(nextBytes, &nextStart); err != nil {
		return fail(fmt.Errorf("copy post-operation registry snapshot: %w", err))
	}
	next := core.NewValidatorRegistry()
	next.RestoreFromSnapshot(nextStart)
	if block.Header.Height%core.EpochLength == 0 {
		next.UpdateEpoch(block.Header.Height)
	}
	nextSnapshot := next.TakeSnapshot()
	return effectiveStake, &nextSnapshot, nil
}

func sameDailyStakeMap(a, b map[string]store.LPoDValidatorStake) bool {
	if len(a) != len(b) {
		return false
	}
	for id, stake := range a {
		if b[id] != stake {
			return false
		}
	}
	return true
}

func (s *Server) verifyDailyLPoDAudit(block *core.Block, r *store.LPoDBlockAudit) error {
	return s.verifyDailyLPoDAuditWithCache(block, r, nil, nil, nil)
}

// verifyDailyLPoDAuditWithCache reuses immutable adjacent checkpoints and the
// already-authenticated contiguous predecessor supplied by the daily scan.
// Standalone callers use verifyDailyLPoDAudit, which performs all reads itself.
func (s *Server) verifyDailyLPoDAuditWithCache(
	block *core.Block,
	r *store.LPoDBlockAudit,
	checkpoints map[crypto.Hash32]*store.LPoDCheckpoint,
	parentBlock *core.Block,
	projectedStake map[string]store.LPoDValidatorStake,
) error {
	if r.Height != block.Header.Height || r.BlockHash != block.Hash() ||
		r.ParentHash != block.Header.PrevHash || r.HeaderTimestamp != block.Header.Timestamp {
		return fmt.Errorf("record identity differs from canonical block")
	}
	if len(block.Txs) < 2 || block.Txs[0].Hash() != r.PayoutTransactionHash {
		return fmt.Errorf("payout transaction identity mismatch")
	}
	payoutAuth, err := block.Txs[0].LPoDPayoutAuthorization()
	if err != nil {
		return fmt.Errorf("payout authorization invalid")
	}
	digest, err := block.Txs[1].LPoDCheckpointDigest()
	if err != nil {
		return fmt.Errorf("checkpoint commitment transaction invalid")
	}
	if digest != r.AfterCheckpointDigest {
		return fmt.Errorf("checkpoint transaction digest mismatch")
	}
	if len(r.Outputs) != len(block.Txs[0].Outputs) {
		return fmt.Errorf("payout output references do not cover the canonical transaction")
	}
	loadCheckpoint := func(hash crypto.Hash32) (*store.LPoDCheckpoint, error) {
		if checkpoints != nil {
			if checkpoint := checkpoints[hash]; checkpoint != nil {
				return checkpoint, nil
			}
		}
checkpoint, err := s.blockStore.LoadLPoDCheckpointAtBounded(hash, dailyJobMaxBytes)
		if err != nil || checkpoint == nil || checkpoints == nil {
			return checkpoint, err
		}
		checkpoints[hash] = checkpoint
		if len(checkpoints) > 2 {
			var oldestHash crypto.Hash32
			oldestHeight := ^uint64(0)
			for cachedHash, cached := range checkpoints {
				if cachedHash != hash && cached.State.LastHeight < oldestHeight {
					oldestHash, oldestHeight = cachedHash, cached.State.LastHeight
				}
			}
			delete(checkpoints, oldestHash)
		}
		return checkpoint, nil
	}
	after, err := loadCheckpoint(block.Hash())
	if err != nil || after == nil || after.Digest() != r.AfterCheckpointDigest || after.State.LastHeight != r.Height {
		return fmt.Errorf("after checkpoint unavailable or digest mismatch")
	}
	if payoutAuth.Digest != after.Digest() {
		return fmt.Errorf("payout authorization checkpoint digest mismatch")
	}
	before, err := loadCheckpoint(block.Header.PrevHash)
	if err != nil || before == nil || before.Digest() != r.BeforeCheckpointDigest {
		return fmt.Errorf("before checkpoint unavailable or digest mismatch")
	}
	if err := before.State.Validate(); err != nil {
		return fmt.Errorf("before checkpoint supply conservation invalid: %w", err)
	}
	if err := after.State.Validate(); err != nil {
		return fmt.Errorf("after checkpoint supply conservation invalid: %w", err)
	}
	if err := verifyDailyFundingIdentity(before, after); err != nil {
		return err
	}
	amounts, positionAccrued, positionPaid, principal, hasPositions, err :=
		s.verifyDailyPositionRowsWithParent(block, r, before, after, parentBlock, projectedStake)
	if err != nil {
		return err
	}
	var accrued, leaderPaid, guardianPaid uint64
	for _, v := range r.Vaults {
		if !sumDaily(&accrued, v.Accrued) || !sumDaily(&leaderPaid, v.ActualLeaderPaid) ||
			!sumDaily(&guardianPaid, v.GuardianPaid) {
			return fmt.Errorf("vault totals overflow")
		}
	}
	if after.State.AccruedLiability < before.State.AccruedLiability ||
		after.State.LeaderPaid < before.State.LeaderPaid || after.State.AngelPaid < before.State.AngelPaid ||
		after.PrincipalReturned < before.PrincipalReturned ||
		accrued != after.State.AccruedLiability-before.State.AccruedLiability ||
		leaderPaid != after.State.LeaderPaid-before.State.LeaderPaid ||
		guardianPaid != after.State.AngelPaid-before.State.AngelPaid ||
		positionAccrued != after.State.AccruedLiability-before.State.AccruedLiability ||
		positionPaid != after.State.AngelPaid-before.State.AngelPaid ||
		principal != after.PrincipalReturned-before.PrincipalReturned {
		return fmt.Errorf("record arithmetic does not reconcile checkpoint deltas")
	}
	if leaderPaid > 0 {
		if !addDailyAmount(amounts, payoutAuth.Leader, leaderPaid) {
			return fmt.Errorf("leader payout aggregate overflow")
		}
	}
	if err := verifyDailyPayoutOutputs(block, r, after.Digest(), amounts); err != nil {
		return err
	}
	// Registry snapshots are explicitly operator-attested, not an independent
	// chain proof. The daily endpoint supplies a replay-verified stake projection
	// rooted in its trusted opening snapshot; standalone verification has none.
	if hasPositions && projectedStake == nil {
		return fmt.Errorf("position APR tier basis has no verified operator-attested registry snapshot")
	}
	return verifyDailyBurnRecord(block, r, s.avmGasBurnActivationHeight)
}

func verifyDailyPayoutOutputs(
	block *core.Block,
	r *store.LPoDBlockAudit,
	digest crypto.Hash32,
	amounts map[crypto.Address]uint64,
) error {
	addresses := make([]string, 0, len(amounts))
	for address, amount := range amounts {
		if amount > 0 {
			addresses = append(addresses, string(address))
		}
	}
	sort.Strings(addresses)
	if len(addresses) != len(block.Txs[0].Outputs) || len(addresses) != len(r.Outputs) {
		return fmt.Errorf("payout recipients do not match canonical output count")
	}
	expectedOutputs := make([]core.Output, 0, len(addresses))
	for i, text := range addresses {
		address := crypto.Address(text)
		amount := amounts[address]
		expected, buildErr := core.BuildLPoDPayoutOutput(address, amount, block.Header.Height,
			block.Header.PrevHash, digest)
		if buildErr != nil {
			return fmt.Errorf("cannot reconstruct payout output: %w", buildErr)
		}
		expectedOutputs = append(expectedOutputs, expected)
		ref := r.Outputs[i]
		if ref.TransactionHash != r.PayoutTransactionHash || int(ref.OutputIndex) != i ||
			ref.Beneficiary != text || ref.Amount != amount {
			return fmt.Errorf("payout output reference beneficiary or amount mismatch")
		}
	}
	outputsMatch := len(expectedOutputs) == len(block.Txs[0].Outputs)
	for i := 0; outputsMatch && i < len(expectedOutputs); i++ {
		outputsMatch = expectedOutputs[i] == block.Txs[0].Outputs[i]
	}
	if !outputsMatch {
		return fmt.Errorf("reconstructed payout commitments or keys differ from canonical transaction")
	}
	return nil
}

func verifyDailyFundingIdentity(before, after *store.LPoDCheckpoint) error {
	if after.Allocation == nil {
		return fmt.Errorf("checkpoint funding identity unavailable across transition")
	}
	if before.Allocation == nil {
		// Funding establishes an authenticated allocation against the
		// synthetic pre-allocation checkpoint on the parent block.
		if after.Allocation.FundingHeight == after.State.LastHeight &&
			before.State.FundingDebit == after.State.FundingDebit {
			return nil
		}
		return fmt.Errorf("checkpoint funding identity unavailable across transition")
	}
	a, b := before.Allocation, after.Allocation
	if a.Version != b.Version || a.PositionLifecycleVersion != b.PositionLifecycleVersion ||
		a.FundingHeight != b.FundingHeight || a.FundingBlock != b.FundingBlock ||
		a.Genesis != b.Genesis || a.ReconciliationRoot != b.ReconciliationRoot {
		return fmt.Errorf("checkpoint funding identity changed across transition")
	}
	return nil
}

func addDailyAmount(amounts map[crypto.Address]uint64, address crypto.Address, value uint64) bool {
	current := amounts[address]
	if ^uint64(0)-current < value {
		return false
	}
	amounts[address] = current + value
	return true
}

func (s *Server) verifyDailyPositionRows(
	block *core.Block,
	r *store.LPoDBlockAudit,
	before, after *store.LPoDCheckpoint,
) (map[crypto.Address]uint64, uint64, uint64, uint64, bool, error) {
	return s.verifyDailyPositionRowsWithParent(block, r, before, after, nil, nil)
}

func (s *Server) verifyDailyPositionRowsWithParent(
	block *core.Block,
	r *store.LPoDBlockAudit,
	before, after *store.LPoDCheckpoint,
	verifiedParent *core.Block,
	projectedStake map[string]store.LPoDValidatorStake,
) (map[crypto.Address]uint64, uint64, uint64, uint64, bool, error) {
	fail := func(err error) (map[crypto.Address]uint64, uint64, uint64, uint64, bool, error) {
		return nil, 0, 0, 0, false, err
	}
	vaultRows := make(map[string]store.LPoDAuditVault, len(r.Vaults))
	for _, v := range r.Vaults {
		if v.ID == "" {
			return fail(fmt.Errorf("empty vault ID in audit row"))
		}
		if _, exists := vaultRows[v.ID]; exists {
			return fail(fmt.Errorf("duplicate vault row %q", v.ID))
		}
		tier, err := lpod.TierFor(v.TierStake)
		if err != nil || tier.APRPercent != v.APRPercent || tier.LeaderPercent != v.LeaderPercent ||
			v.TotalStake < v.ValidatorStake || v.TotalStake-v.ValidatorStake != v.GuardianStake {
			return fail(fmt.Errorf("vault tier or stake arithmetic mismatch for %q", v.ID))
		}
		tierStake := v.TotalStake
		const maxTierStake = 100_000_000 * lpod.Unit
		if tierStake > maxTierStake {
			tierStake = maxTierStake
		}
		if v.TierStake != tierStake {
			return fail(fmt.Errorf("vault tier stake mismatch for %q", v.ID))
		}
		if projectedStake != nil {
			validator, exists := projectedStake[v.ID]
			if !exists || validator.Amount != v.ValidatorStake {
				return fail(fmt.Errorf("vault validator stake differs from verified registry projection for %q", v.ID))
			}
		}
		vaultRows[v.ID] = v
	}
	if block.Header.Height == 0 {
		return fail(fmt.Errorf("position audit cannot verify a genesis parent"))
	}
	parentHash, found, err := s.blockStore.GetCanonicalHash(block.Header.Height - 1)
	if err != nil || !found || parentHash != block.Header.PrevHash {
		return fail(fmt.Errorf("position accrual parent is not canonical"))
	}
	parent := verifiedParent
	if parent == nil {
		parentRaw, readErr := s.blockStore.GetRawBlock(parentHash)
		var decoded core.Block
		if readErr != nil || len(parentRaw) == 0 || json.Unmarshal(parentRaw, &decoded) != nil ||
			decoded.Hash() != parentHash || !decoded.Header.VerifySignature() ||
			decoded.Header.MerkleRoot != core.MerkleRoot(decoded.Txs) {
			return fail(fmt.Errorf("position accrual parent body or timestamp unavailable"))
		}
		parent = &decoded
	}
	if parent.Header.Height+1 != block.Header.Height || parent.Hash() != parentHash ||
		block.Header.Timestamp <= parent.Header.Timestamp {
		return fail(fmt.Errorf("position accrual parent body or timestamp unavailable"))
	}
	elapsed := uint64(block.Header.Timestamp - parent.Header.Timestamp)
	const maxElapsedNS = uint64(15_000_000_000)
	if elapsed > maxElapsedNS {
		elapsed = maxElapsedNS
	}
	const aprDenominator = uint64(100) * lpod.YearSeconds * 1_000_000_000

	actions := make(map[string]core.LPoDPositionAction)
	for i := 2; i < len(block.Txs); i++ {
		tx := &block.Txs[i]
		if !tx.IsLPoDPosition() {
			continue
		}
		action, err := tx.LPoDPositionAction()
		if err != nil {
			return fail(fmt.Errorf("invalid position operation: %w", err))
		}
		id := fmt.Sprintf("%x", action.PositionID[:])
		if _, exists := actions[id]; exists {
			return fail(fmt.Errorf("multiple position operations for one ID cannot be independently reconciled"))
		}
		actions[id] = *action
	}
	expectedIDs := make(map[string]struct{})
	for id, p := range before.Positions {
		_, inAfter := after.Positions[id]
		if p.Returned && p.Due == 0 && !inAfter {
			continue
		}
		expectedIDs[id] = struct{}{}
	}
	for id := range after.Positions {
		expectedIDs[id] = struct{}{}
	}
	rows := make(map[string]store.LPoDAuditPosition, len(r.Positions))
	for _, row := range r.Positions {
		if row.ID == "" {
			return fail(fmt.Errorf("empty position ID in audit row"))
		}
		if _, exists := rows[row.ID]; exists {
			return fail(fmt.Errorf("duplicate position row %q", row.ID))
		}
		rows[row.ID] = row
	}
	if len(rows) != len(expectedIDs) {
		return fail(fmt.Errorf("position row count differs from before/after checkpoints"))
	}
	amounts := make(map[crypto.Address]uint64)
	var totalAccrued, totalPaid, totalPrincipal uint64
	for id := range expectedIDs {
		row, exists := rows[id]
		if !exists {
			return fail(fmt.Errorf("checkpoint position %q is missing from audit rows", id))
		}
		prior, hadPrior := before.Positions[id]
		post, hasPost := after.Positions[id]
		var deposit core.LPoDPositionAction
		var dueBefore, carryBefore, carryAfter, accrued uint64
		var vaultBefore, vaultAfter string
		if hadPrior {
			deposit = prior.Deposit
			dueBefore, carryBefore = prior.Due, prior.APRCarry
			vaultBefore = dailyPositionVaultID(prior)
			carryAfter = prior.APRCarry
			if !prior.Returned && prior.UnlockHeight == 0 {
				vault, ok := vaultRows[vaultBefore]
				if !ok {
					return fail(fmt.Errorf("position %q has no authenticated vault accounting row", id))
				}
				if vault.ValidatorStake > 0 {
					n := new(big.Int).SetUint64(prior.Deposit.Amount - prior.Withdrawn)
					n.Mul(n, new(big.Int).SetUint64(vault.APRPercent))
					n.Mul(n, new(big.Int).SetUint64(elapsed))
					n.Add(n, new(big.Int).SetUint64(carryBefore))
					q, rem := new(big.Int), new(big.Int)
					q.QuoRem(n, new(big.Int).SetUint64(aprDenominator), rem)
					if !q.IsUint64() || !rem.IsUint64() {
						return fail(fmt.Errorf("position %q APR accrual overflow", id))
					}
					accrued, carryAfter = q.Uint64(), rem.Uint64()
				}
			}
		} else {
			if !hasPost {
				return fail(fmt.Errorf("position %q absent from both checkpoints", id))
			}
			deposit = post.Deposit
			carryAfter = 0
			vaultBefore = dailyPositionVaultID(post)
			action, ok := actions[id]
			if !ok || action.Action != core.LPoDDeposit ||
				!reflect.DeepEqual(action, post.Deposit) || post.Due != 0 ||
				post.Withdrawn != 0 || post.APRCarry != 0 {
				return fail(fmt.Errorf("new position %q lacks matching canonical deposit", id))
			}
		}
		if hasPost {
			if hadPrior && !reflect.DeepEqual(prior.Deposit, post.Deposit) {
				return fail(fmt.Errorf("position %q changed its signed deposit", id))
			}
			vaultAfter = dailyPositionVaultID(post)
			if post.APRCarry != carryAfter {
				return fail(fmt.Errorf("position %q APR carry differs from checkpoint", id))
			}
		} else {
			vaultAfter = vaultBefore
		}
		if row.SignedVault != hex.EncodeToString(deposit.Vault[:]) ||
			row.EffectiveVault != vaultBefore || row.EffectiveVaultAfter != vaultAfter ||
			row.DueBefore != dueBefore || row.Accrued != accrued ||
			row.APRCarryBefore != carryBefore || row.APRCarryAfter != carryAfter {
			return fail(fmt.Errorf("position %q identity, accrual, or carry differs from checkpoints", id))
		}
		dueAfter := uint64(0)
		principalReturned := uint64(0)
		if hasPost {
			dueAfter = post.Due
			if post.Withdrawn < func() uint64 {
				if hadPrior {
					return prior.Withdrawn
				}
				return 0
			}() {
				return fail(fmt.Errorf("position %q principal regressed", id))
			}
			principalReturned = post.Withdrawn
			if hadPrior {
				principalReturned -= prior.Withdrawn
			}
		} else if hadPrior {
			action, ok := actions[id]
			if !ok || action.Action != core.LPoDWithdraw {
				return fail(fmt.Errorf("removed position %q lacks independently verifiable withdrawal", id))
			}
			principalReturned, err = store.LPoDWithdrawalAmount(prior, action)
			if err != nil {
				return fail(fmt.Errorf("position %q withdrawal amount invalid: %w", id, err))
			}
		}
		dueBeforeAccrued := dueBefore
		if !sumDaily(&dueBeforeAccrued, accrued) || dueBeforeAccrued < row.Paid ||
			dueBeforeAccrued-row.Paid != dueAfter || row.DueAfter != dueAfter ||
			row.PrincipalReturned != principalReturned {
			return fail(fmt.Errorf("position %q due or principal arithmetic mismatch", id))
		}
		if string(deposit.Beneficiary) == "" || row.Beneficiary != string(deposit.Beneficiary) {
			return fail(fmt.Errorf("position %q beneficiary differs from checkpoint", id))
		}
		if !sumDaily(&totalAccrued, accrued) || !sumDaily(&totalPaid, row.Paid) ||
			!sumDaily(&totalPrincipal, principalReturned) {
			return fail(fmt.Errorf("position totals overflow"))
		}
		_, ok := addDailyAmounts(amounts, crypto.Address(row.Beneficiary), row.Paid, principalReturned)
		if !ok {
			return fail(fmt.Errorf("position payout amount overflow"))
		}
	}
	return amounts, totalAccrued, totalPaid, totalPrincipal, len(rows) > 0, nil
}

func addDailyAmounts(amounts map[crypto.Address]uint64, address crypto.Address, values ...uint64) (uint64, bool) {
	sum := amounts[address]
	for _, value := range values {
		if ^uint64(0)-sum < value {
			return 0, false
		}
		sum += value
	}
	amounts[address] = sum
	return sum, true
}

func dailyPositionVaultID(p store.LPoDPosition) string {
	vault := p.Deposit.Vault
	if p.EffectiveVault != nil {
		vault = *p.EffectiveVault
	}
	return hex.EncodeToString(vault[:])
}

func dailyBurns(block *core.Block, avmActivation uint64) (uint64, uint64, uint64, uint64, error) {
	baseFee := block.Header.BaseFee
	if baseFee == 0 {
		for i := range block.Txs {
			tx := &block.Txs[i]
			if !tx.IsCoinbase() && !tx.IsStake() && tx.Fee > 0 {
				return 0, 0, 0, 0, fmt.Errorf("zero header base fee cannot authenticate transaction burns")
			}
		}
	}
	var base, intentional, avm, total uint64
	add := func(sum *uint64, v uint64) bool { return sumDaily(sum, v) }
	for i := range block.Txs {
		tx := &block.Txs[i]
		marker := bytes.HasPrefix(tx.Extra, []byte("APRO-BURN\x01"))
		burn, isBurn := tx.BurnAmount()
		if marker && !isBurn {
			return 0, 0, 0, 0, fmt.Errorf("invalid intentional burn marker")
		}
		if tx.IsCoinbase() || tx.IsStake() {
			if marker {
				return 0, 0, 0, 0, fmt.Errorf("intentional burn marker on excluded transaction")
			}
			continue
		}
		if tx.Size() < 0 {
			return 0, 0, 0, 0, fmt.Errorf("negative transaction size")
		}
		minimum := new(big.Int).Mul(new(big.Int).SetUint64(baseFee), big.NewInt(int64(tx.Size())))
		fee := new(big.Int).SetUint64(tx.Fee)
		protocol := tx.Fee
		if minimum.Cmp(fee) <= 0 {
			protocol = minimum.Uint64()
			if isBurn && !add(&intentional, burn) {
				return 0, 0, 0, 0, fmt.Errorf("intentional burn overflow")
			}
		}
		if !add(&base, protocol) {
			return 0, 0, 0, 0, fmt.Errorf("base fee burn overflow")
		}
		if avmActivation > 0 && block.Header.Height >= avmActivation && tx.IsAVM() {
			if tx.AVM == nil {
				return 0, 0, 0, 0, fmt.Errorf("missing AVM transaction payload")
			}
			gas, gasErr := core.AVMGasFee(tx.AVM.GasLimit)
			if gasErr != nil || !add(&avm, gas) {
				return 0, 0, 0, 0, fmt.Errorf("invalid AVM gas burn")
			}
		}
	}
	if !add(&total, base) || !add(&total, intentional) || !add(&total, avm) {
		return 0, 0, 0, 0, fmt.Errorf("total burn overflow")
	}
	return base, intentional, avm, total, nil
}

func verifyDailyBurnRecord(block *core.Block, record *store.LPoDBlockAudit, avmActivation uint64) error {
	base, intentional, avm, total, err := dailyBurns(block, avmActivation)
	if err != nil {
		return err
	}
	if record.ProtocolBaseFeeBurnNAPRO == nil || record.SignedIntentionalBurnNAPRO == nil ||
		record.AVMGasBurnNAPRO == nil || record.TotalBurnNAPRO == nil ||
		*record.ProtocolBaseFeeBurnNAPRO != base || *record.SignedIntentionalBurnNAPRO != intentional ||
		*record.AVMGasBurnNAPRO != avm || *record.TotalBurnNAPRO != total {
		return fmt.Errorf("burn components do not match canonical transaction body")
	}
	return nil
}
