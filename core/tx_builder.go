// Package core — RingCT transaction builder.
// Assembles a fully-signed privacy transaction from the sender's owned UTXOs.
//
// Phase 1 limitation: decoy ring members are randomly generated.
// In production (Phase 2+) decoys are real UTXOs sampled from the chain.
package core

import (
        "fmt"
        "sort"

        "github.com/aperod/aperod/crypto"
)

// TxBuilder constructs RingCT transactions.
type TxBuilder struct {
        spendPriv  crypto.Scalar32
        viewPriv   crypto.Scalar32
        spendPub   crypto.Point32
        ownedUTXOs []OwnedUTXO
        feePerByte uint64
}

// NewTxBuilder creates a transaction builder for a wallet.
// ownedUTXOs must come from WalletScanner.ScanChain — amounts must be decrypted.
func NewTxBuilder(
        spendPriv, viewPriv crypto.Scalar32,
        spendPub crypto.Point32,
        ownedUTXOs []OwnedUTXO,
        feePerByte uint64,
) *TxBuilder {
        if feePerByte == 0 {
                feePerByte = 1
        }
        return &TxBuilder{
                spendPriv:  spendPriv,
                viewPriv:   viewPriv,
                spendPub:   spendPub,
                ownedUTXOs: ownedUTXOs,
                feePerByte: feePerByte,
        }
}

// BuildResult contains the signed transaction and its metadata.
type BuildResult struct {
        Tx           Transaction
        ChangeAmount uint64
        TotalFee     uint64
        InputCount   int
        OutputCount  int
}

// Build constructs a signed RingCT transaction.
//
// amount — payment in base units (1 APR = 100_000_000).
// recipient — Aperod address of the payment recipient.
// changeAddr — sender's own address for the change output.
func (b *TxBuilder) Build(amount uint64, recipient, changeAddr crypto.Address) (*BuildResult, error) {
        if amount == 0 {
                return nil, fmt.Errorf("amount must be > 0")
        }
        if err := crypto.Validate(recipient); err != nil {
                return nil, fmt.Errorf("invalid recipient address: %w", err)
        }
        if err := crypto.Validate(changeAddr); err != nil {
                return nil, fmt.Errorf("invalid change address: %w", err)
        }

        // Sort available UTXOs largest-first for greedy selection.
        available := make([]OwnedUTXO, len(b.ownedUTXOs))
        copy(available, b.ownedUTXOs)
        sort.Slice(available, func(i, j int) bool {
                return available[i].Amount > available[j].Amount
        })

        // ── UTXO selection: iterate until fee estimate stabilises ────────────────
        const maxInputs = 8
        var (
                selected     []OwnedUTXO
                totalIn      uint64
                estimatedFee uint64
        )
        for range 3 { // converges in ≤3 passes
                nIn := len(selected)
                if nIn == 0 {
                        nIn = 1
                }
                estimatedFee = txEstimateFee(nIn, 2, b.feePerByte)
                needed := amount + estimatedFee

                selected = nil
                totalIn = 0
                for _, u := range available {
                        if totalIn >= needed {
                                break
                        }
                        selected = append(selected, u)
                        totalIn += u.Amount
                        if len(selected) >= maxInputs {
                                break
                        }
                }
                if totalIn < needed {
                        return nil, fmt.Errorf("insufficient funds: have %d, need %d (amount %d + fee %d)",
                                totalIn, needed, amount, estimatedFee)
                }
                newFee := txEstimateFee(len(selected), 2, b.feePerByte)
                if newFee == estimatedFee {
                        break
                }
                estimatedFee = newFee
        }

        changeAmount := totalIn - amount - estimatedFee
        hasChange := changeAmount > 0

        // ── Build outputs ─────────────────────────────────────────────────────────
        type outEntry struct {
                output Output
                blind  crypto.BlindFactor
                amount uint64
        }

        outEntries := []outEntry{}

        payOut, payBlind, err := txBuildOutput(recipient, amount)
        if err != nil {
                return nil, fmt.Errorf("build payment output: %w", err)
        }
        outEntries = append(outEntries, outEntry{payOut, payBlind, amount})

        if hasChange {
                chOut, chBlind, err := txBuildOutput(changeAddr, changeAmount)
                if err != nil {
                        return nil, fmt.Errorf("build change output: %w", err)
                }
                outEntries = append(outEntries, outEntry{chOut, chBlind, changeAmount})
        }

        // ── Fee commitment ────────────────────────────────────────────────────────
        feeBlind, err := crypto.NewBlindFactor()
        if err != nil {
                return nil, fmt.Errorf("fee blind: %w", err)
        }
        feeCommit, err := crypto.Commit(estimatedFee, feeBlind)
        if err != nil {
                return nil, fmt.Errorf("fee commit: %w", err)
        }

        // ── Build ring inputs and derive one-time spend keys ─────────────────────
        inputs := make([]RingInput, len(selected))
        inputPrivKeys := make([]crypto.Scalar32, len(selected))
        inputRealIdxs := make([]int, len(selected))

        for i, u := range selected {
                // one_time_priv = Hs + spend_priv
                oneTimePriv, err := crypto.AddScalars(u.HsScalar, b.spendPriv)
                if err != nil {
                        return nil, fmt.Errorf("derive one-time priv key [%d]: %w", i, err)
                }
                inputPrivKeys[i] = oneTimePriv

                ring, realIdx, err := txBuildRing(u.OneTimePub)
                if err != nil {
                        return nil, fmt.Errorf("build ring [%d]: %w", i, err)
                }
                inputRealIdxs[i] = realIdx

                ki, err := crypto.ComputeKeyImage(oneTimePriv, u.OneTimePub)
                if err != nil {
                        return nil, fmt.Errorf("compute key image [%d]: %w", i, err)
                }

                inputs[i] = RingInput{
                        KeyImage:     ki,
                        Ring:         ring,
                        AmountCommit: u.AmountCommit,
                }
        }

        // ── Range proofs ──────────────────────────────────────────────────────────
        outputs := make([]Output, len(outEntries))
        rangeProofs := make([]*crypto.RangeProof, len(outEntries))
        for i, e := range outEntries {
                outputs[i] = e.output
                proof, err := crypto.ProveRange(e.amount, e.blind)
                if err != nil {
                        return nil, fmt.Errorf("range proof [%d]: %w", i, err)
                }
                rangeProofs[i] = proof
        }

        // ── Assemble unsigned transaction ─────────────────────────────────────────
        tx := Transaction{
                Version:     TxVersionBase,
                Inputs:      inputs,
                Outputs:     outputs,
                Fee:         estimatedFee,
                FeeCommit:   feeCommit,
                RangeProofs: rangeProofs,
                Signatures:  make([]*crypto.MLSAGSignature, len(inputs)),
        }

        // ── MLSAG sign each input ─────────────────────────────────────────────────
        txHash := tx.Hash()
        for i := range inputs {
                msg := ringSignMessage(txHash, uint32(i))
                sig, err := crypto.MLSAGSign(msg, inputs[i].Ring, inputRealIdxs[i], inputPrivKeys[i])
                if err != nil {
                        return nil, fmt.Errorf("mlsag sign [%d]: %w", i, err)
                }
                tx.Signatures[i] = sig
                tx.Inputs[i].KeyImage = sig.KeyImage
        }

        return &BuildResult{
                Tx:           tx,
                ChangeAmount: changeAmount,
                TotalFee:     estimatedFee,
                InputCount:   len(inputs),
                OutputCount:  len(outputs),
        }, nil
}

