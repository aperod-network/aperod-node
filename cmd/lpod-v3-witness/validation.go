package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

const (
	approvedNominalCirculatingAPRO uint64 = 6_230_000_000
	approvedEligibleNominalAPRO    uint64 = 1_000_000_000
)

func validateOptions(opts options) error {
	if opts.dbPath == "" || opts.outputPath == "" || opts.keyPath == "" || opts.registryPath == "" {
		return fmt.Errorf("db, output, validator-key, and validator-set-json are all required")
	}
	if !opts.acceptOperatorRegistryAssumption {
		return fmt.Errorf("refusing to sign: pass --accept-operator-registry-assumption to acknowledge that the registry is operator-trusted, its block anchor does not authenticate its contents, and one vote does not prove a quorum")
	}
	if !opts.confirmFrozenCopy {
		return fmt.Errorf("refusing to sign: pass --confirm-frozen-copy only after confirming --db is an offline frozen copy")
	}
	if opts.height < 2 || opts.parentHash == (crypto.Hash32{}) || opts.genesis == (crypto.Hash32{}) ||
		opts.registryHash == (crypto.Hash32{}) {
		return fmt.Errorf("activation height must be at least 2 and all hashes must be explicit and nonzero")
	}
	if len(opts.expectedValidator) != 32 {
		return fmt.Errorf("expected validator public key must be exactly 32 bytes")
	}
	if isZeroKey(opts.expectedValidator) {
		return fmt.Errorf("expected validator public key must not be zero")
	}
	if opts.nominalCirculatingAPRO == 0 || opts.eligibleNominalAPRO == 0 {
		return fmt.Errorf("nominal circulation and eligible nominal allocation must be nonzero")
	}
	if err := validateApprovedNominalAmounts(opts.nominalCirculatingAPRO, opts.eligibleNominalAPRO); err != nil {
		return err
	}
	if filepath.Clean(opts.outputPath) == filepath.Clean(opts.keyPath) {
		return fmt.Errorf("output path must not overwrite validator key")
	}
	return nil
}

func validateApprovedNominalAmounts(circulatingAPRO, eligibleAPRO uint64) error {
	if circulatingAPRO != approvedNominalCirculatingAPRO ||
		eligibleAPRO != approvedEligibleNominalAPRO {
		return fmt.Errorf("this one-time command accepts only the approved operator declarations: exactly %d APRO nominal circulation and exactly %d APRO eligible nominal; these values are not proof of availability",
			approvedNominalCirculatingAPRO, approvedEligibleNominalAPRO)
	}
	return nil
}

func isZeroKey(key crypto.ValidatorPubKey) bool {
	for _, b := range key {
		if b != 0 {
			return false
		}
	}
	return true
}

func parseHash(text string) (crypto.Hash32, error) {
	var out crypto.Hash32
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("expected exactly 32 bytes of hex")
	}
	copy(out[:], raw)
	if out == (crypto.Hash32{}) {
		return out, fmt.Errorf("zero hash is not permitted")
	}
	return out, nil
}

func validateOfflinePath(path, label string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/tmp" || !strings.HasPrefix(filepath.Clean(path), "/tmp/") {
		return fmt.Errorf("%s path must be an absolute path under /tmp", label)
	}
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("stat %s path: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s path must not be a symlink", label)
	}
	if label == "offline DB" && !info.IsDir() {
		return fmt.Errorf("offline DB path must be a directory")
	}
	if label == "validator set" && !info.Mode().IsRegular() {
		return fmt.Errorf("validator-set path must be a regular file")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil || filepath.Clean(resolved) == "/tmp" || !strings.HasPrefix(filepath.Clean(resolved), "/tmp/") {
		return fmt.Errorf("%s path resolves outside /tmp", label)
	}
	return nil
}

func aproToNAPRO(amount uint64) (uint64, error) {
	if amount > math.MaxUint64/lpod.Unit {
		return 0, fmt.Errorf("APRO amount overflows nAPRO")
	}
	return amount * lpod.Unit, nil
}

func validateNominalAmounts(circulating, eligible uint64) error {
	if circulating < eligible {
		return fmt.Errorf("nominal circulation cannot be less than eligible nominal allocation")
	}
	if circulating > store.LPoDV3NominalCirculationMaxNAPRO {
		return fmt.Errorf("nominal circulation exceeds the 10B APRO cap")
	}
	if eligible < lpod.InitialNAPRO || eligible > store.LPoDV3NominalAllocationMaxNAPRO {
		return fmt.Errorf("eligible nominal allocation must be at least 1B and at most 7B APRO")
	}
	return nil
}

func validateEligibleAgainstValidatorPool(eligible, pool uint64) error {
	const publicAllocation = uint64(9_000_000_000) * lpod.Unit
	const validatorPoolMax = uint64(2_000_000_000) * lpod.Unit
	if pool > validatorPoolMax || pool > publicAllocation || eligible > publicAllocation-pool {
		return fmt.Errorf("eligible nominal allocation overlaps the validator budget or public allocation limit")
	}
	return nil
}
