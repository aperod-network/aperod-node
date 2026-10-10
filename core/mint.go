package core

import (
        "fmt"

        "github.com/aperod/aperod/crypto"
)

// BuildMintTx creates a transparent coinbase-style transaction that mints amount to addr.
//
// Unlike regular RingCT transfers, mint outputs use a deterministic shift of the
// spend public key as OneTimePub: mint_pub = spend_pub + height*G. This keeps the
// output "transparent" — the block explorer can recompute the expected key for any
// height it is scanning, without needing the recipient's view key — while making
// every mint output cryptographically unique per height. Passing height=0 exactly
// reproduces the legacy behavior (mint_pub == spend_pub), which is intentionally
// used for one-off admin mints where a future inclusion height isn't known in
// advance (see restAdminMint); those are far less likely to collide since they
// require the exact same address+amount to be minted more than once.
//
// height must be the exact height the transaction will be included at (as it is
// for the PoA per-block coinbase reward, which is called from produceBlock with
// the block's own height) — otherwise the output will not be recognized by
// scanning/spending code, which recomputes mint_pub from the actual block height.
func BuildMintTx(addr crypto.Address, amount uint64, height uint64) (*Transaction, error) {
        if amount == 0 {
                return nil, fmt.Errorf("mint amount must be > 0")
        }

        _, spendPub, _, err := crypto.DecodeAddress(addr)
        if err != nil {
                return nil, fmt.Errorf("decode address: %w", err)
        }

        // Use a deterministic blind derived from spendPub + amount + height.
        // For height > 0 (block-reward mints) we use V2 which includes height in
        // the derivation so each block's reward has a distinct blind even for the
        // same (address, amount) pair.  For height=0 (one-off admin mints) we
        // keep the legacy V1 formula for backward compatibility with existing UTXOs.
        var blind crypto.BlindFactor
        if height > 0 {
                blind, err = crypto.DeterministicMintBlindV2(spendPub, amount, height)
        } else {
                blind, err = crypto.DeterministicMintBlind(spendPub, amount)
        }
        if err != nil {
                return nil, fmt.Errorf("deterministic mint blind: %w", err)
        }
        commit, err := crypto.Commit(amount, blind)
        if err != nil {
                return nil, fmt.Errorf("pedersen commit: %w", err)
        }

        heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(height))
        if err != nil {
                return nil, fmt.Errorf("derive height pub: %w", err)
        }
        oneTimePub, err := crypto.AddPoints(spendPub, heightPub)
        if err != nil {
                return nil, fmt.Errorf("derive mint one-time pub: %w", err)
        }

        tx := &Transaction{
                Version: TxVersionBase,
                Inputs:  nil, // coinbase — no ring inputs
                Outputs: []Output{{
                        OneTimePub:   oneTimePub, // transparent: spend_pub + height*G
                        AmountCommit: commit,
                }},
                Fee: 0, // mints are fee-exempt (coinbase exemption in mempool)
        }
        return tx, nil
}
