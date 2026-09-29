package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func prepareAndSign(opts options) (err error) {
	if err := validateOptions(opts); err != nil {
		return err
	}

	if err := validateOfflinePath(opts.dbPath, "offline DB"); err != nil {
		return err
	}
	if err := validateOfflinePath(opts.registryPath, "validator set"); err != nil {
		return err
	}
	dbPath, err := filepath.Abs(opts.dbPath)
	if err != nil {
		return fmt.Errorf("resolve DB path: %w", err)
	}
	dbPath, err = filepath.EvalSymlinks(dbPath)
	if err != nil {
		return fmt.Errorf("resolve offline DB directory: %w", err)
	}
	outputPath, err := filepath.Abs(opts.outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	outputParentPath, err := filepath.EvalSymlinks(filepath.Dir(outputPath))
	if err != nil {
		return fmt.Errorf("resolve output parent directory: %w", err)
	}
	outputPath = filepath.Join(outputParentPath, filepath.Base(outputPath))
	if rel, err := filepath.Rel(dbPath, outputPath); err == nil && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("output path must not be inside the offline DB directory")
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return fmt.Errorf("output path already exists; refusing to replace it")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect output path: %w", err)
	}
	outputParent, err := os.Stat(filepath.Dir(outputPath))
	if err != nil || !outputParent.IsDir() {
		return fmt.Errorf("output parent must be an existing directory")
	}

	registry, err := readAnchoredRegistry(opts.registryPath)
	if err != nil {
		return err
	}
	if registry.Height != opts.registryHeight || registry.Hash != opts.registryHash {
		return fmt.Errorf("validator-set file anchor differs from explicit height/hash")
	}
	if err := checkOperatorRegistrySnapshot(registry.Registry, opts.expectedValidator); err != nil {
		return err
	}

	db, err := store.OpenReadOnly(opts.dbPath)
	if err != nil {
		return fmt.Errorf("open offline DB read-only: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close offline DB: %w", closeErr)
		}
	}()

	parentHeight := opts.height - 1
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return fmt.Errorf("read offline DB tip: %w", err)
	}
	if tipHeight != parentHeight || tipHash != opts.parentHash {
		return fmt.Errorf("offline DB tip is not the exact expected activation parent")
	}
	if err := verifyCanonicalParent(db, parentHeight, opts.parentHash, opts.expectedValidator); err != nil {
		return err
	}
	if err := verifyExpectedGenesis(db, opts.genesis); err != nil {
		return err
	}
	if registry.Height > parentHeight {
		return fmt.Errorf("validator-set snapshot is above the activation parent")
	}
	if parentHeight-registry.Height > maxRegistryReplayBlocks {
		return fmt.Errorf("validator-set snapshot is too old; refresh it within %d canonical blocks", maxRegistryReplayBlocks)
	}
	if err := verifyRegistryAnchorAndReplay(db, registry, parentHeight, opts.parentHash, opts.expectedValidator); err != nil {
		return err
	}

	cert, err := db.LoadFinalityCertificate()
	if err != nil {
		return fmt.Errorf("read durable finality certificate: %w", err)
	}
	if err := verifyOneValidatorCertificate(cert, parentHeight, opts.parentHash, opts.expectedValidator); err != nil {
		return err
	}

	pool, found, err := db.LoadStakingPoolRemaining()
	if err != nil {
		return fmt.Errorf("read remaining validator budget: %w", err)
	}
	if !found || pool != opts.validatorRemainingNAPRO {
		return fmt.Errorf("validator remaining budget does not match the explicit value in the offline DB")
	}

	circulatingNAPRO, err := aproToNAPRO(opts.nominalCirculatingAPRO)
	if err != nil {
		return err
	}
	eligibleNAPRO, err := aproToNAPRO(opts.eligibleNominalAPRO)
	if err != nil {
		return err
	}
	if err := validateNominalAmounts(circulatingNAPRO, eligibleNAPRO); err != nil {
		return err
	}
	if err := validateEligibleAgainstValidatorPool(eligibleNAPRO, pool); err != nil {
		return err
	}

	snapshot := &store.LPoDNominalSnapshot{
		Basis:                          store.LPoDV3NominalBasis,
		NominalCirculatingBefore:       circulatingNAPRO,
		EligibleNominal:                eligibleNAPRO,
		PreexistingGuardianReservation: 0,
		ValidatorRemaining:             pool,
		EligibilityRule:                store.LPoDV3EligibilityRule,
	}
	migration := &store.LPoDMigration{
		Version:                  3,
		PositionLifecycleVersion: 1,
		Height:                   opts.height,
		Genesis:                  opts.genesis,
		ValidatorRemaining:       pool,
		TrustedValidators:        []crypto.ValidatorPubKey{opts.expectedValidator},
		TrustAssumption:          store.LPoDV3TrustAssumption,
		ParentHash:               opts.parentHash,
		SnapshotRoot:             snapshot.Root(),
		NominalEligible:          eligibleNAPRO,
		NominalSnapshot:          snapshot,
	}
	migration.ReconciliationRoot = migration.Root()

	// Do not read or unlock the signing key until all offline chain, operator-
	// trusted registry, single-vote certificate, budget, and amount checks above
	// have succeeded. The registry is not consensus quorum proof.
	keyBytes, err := read0600RegularFile(opts.keyPath)
	if err != nil {
		return fmt.Errorf("read validator key: %w", err)
	}
	key, err := crypto.NewLockedValidatorKey(keyBytes, func(lockErr error) {
		fmt.Fprintf(os.Stderr, "lpod-v3-witness: warning: validator key could not be memory-locked: %v\n", lockErr)
	})
	crypto.ZeroBytes(keyBytes)
	if err != nil {
		return fmt.Errorf("load validator key: %w", err)
	}
	defer key.Destroy()
	if !bytes.Equal(key.Public(), opts.expectedValidator) {
		return fmt.Errorf("validator key does not match expected validator public key")
	}
	signature, err := key.Sign(migration.AttestationMessage())
	if err != nil {
		return fmt.Errorf("sign nominal-reclassification attestation: %w", err)
	}
	migration.Attestations = []store.LPoDAttestation{{
		Validator: opts.expectedValidator,
		Signature: signature,
	}}
	if err := migration.VerifyAuthorization(); err != nil {
		return fmt.Errorf("verify prepared witness authorization: %w", err)
	}
	if _, verifiedParent, err := db.VerifyLPoDMigration(migration); err != nil {
		return fmt.Errorf("verify prepared witness against offline chain: %w", err)
	} else if verifiedParent != opts.parentHash {
		return fmt.Errorf("migration verifier returned an unexpected parent")
	}

	return writeExclusive0600(opts.outputPath, migration)
}
