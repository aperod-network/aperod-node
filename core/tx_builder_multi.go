package core

// Multi-output transaction builder — extends TxBuilder with batch payment support.
// One blockchain transaction with N payment outputs + optional change.
// More efficient than N separate transactions: one fee, one mempool entry.

import (
	"fmt"
	"sort"

	"github.com/aperod/aperod/crypto"
)

// BatchRecipient is a single payment target for BuildMulti.
type BatchRecipient struct {
	Address    crypto.Address
	AmountNAPR uint64
}

// BatchBuildResult holds the signed multi-output transaction and its metadata.
type BatchBuildResult struct {
	Tx           Transaction
	TotalFee     uint64
	ChangeAmount uint64
	ChangeBlind  crypto.BlindFactor
	ChangeOutIdx int
	// Parallel arrays — one element per recipient, in input order.
	PayBlinds  []crypto.BlindFactor
	PayOutIdxs []int
	PayAmounts []uint64
	// Decoy diagnostics.
	RealDecoyCount     int
	FallbackDecoyCount int
}

// BuildMulti constructs a signed RingCT transaction that pays N recipients in a
// single on-chain transaction.  This is the foundation for the game batch-send
// API: one server call distributes rewards to N wallets atomically and cheaply.
//
// recipients — non-empty slice of (address, amountNAPR) pairs; max 15.
// changeAddr — sender's own address for the change output (required).
//
// Pedersen blind balancing:
//   - If totalIn > totalAmount+fee (hasChange): all pay outputs get deterministic
//     blinds via txBuildOutput; the change output uses BlindSum to balance.
//   - Otherwise: first N-1 pay outputs get deterministic blinds; the last pay
//     output uses BlindSum to balance (no change output produced).
func (b *TxBuilder) BuildMulti(recipients []BatchRecipient, changeAddr crypto.Address) (*BatchBuildResult, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("at least one recipient required")
	}
	if len(recipients) > 15 {
		return nil, fmt.Errorf("recipients exceeds maximum of 15 per batch (tx output limit)")
	}

	var totalAmount uint64
	for i, r := range recipients {
		if r.AmountNAPR == 0 {
			return nil, fmt.Errorf("recipients[%d]: amount must be > 0", i)
		}
		if err := crypto.Validate(r.Address); err != nil {
			return nil, fmt.Errorf("recipients[%d]: invalid address: %w", i, err)
		}
		totalAmount += r.AmountNAPR
	}
	if err := crypto.Validate(changeAddr); err != nil {
		return nil, fmt.Errorf("invalid change address: %w", err)
	}

	// Sort available UTXOs largest-first for greedy selection.
	available := make([]OwnedUTXO, len(b.ownedUTXOs))
	copy(available, b.ownedUTXOs)
	sort.Slice(available, func(i, j int) bool { return available[i].Amount > available[j].Amount })

	// Deduplicate by OneTimePub: outputs sharing a one-time public key share
	// the same key image (height-0 admin mints to one address); spending two
	// of them in one tx produces duplicate key images and is rejected by
	// consensus.  Keep the largest (first occurrence, list is largest-first).
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

	// Fee estimation: nOut = N recipients + 1 change slot.
	const maxInputs = 8
	nOut := len(recipients) + 1
	estimatedFee := txEstimateFee(1, nOut, b.feePerByte)
	needed := totalAmount + estimatedFee

	var selected []OwnedUTXO
	var totalIn uint64
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
	// Recalculate with actual input count.
	estimatedFee = txEstimateFee(len(selected), nOut, b.feePerByte)
	needed = totalAmount + estimatedFee

	if totalIn < needed {
		return nil, fmt.Errorf("insufficient funds: have %d nAPRO, need %d (amount %d + fee %d)",
			totalIn, needed, totalAmount, estimatedFee)
	}

	changeAmount := totalIn - totalAmount - estimatedFee
	hasChange := changeAmount > 0

	// Fee commitment — zero blind (fee is public).
	var feeBlind crypto.BlindFactor
	feeCommit, err := crypto.Commit(estimatedFee, feeBlind)
	if err != nil {
		return nil, fmt.Errorf("fee commit: %w", err)
	}

	// Collect input blinds for BlindSum.
	inBlinds := make([]crypto.BlindFactor, len(selected))
	for i, u := range selected {
		inBlinds[i] = u.Blind
	}

	type outEntry struct {
		output Output
		blind  crypto.BlindFactor
		amount uint64
	}

	var outEntries []outEntry
	payBlinds := make([]crypto.BlindFactor, len(recipients))
	payOutIdxs := make([]int, len(recipients))
	payAmounts := make([]uint64, len(recipients))
	changeOutIdx := -1
	var changeBlindResult crypto.BlindFactor

	// outBlindsSoFar accumulates blinds already committed to (fee + each pay output
	// as it's built).  The final BlindSum call uses this list to derive the
	// balancing blind so that ΣC_in == ΣC_out + C_fee.
	outBlindsSoFar := []crypto.BlindFactor{feeBlind}

	if hasChange {
		// All pay outputs get deterministic blinds; change output balances.
		for i, r := range recipients {
			payOut, payBlind, err := txBuildOutput(r.Address, r.AmountNAPR)
			if err != nil {
				return nil, fmt.Errorf("build payment output[%d]: %w", i, err)
			}
			outEntries = append(outEntries, outEntry{payOut, payBlind, r.AmountNAPR})
			payBlinds[i] = payBlind
			payOutIdxs[i] = i
			payAmounts[i] = r.AmountNAPR
			outBlindsSoFar = append(outBlindsSoFar, payBlind)
		}
		changeBlind, err := crypto.BlindSum(inBlinds, outBlindsSoFar)
		if err != nil {
			return nil, fmt.Errorf("change blind sum: %w", err)
		}
		chOut, err := txBuildOutputWithBlind(changeAddr, changeAmount, changeBlind)
		if err != nil {
			return nil, fmt.Errorf("build change output: %w", err)
		}
		outEntries = append(outEntries, outEntry{chOut, changeBlind, changeAmount})
		changeBlindResult = changeBlind
		changeOutIdx = len(recipients)
	} else {
		// No change: first N-1 pay outputs get deterministic blinds; last balances.
		for i := 0; i < len(recipients)-1; i++ {
			r := recipients[i]
			payOut, payBlind, err := txBuildOutput(r.Address, r.AmountNAPR)
			if err != nil {
				return nil, fmt.Errorf("build payment output[%d]: %w", i, err)
			}
			outEntries = append(outEntries, outEntry{payOut, payBlind, r.AmountNAPR})
			payBlinds[i] = payBlind
			payOutIdxs[i] = i
			payAmounts[i] = r.AmountNAPR
			outBlindsSoFar = append(outBlindsSoFar, payBlind)
		}
		lastIdx := len(recipients) - 1
		lr := recipients[lastIdx]
		lastBlind, err := crypto.BlindSum(inBlinds, outBlindsSoFar)
		if err != nil {
			return nil, fmt.Errorf("last pay blind sum: %w", err)
		}
		lastOut, err := txBuildOutputWithBlind(lr.Address, lr.AmountNAPR, lastBlind)
		if err != nil {
			return nil, fmt.Errorf("build last payment output: %w", err)
		}
		outEntries = append(outEntries, outEntry{lastOut, lastBlind, lr.AmountNAPR})
		payBlinds[lastIdx] = lastBlind
		payOutIdxs[lastIdx] = lastIdx
		payAmounts[lastIdx] = lr.AmountNAPR
	}

	// ── Sample ring decoys (Phase 2) ──────────────────────────────────────────
	var allDecoys []DecoyUTXO
	if b.utxoSet != nil {
		excludePubs := make(map[crypto.Point32]bool, len(selected))
		for _, u := range selected {
			excludePubs[u.OneTimePub] = true
		}
		need := len(selected) * (crypto.RingSize - 1)
		allDecoys = b.utxoSet.SampleDecoys(need, excludePubs)
	}

	// ── Build ring inputs ─────────────────────────────────────────────────────
	inputs := make([]RingInput, len(selected))
	inputPrivKeys := make([]crypto.Scalar32, len(selected))
	inputRealIdxs := make([]int, len(selected))
	var totalFallback, totalReal int

	for i, u := range selected {
		oneTimePriv, err := crypto.AddScalars(u.HsScalar, b.spendPriv)
		if err != nil {
			return nil, fmt.Errorf("derive one-time priv[%d]: %w", i, err)
		}
		inputPrivKeys[i] = oneTimePriv

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
			return nil, fmt.Errorf("build ring[%d]: %w", i, err)
		}
		inputRealIdxs[i] = realIdx
		totalFallback += fallbacks
		totalReal += (crypto.RingSize - 1) - fallbacks

		ki, err := crypto.ComputeKeyImage(oneTimePriv, u.OneTimePub)
		if err != nil {
			return nil, fmt.Errorf("compute key image[%d]: %w", i, err)
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
			return nil, fmt.Errorf("range proof[%d]: %w", i, err)
		}
		rangeProofs[i] = proof
	}

	// ── Assemble and MLSAG-sign ───────────────────────────────────────────────
	tx := Transaction{
		Version:     TxVersionBase,
		Inputs:      inputs,
		Outputs:     outputs,
		Fee:         estimatedFee,
		FeeCommit:   feeCommit,
		RangeProofs: rangeProofs,
		Signatures:  make([]*crypto.MLSAGSignature, len(inputs)),
	}
	txHashVal := tx.Hash()
	for i := range inputs {
		msg := ringSignMessage(txHashVal, uint32(i))
		sig, err := crypto.MLSAGSign(msg, inputs[i].Ring, inputRealIdxs[i], inputPrivKeys[i])
		if err != nil {
			return nil, fmt.Errorf("mlsag sign[%d]: %w", i, err)
		}
		tx.Signatures[i] = sig
		tx.Inputs[i].KeyImage = sig.KeyImage
	}

	return &BatchBuildResult{
		Tx:                 tx,
		TotalFee:           estimatedFee,
		ChangeAmount:       changeAmount,
		ChangeBlind:        changeBlindResult,
		ChangeOutIdx:       changeOutIdx,
		PayBlinds:          payBlinds,
		PayOutIdxs:         payOutIdxs,
		PayAmounts:         payAmounts,
		RealDecoyCount:     totalReal,
		FallbackDecoyCount: totalFallback,
	}, nil
}
