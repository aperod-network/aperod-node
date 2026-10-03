package main

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

const maxIdentityOutputs = 256

type identityOutput struct {
	TxHash      [32]byte
	OutIdx      uint32
	OneTimePub  crypto.Point32
	TxPubKey    crypto.Point32
	BlockHeight uint64
}

type outputIdentity struct {
	TxHash   [32]byte
	OutIdx   uint32
	KeyImage crypto.KeyImage
}

// identifyOutputRecords establishes ownership using only public output keys and
// spend-key derivation. It deliberately does not read amount, encrypted amount,
// blind, or commitment fields.
func identifyOutputRecords(keys *wallet.DerivedKeys, outputs []identityOutput) ([]outputIdentity, error) {
	if len(outputs) > maxIdentityOutputs {
		return nil, fmt.Errorf("outputs exceeds the %d-output identity chunk limit", maxIdentityOutputs)
	}
	result := make([]outputIdentity, 0, len(outputs))
	var zero crypto.Point32
	for i, output := range outputs {
		var hs *crypto.Scalar32
		if output.TxPubKey == zero {
			heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(output.BlockHeight))
			if err != nil {
				return nil, fmt.Errorf("outputs[%d].block_height: %w", i, err)
			}
			expected, err := crypto.AddPoints(keys.Keys.Spend.Public, heightPub)
			if err != nil {
				return nil, fmt.Errorf("outputs[%d] mint public key: %w", i, err)
			}
			if output.OneTimePub != expected {
				continue
			}
			mintHS := crypto.ScalarFromUint64(output.BlockHeight)
			hs = &mintHS
		} else {
			found, err := crypto.ScanForOutput(
				keys.Keys.View.Private,
				keys.Keys.Spend.Public,
				output.TxPubKey,
				output.OneTimePub,
			)
			if err != nil {
				return nil, fmt.Errorf("outputs[%d] public key scan: %w", i, err)
			}
			if found == nil {
				continue
			}
			hs = found
		}

		oneTimePrivate, err := crypto.AddScalars(*hs, keys.Keys.Spend.Private)
		if err != nil {
			return nil, fmt.Errorf("outputs[%d] spend-key derivation: %w", i, err)
		}
		keyImage, err := crypto.ComputeKeyImage(oneTimePrivate, output.OneTimePub)
		if err != nil {
			return nil, fmt.Errorf("outputs[%d] key-image derivation: %w", i, err)
		}
		keyImage, err = crypto.CanonicalKeyImage(keyImage)
		if err != nil {
			return nil, fmt.Errorf("outputs[%d] key-image canonicalization: %w", i, err)
		}
		result = append(result, outputIdentity{
			TxHash:   output.TxHash,
			OutIdx:   output.OutIdx,
			KeyImage: keyImage,
		})
	}
	return result, nil
}
