package core

import (
	"crypto/rand"
	"errors"
	"io"
	"math/big"

	"github.com/aperod/aperod/crypto"
)

// Builders distinguish an entropy failure from an ordinary candidate shortfall:
// legacy shortfalls may use Phase 1, but a failure must stop both builder paths.
func (b *TxBuilder) sampleChainDecoys(count int, exclude map[crypto.Point32]bool, clsag bool) ([]DecoyUTXO, error) {
	entropy := b.decoyEntropy
	if entropy == nil {
		entropy = rand.Reader
	}
	if clsag {
		return b.utxoSet.sampleCLSAGDecoys(count, exclude, entropy)
	}
	return b.utxoSet.sampleDecoys(count, exclude, entropy)
}

// sampleDecoyCandidates partially shuffles a call-owned candidate slice.
// rand.Int rejection sampling makes every remaining index equally likely;
// partial Fisher-Yates chooses distinct entries without changing UTXO records.
// Entropy failure discards the entire selection, including already drawn items.
func sampleDecoyCandidates(candidates []*UTXO, count int, entropy io.Reader) ([]DecoyUTXO, error) {
	if count <= 0 || len(candidates) == 0 {
		return nil, nil
	}
	if entropy == nil {
		return nil, errors.New("decoy sampling requires cryptographic entropy")
	}
	want := count
	if want > len(candidates) {
		want = len(candidates)
	}
	var bound big.Int
	for i := 0; i < want; i++ {
		bound.SetInt64(int64(len(candidates) - i))
		offset, err := rand.Int(entropy, &bound)
		if err != nil {
			return nil, err
		}
		j := i + int(offset.Int64())
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}
	out := make([]DecoyUTXO, want)
	for i := range out {
		out[i] = DecoyUTXO{
			OneTimePub:   candidates[i].OneTimePub,
			AmountCommit: candidates[i].AmountCommit,
		}
	}
	return out, nil
}
