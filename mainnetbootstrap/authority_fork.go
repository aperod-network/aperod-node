package mainnetbootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/aperod/aperod/core"
	chaincrypto "github.com/aperod/aperod/crypto"
)

// BuildSuccessorAuthorityFork creates a future-only authority transition from
// the exact signer and registry at a stopped canonical tip. The caller must
// obtain parentHash and registry from that same closed database snapshot.
func BuildSuccessorAuthorityFork(
	previous *core.ValidatorAuthorityFork,
	checkpointHeight uint64,
	parentHash chaincrypto.Hash32,
	registry core.RegistrySnapshot,
	replacementPrivate chaincrypto.ValidatorPrivKey,
	expectedCurrentSignerHex string,
	expectedReplacementHex string,
) (*core.ValidatorAuthorityFork, string, error) {
	if previous == nil || replacementPrivate == nil {
		return nil, "", errors.New("previous authority and replacement key are required")
	}
	if err := previous.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate historical authority fork: %w", err)
	}
	if previous.Genesis != mustHash(CanonicalMainnetGenesisIndexHashHex) ||
		!previous.Identity.Equals(mustPublicKey(GenesisSignerPublicKeyHex)) {
		return nil, "", errors.New("historical authority is not anchored to the pinned Mainnet genesis identity")
	}
	if checkpointHeight == math.MaxUint64 || checkpointHeight < previous.Height ||
		parentHash == (chaincrypto.Hash32{}) {
		return nil, "", errors.New("canonical checkpoint cannot support a forward authority transition")
	}

	currentSigner := previous.Authority(previous.Identity, checkpointHeight)
	if currentSigner.Hex() != expectedCurrentSignerHex {
		return nil, "", errors.New("current signer differs from the explicitly expected historical signer")
	}
	replacementPublic := replacementPrivate.Public()
	if replacementPublic.Hex() != expectedReplacementHex {
		return nil, "", errors.New("replacement private key does not match the pinned public key")
	}
	if replacementPublic.Equals(previous.Identity) || replacementPublic.Equals(currentSigner) {
		return nil, "", errors.New("replacement signer must differ from both collateral identity and current signer")
	}

	activeValidators := 0
	var identityEntry *core.ValidatorEntry
	for key, entry := range registry.Validators {
		if entry == nil || len(entry.PubKey) != 32 ||
			key != hex.EncodeToString(entry.PubKey) || entry.Seeded {
			return nil, "", errors.New("checkpoint registry contains an invalid or synthetic validator")
		}
		if entry.Status == core.ValidatorActive {
			activeValidators++
			if key == GenesisSignerPublicKeyHex {
				identityEntry = entry
			}
		}
	}
	if activeValidators != 1 || identityEntry == nil ||
		identityEntry.StakeNAPR < core.MinStakeNAPR {
		return nil, "", errors.New("checkpoint must have exactly one active, sufficiently funded Mainnet identity validator")
	}

	registryJSON, err := json.Marshal(registry)
	if err != nil {
		return nil, "", fmt.Errorf("encode canonical registry snapshot: %w", err)
	}
	registryHash := sha256.Sum256(registryJSON)
	fork := &core.ValidatorAuthorityFork{
		Version:             2,
		Genesis:             previous.Genesis,
		Height:              checkpointHeight + 1,
		Parent:              parentHash,
		Identity:            append(chaincrypto.ValidatorPubKey(nil), previous.Identity...),
		Replacement:         append(chaincrypto.ValidatorPubKey(nil), replacementPublic...),
		RegistrySHA256:      registryHash,
		LegacyWitnessSHA256: previous.LegacyWitnessSHA256,
		Previous:            previous.Clone(),
	}
	fork.Possession, err = replacementPrivate.Sign(fork.Message())
	if err != nil {
		return nil, "", fmt.Errorf("sign successor-key possession proof: %w", err)
	}
	if err := fork.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate successor authority fork: %w", err)
	}
	return fork, hex.EncodeToString(registryHash[:]), nil
}

func mustHash(value string) chaincrypto.Hash32 {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		panic("invalid compiled Mainnet hash")
	}
	var result chaincrypto.Hash32
	copy(result[:], decoded)
	return result
}

func mustPublicKey(value string) chaincrypto.ValidatorPubKey {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic("invalid compiled Mainnet public key")
	}
	public, err := chaincrypto.ValidatorPubKeyFromBytes(decoded)
	if err != nil {
		panic("invalid compiled Mainnet public key")
	}
	return public
}
