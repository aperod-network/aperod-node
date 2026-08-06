package main

// validator_list.go — helpers for constructing the initial validator list
// used to seed consensus.Config.Validators and the ValidatorRegistry.
//
// Extracted from run() so the seeding logic can be unit-tested independently
// of the full node startup sequence.

import (
	"encoding/hex"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// buildValidatorList constructs the validators slice that seeds the registry
// and the consensus engine's static fallback list.
//
// Non-validator mode (nonValidator=true):
//
//	Decodes and returns the public keys from genesisValidators (hex strings as
//	they appear in genesis.yaml).  This ensures handleIncomingBlock's
//	isKnownValidator() and proposer-slot checks recognise the real network
//	validators so the node can sync without producing blocks.
//	The node's own myKey must NOT appear in the returned list.
//
// Validator mode (nonValidator=false):
//
//	Returns a single-element slice containing myKey.Public().  This is the
//	single-validator / testnet design where this node IS the proposer.
func buildValidatorList(
	nonValidator bool,
	genesisValidators []string,
	myKey *crypto.LockedValidatorKey,
) ([]crypto.ValidatorPubKey, error) {
	if nonValidator {
		validators := make([]crypto.ValidatorPubKey, 0, len(genesisValidators))
		for _, hexPub := range genesisValidators {
			pubBytes, err := hex.DecodeString(hexPub)
			if err != nil {
				return nil, fmt.Errorf("parse genesis validator key %q: %w", hexPub, err)
			}
			pub, err := crypto.ValidatorPubKeyFromBytes(pubBytes)
			if err != nil {
				return nil, fmt.Errorf("invalid genesis validator key %q: %w", hexPub, err)
			}
			validators = append(validators, pub)
		}
		return validators, nil
	}
	return []crypto.ValidatorPubKey{myKey.Public()}, nil
}
