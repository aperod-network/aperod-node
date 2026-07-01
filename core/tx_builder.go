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
}

// NewTxBuilder creates a transaction builder for a wallet.
// ownedUTXOs must come from WalletScanner.ScanChain — amounts must be decrypted.
// The fee is always FlatFee (0.5 APRO); the feePerByte parameter is kept for
// backwards-compatibility but ignored.
func NewTxBuilder(
        spendPriv, viewPriv crypto.Scalar32,
        spendPub crypto.Point32,
        ownedUTXOs []OwnedUTXO,
        _ uint64, // feePerByte — deprecated, flat fee is used instead
) *TxBuilder {
        return &TxBuilder{
                spendPriv:  spendPriv,
                viewPriv:   viewPriv,
                spendPub:   spendPub,
                ownedUTXOs: ownedUTXOs,
        }
}

// BuildResult contains the signed transaction and its metadata.
type BuildResult struct {
        Tx           Transaction
        ChangeAmount uint64
        TotalFee     uint64
        InputCount   int
        OutputCount  int
        // ChangeBlind is the Pedersen blinding factor of the change output.
        // Callers must store this in utxo_blinds to be able to spend the change later.
        // Zero if there is no change output.
        ChangeBlind  crypto.BlindFactor
        // ChangeOutIdx is the index of the change output within Tx.Outputs.
        // -1 if there is no change output.
        ChangeOutIdx int
        // PayBlind is the Pedersen blinding factor of the payment output.
        // Must be stored in utxo_blinds for the recipient so they can spend received funds.
        PayBlind  crypto.BlindFactor
        // PayOutIdx is the index of the payment output within Tx.Outputs (always 0).
        PayOutIdx int
}

// Build constructs a signed RingCT transaction.
//
// amount — payment in base units (1 APRO = 100_000_000).
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
        // Flat fee is fixed regardless of transaction size.
        estimatedFee = FlatFee
        needed := amount + estimatedFee

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

        changeAmount := totalIn - amount - estimatedFee
        hasChange := changeAmount > 0

        // ── Fee commitment ────────────────────────────────────────────────────────
        feeBlind, err := crypto.NewBlindFactor()
        if err != nil {
                return nil, fmt.Errorf("fee blind: %w", err)
        }
        feeCommit, err := crypto.Commit(estimatedFee, feeBlind)
        if err != nil {
                return nil, fmt.Errorf("fee commit: %w", err)
        }

        // ── Pedersen blind balancing ──────────────────────────────────────────────
        // Commitment balance constraint: ΣC_in = ΣC_out + C_fee
        // Blind constraint:              Σr_in = Σr_out + r_fee
        //
        // Strategy (Monero-style):
        //   • If hasChange: pay blind is random, change blind balances.
        //     change_blind = Σr_in - r_pay - r_fee
        //   • If !hasChange: pay blind balances directly.
        //     pay_blind = Σr_in - r_fee
        inBlinds := make([]crypto.BlindFactor, len(selected))
        for i, u := range selected {
                inBlinds[i] = u.Blind
        }

        // ── Build outputs ─────────────────────────────────────────────────────────
        type outEntry struct {
                output Output
                blind  crypto.BlindFactor
                amount uint64
        }

        var outEntries []outEntry
        var changeBlindResult crypto.BlindFactor
        var payBlindResult crypto.BlindFactor
        changeOutIdx := -1

        if hasChange {
                // Payment blind: random (recipient cannot see our change balance)
                payOut, payBlind, err := txBuildOutput(recipient, amount)
                if err != nil {
                        return nil, fmt.Errorf("build payment output: %w", err)
                }
                outEntries = append(outEntries, outEntry{payOut, payBlind, amount})
                payBlindResult = payBlind

                // Change blind: computed so that ΣC_in == C_pay + C_change + C_fee
                changeBlind, err := crypto.BlindSum(inBlinds, []crypto.BlindFactor{payBlind, feeBlind})
                if err != nil {
                        return nil, fmt.Errorf("change blind sum: %w", err)
                }
                chOut, err := txBuildOutputWithBlind(changeAddr, changeAmount, changeBlind)
                if err != nil {
                        return nil, fmt.Errorf("build change output: %w", err)
                }
                outEntries = append(outEntries, outEntry{chOut, changeBlind, changeAmount})
                changeBlindResult = changeBlind
                changeOutIdx = 1
        } else {
                // No change: payment blind balances the equation directly
                // pay_blind = Σr_in - r_fee
                payBlind, err := crypto.BlindSum(inBlinds, []crypto.BlindFactor{feeBlind})
                if err != nil {
                        return nil, fmt.Errorf("pay blind sum: %w", err)
                }
                payOut, err := txBuildOutputWithBlind(recipient, amount, payBlind)
                if err != nil {
                        return nil, fmt.Errorf("build payment output: %w", err)
                }
                outEntries = append(outEntries, outEntry{payOut, payBlind, amount})
                payBlindResult = payBlind
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
                ChangeBlind:  changeBlindResult,
                ChangeOutIdx: changeOutIdx,
                PayBlind:     payBlindResult,
                PayOutIdx:    0,
        }, nil
}

// txBuildOutputWithBlind creates an Output using a specified blind factor.
// Used for the change output and the "no change" payment path where the blind
// is computed to satisfy the Pedersen commitment balance constraint.
func txBuildOutputWithBlind(addr crypto.Address, amount uint64, blind crypto.BlindFactor) (Output, error) {
        _, spendPub, viewPub, err := crypto.DecodeAddress(addr)
        if err != nil {
                return Output{}, fmt.Errorf("decode address: %w", err)
        }

        so, err := crypto.CreateStealthOutput(spendPub, viewPub)
        if err != nil {
                return Output{}, fmt.Errorf("stealth output: %w", err)
        }

        commit, err := crypto.Commit(amount, blind)
        if err != nil {
                return Output{}, err
        }

        encAmount := EncryptAmount(amount, &so.HsScalar)

        return Output{
                OneTimePub:   so.OneTimePub,
                TxPubKey:     so.TxPubKey,
                AmountCommit: commit,
                EncAmount:    encAmount,
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

// Estimated serialized byte sizes used for fee calculation.
// These mirror the formula in Transaction.Size() (transaction.go).
const (
        // txOverheadBytes: version(1) + fee(8) + feeCommit(32).
        txOverheadBytes = 41
        // txBytesPerInput: keyImage(32) + ring(16×32) + amountCommit(32)
        //                  + MLSAG c0(32) + MLSAG ss(11×32) + MLSAG keyImage(32).
        txBytesPerInput = 832
        // txBytesPerOutput: oneTimePub(32) + txPubKey(32) + amountCommit(32)
        //                   + encAmount(8) + rangeProof(675).
        txBytesPerOutput = 779
)

// ExportedEstimateFee returns the estimated fee in base units for a transaction
// with nInputs inputs and nOutputs outputs at the given feePerByte rate.
// The estimate scales linearly with the number of inputs/outputs and with the
// fee rate, making it suitable for fee-bumping and wallet UI display.
func ExportedEstimateFee(nInputs, nOutputs int, feePerByte uint64) uint64 {
        if feePerByte == 0 {
                feePerByte = 1
        }
        size := uint64(txOverheadBytes + nInputs*txBytesPerInput + nOutputs*txBytesPerOutput)
        return size * feePerByte
}

// txEstimateFee is the internal variant used during transaction construction.
func txEstimateFee(nInputs, nOutputs int, feePerByte uint64) uint64 {
        return ExportedEstimateFee(nInputs, nOutputs, feePerByte)
}
