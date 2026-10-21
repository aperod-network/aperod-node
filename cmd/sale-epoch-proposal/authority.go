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
	"github.com/aperod/aperod/store"
)

// Resolve only the independently pinned, canonical authority fork already
// configured on the producer. Native validator identity is not its signing key.
func signingAuthority(cfg config.ConsensusConfig, d *store.DB) (*core.ValidatorAuthorityFork, error) {
	if cfg.ValidatorAuthorityForkFile == "" {
		if cfg.ValidatorAuthorityForkSHA256 != "" {
			return nil, fmt.Errorf("authority digest without rule")
		}
		return nil, nil
	}
	want, err := hex.DecodeString(cfg.ValidatorAuthorityForkSHA256)
	if err != nil || len(want) != 32 {
		return nil, fmt.Errorf("independent authority digest required")
	}
	f, err := os.Open(cfg.ValidatorAuthorityForkFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, fmt.Errorf("authority rule exceeds limit")
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], want) {
		return nil, fmt.Errorf("authority digest mismatch")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rule core.ValidatorAuthorityFork
	if err := dec.Decode(&rule); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("authority trailing data")
	}
	if err := rule.Validate(); err != nil {
		return nil, err
	}
	for _, anchor := range []struct {
		height uint64
		hash   crypto.Hash32
	}{{0, rule.Genesis}, {rule.Height - 1, rule.Parent}} {
		hash, found, err := d.GetCanonicalHash(anchor.height)
		if err != nil || !found || hash != anchor.hash {
			return nil, fmt.Errorf("authority canonical anchor mismatch")
		}
	}
	if cfg.LPoDMigrationFile != "" {
		witness, err := os.ReadFile(cfg.LPoDMigrationFile)
		if err != nil || rule.LegacyWitnessSHA256 == (crypto.Hash32{}) ||
			crypto.Hash32(sha256.Sum256(witness)) != rule.LegacyWitnessSHA256 {
			return nil, fmt.Errorf("authority historical witness mismatch")
		}
	}
	return &rule, nil
}
