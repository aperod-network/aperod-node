package core

import (
	"encoding/binary"
	"fmt"
	"reflect"

	"github.com/aperod/aperod/crypto"
)

// GuardianFundNAPR is 1,000,000,000 APRO expressed in nAPRO. It is part of
// the existing 10B nominal cap, not an emission or a normal wallet transfer.
const GuardianFundNAPR uint64 = 1_000_000_000 * 100_000_000

const (
	GuardianFundVersion   uint8 = 1
	guardianFundExtraSize       = len("APRO-GUARDIAN-FUND") + 1 + 32 + 8
)

var guardianFundDomain = []byte("APRO-GUARDIAN-FUND")

// GuardianFundID is a stable domain identifier for explorers and auditors.
var GuardianFundID = crypto.HashBytes(guardianFundDomain, []byte{GuardianFundVersion})

func guardianFundExtra(anchor crypto.Hash32, height uint64) []byte {
	extra := make([]byte, 0, guardianFundExtraSize)
	extra = append(extra, guardianFundDomain...)
	extra = append(extra, GuardianFundVersion)
	extra = append(extra, anchor[:]...)
	var h [8]byte
	binary.LittleEndian.PutUint64(h[:], height)
	return append(extra, h[:]...)
}

// BuildGuardianFundTx creates the only valid Guardian transaction for an
// anchored fork. The output public key is a hash-to-curve point, deliberately
// not scalar*G, so no spend private key is known. The zero blind makes the
// exact nominal amount independently verifiable.
func BuildGuardianFundTx(chainAnchor crypto.Hash32, activationHeight uint64) (Transaction, error) {
	if chainAnchor == (crypto.Hash32{}) {
		return Transaction{}, fmt.Errorf("guardian fund requires a non-zero chain anchor")
	}
	extra := guardianFundExtra(chainAnchor, activationHeight)
	p := crypto.HashToCurvePoint(append([]byte("aperod/guardian-fund/key/v1"), extra...))
	var pub crypto.Point32
	copy(pub[:], p.Bytes())
	var zero crypto.BlindFactor
	commit, err := crypto.Commit(GuardianFundNAPR, zero)
	if err != nil {
		return Transaction{}, fmt.Errorf("guardian fund commitment: %w", err)
	}
	return Transaction{
		Version: TxVersionGuardianFund,
		Outputs: []Output{{OneTimePub: pub, AmountCommit: commit}},
		Extra:   extra,
	}, nil
}

// GuardianFundTxAt returns true only for the byte-for-byte canonical
// transaction. This makes amount, point, commitment, version and hash-bound
// extra independently consensus-verifiable.
func GuardianFundTxAt(tx *Transaction, chainAnchor crypto.Hash32, activationHeight uint64) bool {
	if tx == nil {
		return false
	}
	if err := tx.Validate(); err != nil {
		return false
	}
	expected, err := BuildGuardianFundTx(chainAnchor, activationHeight)
	return err == nil && reflect.DeepEqual(*tx, expected)
}
