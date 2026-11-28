package api

import (
	"encoding/hex"
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
)

type mainnetGenesisIdentity struct {
	genesisHeaderHash    crypto.Hash32
	genesisIndexHash     crypto.Hash32
	checkpointHeight     uint64
	checkpointHeaderHash crypto.Hash32
	checkpointIndexHash  crypto.Hash32
	signedBootstrap      mainnetbootstrap.SignedStatement
}

// SetMainnetBootstrapIdentity pins API block responses to the already-verified
// Mainnet bootstrap statement. It fails closed if the opened store is bound to
// a different genesis or checkpoint.
func (s *Server) SetMainnetBootstrapIdentity(signed mainnetbootstrap.SignedStatement) error {
	if err := signed.Verify(); err != nil {
		return fmt.Errorf("verify Mainnet bootstrap identity: %w", err)
	}
	if s.blockStore == nil {
		return fmt.Errorf("mainnet API identity requires canonical storage")
	}
	statement := signed.Statement
	identity := &mainnetGenesisIdentity{
		checkpointHeight: statement.CheckpointHeight,
		signedBootstrap:  signed,
	}
	for _, value := range []struct {
		name string
		text string
		dst  *crypto.Hash32
	}{
		{"genesis header hash", statement.GenesisHeaderHash, &identity.genesisHeaderHash},
		{"genesis index hash", statement.GenesisIndexHash, &identity.genesisIndexHash},
		{"checkpoint header hash", statement.CheckpointHeaderHash, &identity.checkpointHeaderHash},
		{"checkpoint index hash", statement.CheckpointIndexHash, &identity.checkpointIndexHash},
	} {
		raw, err := hex.DecodeString(value.text)
		if err != nil || len(raw) != len(value.dst) {
			return fmt.Errorf("invalid signed Mainnet %s", value.name)
		}
		copy(value.dst[:], raw)
	}
	genesis, found, err := s.blockStore.GetCanonicalHash(0)
	if err != nil {
		return fmt.Errorf("read Mainnet genesis index: %w", err)
	}
	if !found || genesis != identity.genesisIndexHash {
		return fmt.Errorf("opened canonical store does not match the signed Mainnet genesis index")
	}
	checkpoint, found, err := s.blockStore.GetCanonicalHash(identity.checkpointHeight)
	if err != nil {
		return fmt.Errorf("read Mainnet checkpoint index: %w", err)
	}
	if !found || checkpoint != identity.checkpointIndexHash {
		return fmt.Errorf("opened canonical store does not match the signed Mainnet checkpoint index")
	}
	s.mainnetIdentity = identity
	return nil
}