// ─── Private helpers ──────────────────────────────────────────────────────────

// txBuildOutput creates an Output for addr carrying amount in base units.
func txBuildOutput(addr crypto.Address, amount uint64) (Output, crypto.BlindFactor, error) {
        _, spendPub, viewPub, err := crypto.DecodeAddress(addr)
        if err != nil {
                return Output{}, crypto.BlindFactor{}, fmt.Errorf("decode address: %w", err)
        }

        so, err := crypto.CreateStealthOutput(spendPub, viewPub)
        if err != nil {
                return Output{}, crypto.BlindFactor{}, fmt.Errorf("stealth output: %w", err)
        }

        blind, err := crypto.NewBlindFactor()
        if err != nil {
                return Output{}, crypto.BlindFactor{}, err
        }
        commit, err := crypto.Commit(amount, blind)
        if err != nil {
                return Output{}, crypto.BlindFactor{}, err
        }

        encAmount := EncryptAmount(amount, &so.HsScalar)

        return Output{
                OneTimePub:   so.OneTimePub,
                TxPubKey:     so.TxPubKey,
                AmountCommit: commit,
                EncAmount:    encAmount,
        }, blind, nil
}

// txBuildRing assembles a ring of RingSize with the real key at a deterministic
// position (derived from the key bytes so signing is consistent).
// Phase 1: decoys are randomly generated keys; Phase 2+ will use real chain UTXOs.
func txBuildRing(realPub crypto.Point32) ([]crypto.RingMember, int, error) {
        ring := make([]crypto.RingMember, crypto.RingSize)
        for i := range ring {
                decoy, err := crypto.GenerateWalletKeys()
                if err != nil {
                        return nil, 0, err
                }
                ring[i] = decoy.Spend.Public
        }
        realIdx := int(realPub[0]) % crypto.RingSize
        ring[realIdx] = realPub
        return ring, realIdx, nil
}

// ExportedEstimateFee is the public wrapper for fee estimation used by the
// wallet package and external callers.
func ExportedEstimateFee(nIn, nOut int, feePerByte uint64) uint64 {
        return txEstimateFee(nIn, nOut, feePerByte)
}

// txEstimateFee estimates the fee for a transaction with nIn inputs and nOut outputs.
func txEstimateFee(nIn, nOut int, feePerByte uint64) uint64 {
        const (
                inputBytes     = 416  // keyimage(32) + ring(11×32) + commit(32)
                sigBytes       = 384  // C0(32) + SS(11×32)
                outputBytes    = 104  // oneTimePub(32) + txPub(32) + commit(32) + enc(8)
                rangeProofBytes = 6144 // 3 × 64 × 32
                headerBytes    = 100
        )
        total := headerBytes + nIn*(inputBytes+sigBytes) + nOut*(outputBytes+rangeProofBytes)
        return uint64(total) * feePerByte
}
