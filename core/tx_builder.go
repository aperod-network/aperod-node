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
        spendPriv    crypto.Scalar32
        viewPriv     crypto.Scalar32
        spendPub     crypto.Point32
        ownedUTXOs   []OwnedUTXO
        feePerByte   uint64   // base-fee + optional tip per byte in nAPRO
        utxoSet      *UTXOSet // optional; if set, real chain UTXOs are used as ring decoys (Phase 2)
}

// NewTxBuilder creates a transaction builder for a wallet.
// ownedUTXOs must come from WalletScanner.ScanChain — amounts must be decrypted.
//
// feePerByte is the total fee per byte the sender is willing to pay (nAPRO/byte).
// It should be at least the current network BaseFee (from the latest block header)
// plus any priority tip the sender wants to add.  If zero, InitialBaseFeePerByte is used.
// Fee = EstimatedTxSize × feePerByte; 100% of BaseFee portion is burned, tip goes to validator.
func NewTxBuilder(
        spendPriv, viewPriv crypto.Scalar32,
        spendPub crypto.Point32,
        ownedUTXOs []OwnedUTXO,
        feePerByte uint64,
) *TxBuilder {
        if feePerByte == 0 {
                feePerByte = InitialBaseFeePerByte
        }
        return &TxBuilder{
                spendPriv:  spendPriv,
                viewPriv:   viewPriv,
                spendPub:   spendPub,
                ownedUTXOs: ownedUTXOs,
                feePerByte: feePerByte,
        }
}

