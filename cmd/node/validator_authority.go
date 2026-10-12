package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// A file is not authorization merely because it contains a valid possession
// signature. The independently deployed configuration pins its full SHA-256.
func loadValidatorAuthorityFork(cfg config.ConsensusConfig) (*core.ValidatorAuthorityFork, error) {
	if cfg.ValidatorAuthorityForkFile == "" {
		if cfg.ValidatorAuthorityForkSHA256 != "" {
			return nil, fmt.Errorf("authority fork digest without a rule file")
		}
		return nil, nil
	}
	want, err := hex.DecodeString(cfg.ValidatorAuthorityForkSHA256)
	if err != nil || len(want) != 32 {
		return nil, fmt.Errorf("authority fork requires an independently approved SHA-256 pin")
	}
	file, err := os.Open(cfg.ValidatorAuthorityForkFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, fmt.Errorf("authority fork file exceeds limit")
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], want) {
		return nil, fmt.Errorf("authority fork file SHA-256 mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var rule core.ValidatorAuthorityFork
	if err := decoder.Decode(&rule); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("authority fork trailing data")
	}
	if err := rule.Validate(); err != nil {
		return nil, err
	}
	// The old witness signer is allowed ONLY for the approved historical file.
	// No substituted/future witness may reuse compromised trust authority.
	if cfg.LPoDMigrationFile != "" {
		if rule.LegacyWitnessSHA256 == (crypto.Hash32{}) {
			return nil, fmt.Errorf("authority fork requires a pinned historical LPoD witness")
		}
		witness, err := os.ReadFile(cfg.LPoDMigrationFile)
		if err != nil {
			return nil, err
		}
		if crypto.Hash32(sha256.Sum256(witness)) != rule.LegacyWitnessSHA256 {
			return nil, fmt.Errorf("authority fork historical witness mismatch")
		}
	}
	return &rule, nil
}
