package core_test

// tx_verifier_test.go — tests for TxVerifier C-0 commitment binding,
// double-spend key-image rejection, and negative-fee inflation attack (F-041).

import (
	"strings"
	"testing"

	"filippo.io/edwards25519"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeUTXO creates a minimal UTXO for testing.
func makeUTXO(txHash crypto.Hash32, outIdx uint32, pub crypto.Point32, commit crypto.Commitment) *core.UTXO {
	return &core.UTXO{
		TxHash:       txHash,
		OutputIndex:  outIdx,
		OneTimePub:   pub,
		AmountCommit: commit,
	}
}

// makeTxWithRingAndCommit builds a minimal tx whose single input uses pub as
// the only ring member and sets AmountCommit to commit.
func makeTxWithRingAndCommit(pub crypto.Point32, commit crypto.Commitment, ki crypto.KeyImage) core.Transaction {
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = pub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i][0] = byte(i + 100)
	}
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				KeyImage:     ki,
				Ring:         ring,
				AmountCommit: commit,
			},
		},
		Outputs: []core.Output{
			{
				OneTimePub:   crypto.Point32{0xFE},
				TxPubKey:     crypto.Point32{},
				AmountCommit: crypto.Commitment{},
			},
		},
		Fee:         500,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}
}

// populateFullRingUTXOs adds all RingSize ring members to the UTXO set with
// the given commit, so the C-0 check can pass for every ring member.
// Returns the ring (same slice used in makeTxWithRingAndCommit).
func populateFullRingUTXOs(utxos *core.UTXOSet, realPub crypto.Point32, commit crypto.Commitment) []crypto.Point32 {
	ring := make([]crypto.Point32, crypto.RingSize)
	ring[0] = realPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i][0] = byte(i + 100)
	}
	for idx, pub := range ring {
		utxos.Add(makeUTXO(crypto.Hash32{byte(idx + 1)}, 0, pub, commit))
	}
	return ring
}

// TestTxVerifier_C0_FabricatedRingCommitment (#486) — TxVerifier must reject a
// tx where the ring member's AmountCommit does not match the on-chain UTXO commit.
//
// Security: the real (unspent) spending key is in byPubKey.  C-0 checks every
// ring member found in byPubKey against inp.AmountCommit.  If the signer forges
// inp.AmountCommit to claim a larger amount, the on-chain UTXO's commitment
// won't match and C-0 rejects the transaction before MLSAG is ever checked.
//
// Phase 2 note: spent decoys are absent from byPubKey (moved to spentPubKeys by
// ApplyBlock) and C-0 skips them.  Only the real unspent ring member triggers
// the commitment check.
func TestTxVerifier_C0_FabricatedRingCommitment(t *testing.T) {
	commitOnChain := crypto.Commitment{0xAA}
	fabricatedCommit := crypto.Commitment{0xBB}
	realPub := crypto.Point32{0x01, 0x02, 0x03}

	// Populate UTXOSet: all ring members have commitOnChain.
	utxos := core.NewUTXOSet()
	populateFullRingUTXOs(utxos, realPub, commitOnChain)

	v := core.NewTxVerifier(utxos)

	// Bad tx: correct ring member pub, but FABRICATED AmountCommit in the input.
	ki1 := makeKeyImage(201)
	txBad := makeTxWithRingAndCommit(realPub, fabricatedCommit, ki1)

	err := v.VerifyTx(&txBad)
	if err == nil {
		t.Fatal("TxVerifier should reject a tx with a fabricated AmountCommit, got nil error")
	}
	if !strings.Contains(err.Error(), "C-0") {
		t.Errorf("error message should cite the C-0 check, got: %v", err)
	}

	// Positive case: same ring with the CORRECT on-chain commit.
	// The C-0 check passes; MLSAG will still fail (dummy sig), but the error
	// must NOT cite "C-0".
	ki2 := makeKeyImage(202)
	txOK := makeTxWithRingAndCommit(realPub, commitOnChain, ki2)
	errOK := v.VerifyTx(&txOK)
	if errOK != nil && strings.Contains(errOK.Error(), "C-0") {
		t.Errorf("correct AmountCommit should pass the C-0 check, but got C-0 error: %v", errOK)
	}
}

// TestTxVerifier_DoubleSpendKeyImage_RejectedByVerifier (#487 partial) —
// TxVerifier must reject a tx that reuses a key image already marked spent.
func TestTxVerifier_DoubleSpendKeyImage_RejectedByVerifier(t *testing.T) {
	ki := makeKeyImage(301)

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(ki)

	v := core.NewTxVerifier(utxos)
	tx := makeTx(1000, 301)

	err := v.VerifyTx(&tx)
	if err == nil {
		t.Fatal("TxVerifier should reject a tx with an already-spent key image, got nil")
	}
	if !strings.Contains(err.Error(), "double-spend") && !strings.Contains(err.Error(), "already spent") {
		t.Errorf("error should mention double-spend, got: %v", err)
	}
}