// WithDecoySet wires a live UTXOSet so that ring decoy slots are filled with
// real on-chain UTXOs (Phase 2).  Without this, decoys are randomly generated
// keys (Phase 1).  Returns the builder for chaining.
func (b *TxBuilder) WithDecoySet(utxos *UTXOSet) *TxBuilder {
        b.utxoSet = utxos
        return b
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
        // RealDecoyCount is the total number of ring slots filled with real
        // on-chain UTXOs (Phase 2 decoys) across all inputs.
        RealDecoyCount int
        // FallbackDecoyCount is the total number of ring slots that could not be
        // filled with real decoys and fell back to randomly-generated Phase 1 keys.
        // A non-zero value means privacy is degraded: the ring contains provably
        // fake members that can be distinguished from real UTXOs.
        FallbackDecoyCount int
        // SelectedUTXOs are the owned UTXOs that were consumed as real inputs,
        // ordered identically to Tx.Inputs.  Callers use this to map a
        // per-input verification failure (e.g. "double-spend at input i") back
        // to the exact source UTXO (TxHash, OutputIndex) so wallets can skip
        // only the failing candidate instead of discarding all of them.
        SelectedUTXOs []OwnedUTXO
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
        isBurn := crypto.IsBurnAddress(recipient)

        // Sort available UTXOs largest-first for greedy selection.
        available := make([]OwnedUTXO, len(b.ownedUTXOs))
        copy(available, b.ownedUTXOs)
        sort.Slice(available, func(i, j int) bool {
                return available[i].Amount > available[j].Amount
        })

        // Deduplicate by OneTimePub: outputs that share a one-time public key
        // share the same key image (e.g. height-0 admin mints to one address —
        // OneTimePub = spendPub + 0*G for all of them).  A transaction spending
        // two such outputs would carry duplicate key images and be rejected by
        // consensus, so only one of them can ever be spent.  Keep the largest
        // (list is sorted largest-first, so the first occurrence wins).
        {
                seen := make(map[crypto.Point32]struct{}, len(available))
                uniq := available[:0]
                for _, u := range available {
                        if _, dup := seen[u.OneTimePub]; dup {
                                continue
                        }
                        seen[u.OneTimePub] = struct{}{}
                        uniq = append(uniq, u)
                }
                available = uniq
        }

        // ── UTXO selection: iterate until fee estimate stabilises ────────────────
        // Fee = estimated_tx_size_bytes × feePerByte.
        // We iterate: pick UTXOs → estimate size → recalculate fee → check again.
        // In practice this converges in ≤2 iterations because adding one UTXO
        // changes fee by at most one input's weight (~576 bytes × feePerByte).
        const maxInputs = 8
        var (
                selected     []OwnedUTXO
                totalIn      uint64
                estimatedFee uint64
        )
        // Rough initial fee: assume 1 input, 2 outputs (pay + change).
        // txBytesPerInput already includes the MLSAG signature bytes.
        // txBytesPerOutput already includes the range proof bytes.
        initialOutputs := 2
        burnExtraBytes := 0
        if isBurn {
                initialOutputs = 1
                burnExtraBytes = len(IntentionalBurnExtra(amount))
        }
        initialSize := txOverheadBytes + 1*txBytesPerInput + initialOutputs*txBytesPerOutput + burnExtraBytes
        estimatedFee = uint64(initialSize) * b.feePerByte
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
        // Recalculate fee based on actual number of selected inputs.
        nOut := 2 // pay + change; refined below
        if isBurn {
                nOut = 1 // possible change output
        }
        actualSize := txOverheadBytes + len(selected)*txBytesPerInput + nOut*txBytesPerOutput + burnExtraBytes
        estimatedFee = uint64(actualSize) * b.feePerByte
        needed = amount + estimatedFee

        if totalIn < needed {
                return nil, fmt.Errorf("insufficient funds: have %d, need %d (amount %d + fee %d)",
                        totalIn, needed, amount, estimatedFee)
        }

        changeAmount := totalIn - amount - estimatedFee
        hasChange := changeAmount > 0
        totalFee := estimatedFee
        if isBurn {
                if totalFee > ^uint64(0)-amount {
                        return nil, fmt.Errorf("intentional burn fee overflows uint64")
                }
                totalFee += amount
        }

        // ── Fee commitment ────────────────────────────────────────────────────────
        // Fee is a public plaintext value, so its Pedersen commitment uses a zero
        // blinding factor.  This lets VerifyTx enforce C_fee == Commit(fee, 0),
        // closing the negative-fee inflation path where an attacker could set C_fee
        // to a commitment to a negative number while keeping ΣC_in = ΣC_out + C_fee
        // balanced.  The blind-balance constraint still holds with r_fee = 0:
        //   Σr_in = Σr_out + 0  →  Σr_in = Σr_out.
        var feeBlind crypto.BlindFactor // zero blind — fee is public, not hidden
        feeCommit, err := crypto.Commit(totalFee, feeBlind)
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
		if err := validateOwnedUTXOOpening(u); err != nil {
			return nil, fmt.Errorf("input opening [%d]: %w", i, err)
		}
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

        if isBurn && hasChange {
                changeBlind, err := crypto.BlindSum(inBlinds, []crypto.BlindFactor{feeBlind})
                if err != nil {
                        return nil, fmt.Errorf("change blind sum: %w", err)
                }
                chOut, err := txBuildOutputWithBlind(changeAddr, changeAmount, changeBlind)
                if err != nil {
                        return nil, fmt.Errorf("build change output: %w", err)
                }
                outEntries = append(outEntries, outEntry{chOut, changeBlind, changeAmount})
                changeBlindResult = changeBlind
                changeOutIdx = 0
        } else if hasChange {
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
        } else if !isBurn {
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

        // ── Sample decoys for Phase 2 ring construction ───────────────────────────
        // Each ring needs RingSize-1 decoys.  Exclude the real inputs' pub keys
        // so the wallet's own outputs are not also used as decoys in the same tx.
        var allDecoys []DecoyUTXO
        if b.utxoSet != nil {
                excludePubs := make(map[crypto.Point32]bool, len(selected))
                for _, u := range selected {
                        excludePubs[u.OneTimePub] = true
                }
                need := len(selected) * (crypto.RingSize - 1)
                allDecoys = b.utxoSet.SampleDecoys(need, excludePubs)
        }

        // ── Build ring inputs and derive one-time spend keys ─────────────────────
        inputs := make([]RingInput, len(selected))
        inputPrivKeys := make([]crypto.Scalar32, len(selected))
        inputRealIdxs := make([]int, len(selected))
        var totalFallbackDecoys int
        var totalRealDecoys int

        for i, u := range selected {
                // one_time_priv = Hs + spend_priv
                oneTimePriv, err := crypto.AddScalars(u.HsScalar, b.spendPriv)
                if err != nil {
                        return nil, fmt.Errorf("derive one-time priv key [%d]: %w", i, err)
                }
                inputPrivKeys[i] = oneTimePriv

                // Assign a non-overlapping slice of decoys to this ring.
                start := i * (crypto.RingSize - 1)
                end := start + (crypto.RingSize - 1)
                if end > len(allDecoys) {
                        end = len(allDecoys)
                }
                var ringDecoys []DecoyUTXO
                if start < end {
                        ringDecoys = allDecoys[start:end]
                }

                ring, realIdx, fallbacks, err := txBuildRing(u.OneTimePub, ringDecoys)
                if err != nil {
                        return nil, fmt.Errorf("build ring [%d]: %w", i, err)
                }
                inputRealIdxs[i] = realIdx
                totalFallbackDecoys += fallbacks
                totalRealDecoys += (crypto.RingSize - 1) - fallbacks

                ki, err := crypto.ComputeKeyImage(oneTimePriv, u.OneTimePub)
                if err != nil {
                        return nil, fmt.Errorf("compute key image [%d]: %w", i, err)
                }

                inputs[i] = RingInput{
                        KeyImage:     ki,
                        Ring:         ring,
                        AmountCommit: u.AmountCommit,
			RealIndex:    uint8(realIdx),
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
		Version:     TxVersionCommitmentBinding,
                Inputs:      inputs,
                Outputs:     outputs,
                Fee:         totalFee,
                FeeCommit:   feeCommit,
                RangeProofs: rangeProofs,
                Signatures:  make([]*crypto.MLSAGSignature, len(inputs)),
        }
        if isBurn {
                tx.Extra = IntentionalBurnExtra(amount)
        }

        // ── MLSAG sign each input ─────────────────────────────────────────────────
        txHash := tx.Hash()
        for i := range inputs {
                msg := ringSignMessage(txHash, uint32(i))
		sig, err := crypto.MLSAGSignV4(
			msg,
			inputs[i].Ring,
			inputRealIdxs[i],
			inputPrivKeys[i],
			selected[i].Blind,
			selected[i].Amount,
		)
                if err != nil {
                        return nil, fmt.Errorf("mlsag sign [%d]: %w", i, err)
                }
                tx.Signatures[i] = sig
                tx.Inputs[i].KeyImage = sig.KeyImage
        }

        return &BuildResult{
                Tx:                 tx,
                ChangeAmount:       changeAmount,
                TotalFee:           totalFee,
                InputCount:         len(inputs),
                OutputCount:        len(outputs),
                ChangeBlind:        changeBlindResult,
                ChangeOutIdx:       changeOutIdx,
                PayBlind:           payBlindResult,
                PayOutIdx:          func() int { if isBurn { return -1 }; return 0 }(),
                RealDecoyCount:     totalRealDecoys,
                FallbackDecoyCount: totalFallbackDecoys,
                SelectedUTXOs:      selected,
        }, nil
}

func validateOwnedUTXOOpening(utxo OwnedUTXO) error {
	commitment, err := crypto.Commit(utxo.Amount, utxo.Blind)
	if err != nil {
		return fmt.Errorf("invalid amount or blind: %w", err)
	}
	if commitment != utxo.AmountCommit {
		return fmt.Errorf("amount/blind do not open the selected UTXO commitment")
	}
	return nil
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

        // Deterministic blind derived from shared ECDH secret so the recipient
        // can always recover it with their view key (no external storage needed).
        blind, err := crypto.DeterministicPaymentBlind(so.HsScalar, amount)
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
// position derived from the key bytes (so signing is consistent across retries).
//
// Phase 2 (decoys provided): decoy slots use real on-chain UTXOs from the supplied
// slice.  Each DecoyUTXO contributes its OneTimePub to the ring; the caller must
// supply at least RingSize-1 entries.  If fewer are provided, remaining slots are
// filled with randomly-generated keys (Phase 1 fallback for that slot).
//
// Phase 1 (decoys nil or empty): all decoy slots use randomly-generated keys.
//
// Returns (ring, realIdx, fallbackCount, error) where fallbackCount is the number
// of slots that could not be filled with a real decoy and used a random key instead.
// A non-zero fallbackCount means privacy is degraded for this ring.
func txBuildRing(realPub crypto.Point32, decoys []DecoyUTXO) ([]crypto.RingMember, int, int, error) {
        realIdx := int(realPub[0]) % crypto.RingSize
        ring := make([]crypto.RingMember, crypto.RingSize)

        di := 0
        fallbackCount := 0
        for i := range ring {
                if i == realIdx {
                        continue
                }
                if di < len(decoys) {
                        ring[i] = decoys[di].OneTimePub
                        di++
                } else {
                        // Phase 1 fallback: not enough real decoys — generate a random key.
                        fake, err := crypto.GenerateWalletKeys()
                        if err != nil {
                                return nil, 0, 0, err
                        }
                        ring[i] = fake.Spend.Public
                        fallbackCount++
                }
        }
        ring[realIdx] = realPub
        return ring, realIdx, fallbackCount, nil
}

// MaxSpendableResult describes the maximum amount that a single Build call
// can actually send, computed with the exact same coin-selection rules that
// Build uses (dedup by OneTimePub, largest-first greedy, ≤8 inputs,
// fee = size × feePerByte with 2 outputs).
type MaxSpendableResult struct {
        // MaxAmount is the largest payment amount (nAPRO) for which
        // Build(MaxAmount, …) succeeds on the current UTXO set.  Zero when
        // nothing is spendable (no UTXOs, or every candidate is dust below fee).
        MaxAmount uint64
        // Fee is the exact fee that Build will charge for MaxAmount.
        Fee uint64
        // InputCount is the number of inputs Build will select for MaxAmount.
        InputCount int
        // SpendableTotal is the sum of all deduplicated UTXO amounts — the
        // theoretical ceiling before fee and the 8-input cap are applied.
        SpendableTotal uint64
        // UTXOCount is the number of UTXOs remaining after OneTimePub dedup.
        UTXOCount int
}

// MaxSpendable computes the maximum sendable amount by replaying Build's
// exact selection algorithm.  It must stay in lockstep with Build: any change
// to selection, dedup, or fee rules there must be mirrored here, otherwise
// "send max" transactions fail with insufficient-funds on the first try.
func (b *TxBuilder) MaxSpendable() MaxSpendableResult {
        // Sort + dedup exactly like Build.
        available := make([]OwnedUTXO, len(b.ownedUTXOs))
        copy(available, b.ownedUTXOs)
        sort.Slice(available, func(i, j int) bool {
                return available[i].Amount > available[j].Amount
        })
        {
                seen := make(map[crypto.Point32]struct{}, len(available))
                uniq := available[:0]
                for _, u := range available {
                        if _, dup := seen[u.OneTimePub]; dup {
                                continue
                        }
                        seen[u.OneTimePub] = struct{}{}
                        uniq = append(uniq, u)
                }
                available = uniq
        }

        res := MaxSpendableResult{UTXOCount: len(available)}
        for _, u := range available {
                res.SpendableTotal += u.Amount
        }
        if len(available) == 0 {
                return res
        }

        const maxInputs = 8
        limit := len(available)
        if limit > maxInputs {
                limit = maxInputs
        }

        // Candidate amounts: take the k largest inputs and send everything
        // minus the exact fee for k inputs / 2 outputs.  For each candidate,
        // simulate Build's greedy selection to confirm it would succeed —
        // Build stops adding inputs once the rough 1-input fee estimate is
        // covered, so a candidate can select fewer inputs than k and fail the
        // final fee check.  Keep the largest candidate that passes.
        var prefix uint64
        prefixSums := make([]uint64, limit+1)
        for k := 1; k <= limit; k++ {
                prefix += available[k-1].Amount
                prefixSums[k] = prefix
        }
        for k := 1; k <= limit; k++ {
                fee := txEstimateFee(k, 2, b.feePerByte)
                if prefixSums[k] <= fee {
                        continue
                }
                amount := prefixSums[k] - fee
                selCount, selTotal := simulateGreedySelection(available, amount, b.feePerByte)
                finalFee := txEstimateFee(selCount, 2, b.feePerByte)
                if selTotal >= amount+finalFee && amount > res.MaxAmount {
                        res.MaxAmount = amount
                        res.Fee = finalFee
                        res.InputCount = selCount
                }
        }
        return res
}

// FeeEstimateResult is the outcome of EstimateFeeForAmount — a dry run of
// Build's coin selection for a specific payment amount.
type FeeEstimateResult struct {
	// Fee is the exact fee (nAPRO) Build will charge for this amount.
	Fee uint64
	// InputCount is the number of inputs Build will select.
	InputCount int
	// TxSizeBytes is the estimated serialized size used for the fee.
	TxSizeBytes int
	// Sufficient reports whether the selected inputs cover amount+Fee —
	// i.e. whether Build(amount, …) would succeed on this UTXO set.
	Sufficient bool
	// SpendableTotal is the sum of deduplicated UTXO amounts.
	SpendableTotal uint64
	// UTXOCount is the number of UTXOs remaining after OneTimePub dedup.
	UTXOCount int
}

// EstimateFeeForAmount replays Build's exact selection for a given payment
// amount without constructing a transaction: sort largest-first, dedup by
// OneTimePub, greedy-select against the rough 1-input fee, then recompute the
// fee from the actual selected input count.  It must stay in lockstep with
// Build — any change to selection, dedup, or fee rules there must be mirrored
// here, or wallets will show a different fee than the node charges.
func (b *TxBuilder) EstimateFeeForAmount(amount uint64) FeeEstimateResult {
	// Sort + dedup exactly like Build.
	available := make([]OwnedUTXO, len(b.ownedUTXOs))
	copy(available, b.ownedUTXOs)
	sort.Slice(available, func(i, j int) bool {
		return available[i].Amount > available[j].Amount
	})
	{
		seen := make(map[crypto.Point32]struct{}, len(available))
		uniq := available[:0]
		for _, u := range available {
			if _, dup := seen[u.OneTimePub]; dup {
				continue
			}
			seen[u.OneTimePub] = struct{}{}
			uniq = append(uniq, u)
		}
		available = uniq
	}

	res := FeeEstimateResult{UTXOCount: len(available)}
	for _, u := range available {
		res.SpendableTotal += u.Amount
	}

	selCount, selTotal := simulateGreedySelection(available, amount, b.feePerByte)
	if selCount < 1 {
		selCount = 1 // a tx always has ≥1 input
	}
	fee := txEstimateFee(selCount, 2, b.feePerByte)
	res.Fee = fee
	res.InputCount = selCount
	res.TxSizeBytes = txOverheadBytes + selCount*txBytesPerInput + 2*txBytesPerOutput
	res.Sufficient = selTotal >= amount+fee
	return res
}

// simulateGreedySelection replays the greedy input-selection loop from Build
// for a given payment amount and returns the number of inputs it would pick
// and their total.  available must already be sorted largest-first and
// deduplicated by OneTimePub.
func simulateGreedySelection(available []OwnedUTXO, amount, feePerByte uint64) (int, uint64) {
        const maxInputs = 8
        needed := amount + txEstimateFee(1, 2, feePerByte)
        var (
                count   int
                totalIn uint64
        )
        for _, u := range available {
                if totalIn >= needed {
                        break
                }
                count++
                totalIn += u.Amount
                if count >= maxInputs {
                        break
                }
        }
        return count, totalIn
}

// Estimated serialized byte sizes used for fee calculation.
// These mirror the formula in Transaction.Size() (transaction.go).
const (
        // txOverheadBytes: version(1) + fee(8) + feeCommit(32).
        txOverheadBytes = 41
		// txBytesPerInput: input body: keyImage(32) + ring(16×32) + amountCommit(32) = 576
		//                  disclosed real index: 1
		//                  v4 MLSAG: c0(32) + keyImage(32) + 3 response vectors
		//                  (3×16×32) + direct link R/S(64) = 1664.
		//                  Total per input: 576 + 1 + 1664 = 2241.
        // Must stay in sync with Transaction.Size() in transaction.go.
		txBytesPerInput = 2241
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
