package consensus

import (
	"fmt"
	"math"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func (e *Engine) verifyFinalityCommitteeMigration(certificate *store.FinalityCertificate) ([]crypto.ValidatorPubKey, error) {
	migration := e.cfg.FinalityMigration
	if migration == nil || certificate == nil || e.cfg.Store == nil {
		return nil, fmt.Errorf("finality migration: missing migration, certificate, or store")
	}
	genesis := e.chain.Genesis()
	tip := e.chain.Tip()
	if genesis == nil || tip == nil || migration.Genesis != genesis.Hash() {
		return nil, fmt.Errorf("finality migration: network genesis mismatch")
	}
	if migration.CertificateHeight != certificate.Height ||
		migration.CertificateBlockHash != certificate.BlockHash {
		return nil, fmt.Errorf("finality migration: certificate binding mismatch")
	}
	if migration.ApprovalHeight > tip.Header.Height {
		return nil, fmt.Errorf("finality migration: approval anchor is above the canonical tip")
	}
	storedTipHash, storedTipHeight, err := e.cfg.Store.GetTip()
	if err != nil || storedTipHeight != tip.Header.Height || storedTipHash != tip.Hash() {
		return nil, fmt.Errorf("finality migration: durable tip does not match the canonical chain")
	}
	anchorHash, found, err := e.cfg.Store.GetCanonicalHash(migration.ApprovalHeight)
	if err != nil || !found || anchorHash != migration.ApprovalBlockHash {
		return nil, fmt.Errorf("finality migration: approval anchor is not canonical")
	}
	thresholdPPM := math.Round(e.cfg.BFTThreshold * 1_000_000)
	if math.IsNaN(e.cfg.BFTThreshold) || math.IsInf(e.cfg.BFTThreshold, 0) ||
		thresholdPPM < 1 || thresholdPPM >= 1_000_000 ||
		uint32(thresholdPPM) != migration.BFTThresholdPPM {
		return nil, fmt.Errorf("finality migration: quorum rule does not match node configuration")
	}

	authorities := e.cfg.FinalityMigrationTrustAuthorities
	if len(authorities) == 0 || len(authorities) > 256 {
		return nil, fmt.Errorf("finality migration: explicit trust authorities are required")
	}
	trusted := make(map[string]bool, len(authorities))
	for _, authority := range authorities {
		if len(authority) != 32 || isZeroFinalityValidator(authority) {
			return nil, fmt.Errorf("finality migration: malformed trust authority")
		}
		id := authority.Hex()
		if trusted[id] {
			return nil, fmt.Errorf("finality migration: duplicate trust authority")
		}
		trusted[id] = true
	}
	message, err := migration.SigningMessage()
	if err != nil {
		return nil, err
	}
	verified := make(map[string]bool, len(migration.Approvals))
	for _, approval := range migration.Approvals {
		id := approval.Validator.Hex()
		if !trusted[id] || verified[id] ||
			!e.authority(approval.Validator, migration.ApprovalHeight).Verify(message, approval.Signature) {
			return nil, fmt.Errorf("finality migration: invalid, duplicate, or untrusted approval")
		}
		verified[id] = true
	}
	neededApprovals := int(float64(len(authorities))*e.cfg.BFTThreshold) + 1
	if len(verified) < neededApprovals {
		return nil, fmt.Errorf("finality migration: %d approvals, need %d", len(verified), neededApprovals)
	}
	return append([]crypto.ValidatorPubKey(nil), migration.HistoricalCommittee...), nil
}

func isZeroFinalityValidator(key crypto.ValidatorPubKey) bool {
	if len(key) != 32 {
		return false
	}
	for _, b := range key {
		if b != 0 {
			return false
		}
	}
	return true
}