// TestTxVerifier_NegativeFeeInflationAttack_Rejected (F-041 regression) —
// VerifyTx must reject a transaction where FeeCommit = ΣC_in − ΣC_out
// (a commitment to a negative value) while the declared tx.Fee is a small
// positive value.
//
// Without check 5b an attacker can produce outputs far exceeding inputs by
// constructing FeeCommit so that the homomorphic balance equation
// ΣC_in = ΣC_out + C_fee holds while the actual output amounts are inflated.
// Fix: VerifyTx recomputes Commit(fee, 0) and requires an exact match.
//
// The test constructs a fully cryptographically valid transaction (real MLSAG
// signature and Bulletproof range proof) so that only the 5b fee-commitment
// check — not an earlier structural or MLSAG check — is responsible for the
// rejection in the attack case.
func TestTxVerifier_NegativeFeeInflationAttack_Rejected(t *testing.T) {
	// ─── Attack scenario ─────────────────────────────────────────────────────
	// Input:  1,000 nAPRO  (v_in)
	// Output: 999,000 nAPRO (v_out — inflated 999×)
	// Declared fee: 1 nAPRO
	// FeeCommit: C_in − C_out  (commits to −998,001 nAPRO — a negative value)
	//
	// The balance equation ΣC_in = ΣC_out + C_fee holds by construction:
	//   C_in = C_out + (C_in − C_out)  ✓
	// But C_fee ≠ Commit(fee, 0) — check 5b must catch this.
	const (
		inAmount  uint64 = 1_000
		outAmount uint64 = 999_000
		feeAmount uint64 = 1
	)

	// ── Generate a real one-time key pair for the spender ────────────────────
	kp, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	ki, err := crypto.ComputeKeyImage(kp.Spend.Private, kp.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage: %v", err)
	}

	// ── Build commitments ────────────────────────────────────────────────────
	rIn, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor rIn: %v", err)
	}
	commitIn, err := crypto.Commit(inAmount, rIn)
	if err != nil {
		t.Fatalf("Commit in: %v", err)
	}

	rOut, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor rOut: %v", err)
	}
	commitOut, err := crypto.Commit(outAmount, rOut)
	if err != nil {
		t.Fatalf("Commit out: %v", err)
	}

	// Attacker computes FeeCommit = C_in − C_out via Pedersen point arithmetic.
	// By homomorphism: C_in − C_out = Commit(v_in − v_out, r_in − r_out)
	//                               = Commit(−998000, r_in − r_out)  (negative value).
	// ΣC_in = ΣC_out + FeeCommit holds; but FeeCommit ≠ Commit(1, 0).
	ptIn, err := crypto.PointFromBytes(commitIn[:])
	if err != nil {
		t.Fatalf("PointFromBytes commitIn: %v", err)
	}
	ptOut, err := crypto.PointFromBytes(commitOut[:])
	if err != nil {
		t.Fatalf("PointFromBytes commitOut: %v", err)
	}
	ptFeeAttack := new(edwards25519.Point).Add(ptIn, new(edwards25519.Point).Negate(ptOut))
	var attackFeeCommit crypto.Commitment
	copy(attackFeeCommit[:], ptFeeAttack.Bytes())

	// ── Build ring: real key at position 0, dummy valid keys for decoys ───────
	// Decoys are not added to the UTXO set, so C-0 skips them (absent = Phase 1
	// random key).  The real key IS in the UTXO set, satisfying C-0a.
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = kp.Spend.Public
	for i := 1; i < crypto.RingSize; i++ {
		decoy, e := crypto.GenerateWalletKeys()
		if e != nil {
			t.Fatalf("GenerateWalletKeys decoy %d: %v", i, e)
		}
		ring[i] = decoy.Spend.Public
	}

	utxos := core.NewUTXOSet()
	utxos.Add(makeUTXO(crypto.Hash32{0xA1}, 0, kp.Spend.Public, commitIn))

	// ── Assemble the unsigned attack transaction ──────────────────────────────
	// Hash() does not include Signatures or RangeProofs, so we can compute the
	// hash with the full tx shape (including the attack FeeCommit and KeyImage)
	// before creating the signature.
	txAttack := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{{
			KeyImage:     ki,
			Ring:         ring,
			AmountCommit: commitIn,
		}},
		Outputs: []core.Output{{
			OneTimePub:   crypto.Point32{0xDE, 0xAD},
			AmountCommit: commitOut,
		}},
		Fee:         feeAmount,
		FeeCommit:   attackFeeCommit,
		Signatures:  make([]*crypto.MLSAGSignature, 1),
		RangeProofs: make([]*crypto.RangeProof, 1),
	}

	// ── Sign the attack transaction with the real spending key ────────────────
	txHash := txAttack.Hash()
	msg := core.RingSignMessage(txHash, 0)
	sig, err := crypto.MLSAGSign(msg, ring, 0, kp.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign attack: %v", err)
	}
	txAttack.Signatures[0] = sig
	txAttack.Inputs[0].KeyImage = sig.KeyImage

	// ── Range proof for the inflated output ──────────────────────────────────
	proof, err := crypto.ProveRange(outAmount, rOut)
	if err != nil {
		t.Fatalf("ProveRange attack: %v", err)
	}
	txAttack.RangeProofs[0] = proof

	// ── Verify: must be rejected with a fee-commitment error ─────────────────
	v := core.NewTxVerifier(utxos)
	errAttack := v.VerifyTx(&txAttack)
	if errAttack == nil {
		t.Fatal("VerifyTx must reject the negative-fee inflation attack, got nil error")
	}
	if !strings.Contains(errAttack.Error(), "fee commitment") &&
		!strings.Contains(errAttack.Error(), "negative-fee") {
		t.Errorf("expected fee-commitment mismatch error, got: %v", errAttack)
	}
	t.Logf("inflation attack correctly rejected: %v", errAttack)

	// ─── Positive case: valid FeeCommit = Commit(fee, 0) ─────────────────────
	// Construct a well-formed transaction where commitments balance exactly:
	//   v_in = v_out + fee  →  1001 = 1000 + 1
	//   r_fee = 0  →  r_out = r_in − r_fee = r_in  (balance blind constraint)
	// This satisfies both 5b (C_fee = Commit(fee, 0)) and check 6 (ΣC_in = ΣC_out + C_fee).
	const (
		inAmountOK  uint64 = 1_001
		outAmountOK uint64 = 1_000
		feeOK       uint64 = 1
	)

	kpOK, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys OK: %v", err)
	}
	kiOK, err := crypto.ComputeKeyImage(kpOK.Spend.Private, kpOK.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage OK: %v", err)
	}

	rInOK, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("NewBlindFactor rInOK: %v", err)
	}
	commitInOK, err := crypto.Commit(inAmountOK, rInOK)
	if err != nil {
		t.Fatalf("Commit inOK: %v", err)
	}
	// r_fee = 0, so r_out must equal r_in for the blind balance to hold.
	commitOutOK, err := crypto.Commit(outAmountOK, rInOK)
	if err != nil {
		t.Fatalf("Commit outOK: %v", err)
	}
	var zeroBlind crypto.BlindFactor
	commitFeeOK, err := crypto.Commit(feeOK, zeroBlind)
	if err != nil {
		t.Fatalf("Commit feeOK: %v", err)
	}

	ringOK := make([]crypto.RingMember, crypto.RingSize)
	ringOK[0] = kpOK.Spend.Public
	for i := 1; i < crypto.RingSize; i++ {
		decoy, e := crypto.GenerateWalletKeys()
		if e != nil {
			t.Fatalf("GenerateWalletKeys decoy OK %d: %v", i, e)
		}
		ringOK[i] = decoy.Spend.Public
	}

	utxosOK := core.NewUTXOSet()
	utxosOK.Add(makeUTXO(crypto.Hash32{0xB1}, 0, kpOK.Spend.Public, commitInOK))

	txOK := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{{
			KeyImage:     kiOK,
			Ring:         ringOK,
			AmountCommit: commitInOK,
		}},
		Outputs: []core.Output{{
			OneTimePub:   crypto.Point32{0x44, 0x55},
			AmountCommit: commitOutOK,
		}},
		Fee:         feeOK,
		FeeCommit:   commitFeeOK,
		Signatures:  make([]*crypto.MLSAGSignature, 1),
		RangeProofs: make([]*crypto.RangeProof, 1),
	}

	txHashOK := txOK.Hash()
	msgOK := core.RingSignMessage(txHashOK, 0)
	sigOK, err := crypto.MLSAGSign(msgOK, ringOK, 0, kpOK.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign OK: %v", err)
	}
	txOK.Signatures[0] = sigOK
	txOK.Inputs[0].KeyImage = sigOK.KeyImage

	proofOK, err := crypto.ProveRange(outAmountOK, rInOK)
	if err != nil {
		t.Fatalf("ProveRange OK: %v", err)
	}
	txOK.RangeProofs[0] = proofOK

	vOK := core.NewTxVerifier(utxosOK)
	errOK := vOK.VerifyTx(&txOK)
	if errOK != nil {
		t.Errorf("valid tx with Commit(fee, 0) must pass VerifyTx, got: %v", errOK)
	}
	t.Logf("valid tx with Commit(fee, 0) correctly accepted")
}

// TestMempool_DoubleSpendKeyImage_Rejected (#487) — pool.Add() must return an
// error when the verifier has the key image marked spent.
func TestMempool_DoubleSpendKeyImage_Rejected(t *testing.T) {
	ki := makeKeyImage(401)

	utxos := core.NewUTXOSet()
	utxos.MarkSpent(ki)

	v := core.NewTxVerifier(utxos)

	cfg := core.MempoolConfig{
		MaxSize:        10,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 0, // disable fee check so only the verifier decides
	}
	pool := core.NewMempool(cfg, silentLogger())
	pool.SetVerifier(v)

	tx := makeTx(1000, 401) // uses makeKeyImage(401) == ki
	err := pool.Add(tx)
	if err == nil {
		t.Fatal("pool.Add() should reject a tx with an already-spent key image, got nil")
	}

	// Verify no entry was added to the pool.
	if len(pool.Hashes()) != 0 {
		t.Errorf("pool should be empty after rejection, got %d entries", len(pool.Hashes()))
	}
}
