package core

import (
        "fmt"
        "time"

        "github.com/aperod/aperod/crypto"
)

// VestingLock holds the genesis vesting state needed to enforce spending
// restrictions on locked genesis allocations at the protocol level.
//
// For each genesis allocation, the OneTimePub of the genesis UTXO equals the
// spendPub decoded from the allocation address (transparent mint pattern).
// BuildVestingLock constructs this map from a GenesisConfig.
type VestingLock struct {
        // allocs maps OneTimePub → GenesisAlloc for every genesis allocation that
        // has a non-immediate vesting schedule.
        allocs map[crypto.Point32]*GenesisAlloc
        // genesisTime is the Unix-second timestamp of the genesis block.
        genesisTime int64
        // nowFn returns the current Unix time in seconds.  Defaults to
        // time.Now().Unix(); overridable in tests.
        nowFn func() int64
}

// BuildVestingLock constructs a VestingLock from the given genesis config and
// the actual genesis block timestamp (Unix seconds).  The caller must pass the
// persisted genesis block's Timestamp field divided by 1e9 (the block stores
// nanoseconds); do NOT pass GenesisConfig.Timestamp which may be zero when the
// node generates the block on first start.
//
// Allocations with placeholder addresses (that fail DecodeAddress) or
// immediate vesting are silently skipped — only non-immediate locked
// allocations are tracked.
func BuildVestingLock(genesis *GenesisConfig, genesisTime int64) (*VestingLock, error) {
        vl := &VestingLock{
                allocs:      make(map[crypto.Point32]*GenesisAlloc),
                genesisTime: genesisTime,
                nowFn:       func() int64 { return time.Now().Unix() },
        }
        for i := range genesis.Allocations {
                alloc := &genesis.Allocations[i]
                if alloc.Address == "" {
                        continue // placeholder
                }
                if alloc.Vesting == nil || alloc.Vesting.Type == VestingImmediate || alloc.Vesting.Type == "" {
                        continue // no locking
                }
                _, spendPub, _, err := crypto.DecodeAddress(crypto.Address(alloc.Address))
                if err != nil {
                        // Placeholder or invalid address — skip rather than fail.
                        continue
                }
                if _, exists := vl.allocs[spendPub]; exists {
                        return nil, fmt.Errorf("BuildVestingLock: duplicate genesis allocation address %q (spendPub %x) — "+
                                "two allocations share the same key, second would silently overwrite the first and could bypass vesting enforcement",
                                alloc.Address, spendPub[:8])
                }
                vl.allocs[spendPub] = alloc
        }
        return vl, nil
}

// LockedAllocsCount returns the number of genesis allocations tracked by this
// vesting lock (i.e. non-immediate schedules).  Used for startup logging.
func (vl *VestingLock) LockedAllocsCount() int {
        return len(vl.allocs)
}

// NewVestingLockForTest constructs a VestingLock with a pre-built allocs map
// and a custom nowFn.  Intended exclusively for unit tests that need direct
// control over the genesis-pub → alloc mapping and the current time.
func NewVestingLockForTest(allocs map[crypto.Point32]*GenesisAlloc, genesisTime int64, nowFn func() int64) *VestingLock {
        return &VestingLock{
                allocs:      allocs,
                genesisTime: genesisTime,
                nowFn:       nowFn,
        }
}

// lockedAt returns the locked amount for a genesis allocation pub key at the
// given Unix-second time.  Returns 0 if the pub key is not a tracked genesis
// allocation or the allocation is fully vested.
func (vl *VestingLock) lockedAt(pub crypto.Point32, now int64) uint64 {
        alloc, ok := vl.allocs[pub]
        if !ok {
                return 0
        }
        return alloc.LockedAmount(now, vl.genesisTime)
}

// TxVerifier validates the cryptographic integrity of transactions.
// Separate from Validate() (structural) — this checks ring sigs and range proofs.
type TxVerifier struct {
        utxos       *UTXOSet
        vestingLock *VestingLock // nil = vesting enforcement disabled
}

// NewTxVerifier creates a verifier backed by the given UTXO set.
func NewTxVerifier(utxos *UTXOSet) *TxVerifier {
        return &TxVerifier{utxos: utxos}
}

// SetVestingLock wires a VestingLock into the verifier so that VerifyTx
// rejects any transaction whose ring members include a still-locked genesis
// UTXO.  Call once after BuildVestingLock at node startup.
func (v *TxVerifier) SetVestingLock(vl *VestingLock) {
        v.vestingLock = vl
}

// VerifyTx performs full cryptographic verification of a non-coinbase transaction.
//
// Checks performed:
//  1. Structural validity (Validate)
//  2. No duplicate key images within the transaction
//  3. No already-spent key images (double-spend)
//  4. MLSAG ring signatures are valid for each input
//  5. Range proofs are valid for each output
//  6. Pedersen commitment balance: ΣC_in = ΣC_out + C_fee
func (v *TxVerifier) VerifyTx(tx *Transaction) error {
        // 1. Structural check
        if err := tx.Validate(); err != nil {
                return err
        }

        // Coinbase (zero-input) transactions must never arrive at the verifier
        // from external sources.  They are synthesized by the consensus engine
        // only and are never routed through the public tx pipeline.  Silently
        // accepting one here would allow inflation without any cryptographic check.
        if len(tx.Inputs) == 0 {
			if tx.IsStake() {
				// Stake payloads are authenticated and applied by the
				// ValidatorRegistry path. Validate() already guarantees that
				// stake transactions have no outputs, so they must not be
				// rejected as coinbase-like zero-input transactions here.
				return nil
			}
                return fmt.Errorf("tx verifier: coinbase (zero-input) transaction rejected — must be engine-synthesized only")
        }

        txHash := tx.Hash()
        txHashPrefix := txHash // copy for slicing

        // 2. No duplicate key images within the transaction
        seen := make(map[crypto.KeyImage]bool, len(tx.Inputs))
        for i, inp := range tx.Inputs {
                if seen[inp.KeyImage] {
                        return fmt.Errorf("tx %x: duplicate key image at input %d", txHashPrefix[:8], i)
                }
                seen[inp.KeyImage] = true
        }

        // 3. No already-spent key images
        if v.utxos != nil {
                for i, inp := range tx.Inputs {
                        if v.utxos.IsSpent(inp.KeyImage) {
                                kiPrefix := inp.KeyImage
                                return fmt.Errorf("tx %x: double-spend at input %d (key image %x already spent)",
                                        txHashPrefix[:8], i, kiPrefix[:8])
                        }
                }
        }

        // 3b. Vesting lock — reject any transaction that includes a still-locked
        // genesis UTXO as a ring member.
        //
        // ORDERING: This check intentionally runs BEFORE the C-0 ring-member
        // presence check (3c below).  The VestingLock is built from the genesis
        // config (address-decode path) and requires only the in-memory allocs
        // map — it does NOT consult byPubKey.  Running vesting first means a
        // locked-genesis spend is always rejected with the precise "locked genesis"
        // error regardless of whether the genesis block's TxData was pruned and
        // its UTXOs are therefore absent from byPubKey.
        //
        // If vesting ran after C-0 and genesis was pruned (UTXOs absent from
        // byPubKey), the C-0 check would fire first with a misleading
        // "not found in UTXO set" error rather than the intended vesting error.
        //
        // Genesis outputs use OneTimePub = spendPub directly (transparent mint),
        // so each ring member can be cross-checked against the genesis allocation
        // map without knowing the private key.  Because the real spender is hidden
        // among ring members we cannot distinguish it from decoys; therefore we
        // conservatively reject the entire transaction if any ring member is a
        // locked genesis UTXO.  This closes the protocol gap where vesting was
        // display-only: a compromised key holder can no longer spend locked tokens
        // even if they construct a valid ring signature.
        if v.vestingLock != nil {
                now := v.vestingLock.nowFn()
                for i, inp := range tx.Inputs {
                        for _, member := range inp.Ring {
                                locked := v.vestingLock.lockedAt(member, now)
                                if locked > 0 {
                                        return fmt.Errorf("tx %x: input %d ring member %x is a "+
                                                "locked genesis allocation (%d nAPRO still locked) — "+
                                                "spending locked genesis tokens is not permitted",
                                                txHashPrefix[:8], i, member[:8], locked)
                                }
                        }
                }
        }

        // 3c. C-0: ring-member commitment binding.
        //
        // At least one ring member that is present in byPubKey (active unspent UTXO)
        // must have AmountCommit == inp.AmountCommit.  The real spender's UTXO is
        // always unspent and therefore always in byPubKey; its commitment must
        // match the claimed inp.AmountCommit.
        //
        // Absent members (Phase 1 random keys or Phase 2 spent decoys removed from
        // byPubKey by ApplyBlock) are silently skipped.  Present members whose
        // commitments do NOT match are tolerated — they are real UTXOs used as
        // active decoys with different amounts.  The security property is preserved:
        //   • The real spender is always present (C-0a guards against all-absent rings).
        //   • The real spender's commitment MUST equal inp.AmountCommit (matchCount ≥ 1).
        //   • The MLSAG ring signature proves knowledge of one private key in the ring.
        //   • The Pedersen balance check (step 6) ensures ΣC_in = ΣC_out + C_fee.
        //
        // The previous "all must match" rule was too strict: it blocked Phase 2
        // transactions whenever any active decoy had a different amount, causing
        // a "commitment mismatch" error even for honest spends.
        //
        // NOTE ON PRUNED STARTS: genesis UTXOs may be absent from byPubKey
        // in light-pruning mode.  The vesting check (3b above) is ordered
        // first so that locked-genesis errors surface before C-0 fires.
        if v.utxos != nil {
                for i, inp := range tx.Inputs {
			if tx.Version == TxVersionCommitmentBinding {
				realMember := inp.Ring[int(inp.RealIndex)]
				utxo := v.utxos.GetByPubKey(realMember)
				if utxo == nil {
					return fmt.Errorf("tx %x: input %d proven real member %x is not an active UTXO",
						txHashPrefix[:8], i, realMember[:8])
				}
				if utxo.AmountCommit != inp.AmountCommit {
					return fmt.Errorf("tx %x: input %d proven real member commitment does not match claimed commitment",
						txHashPrefix[:8], i)
				}
				continue
			}
                        presentCount := 0
                        matchCount := 0
                        for _, member := range inp.Ring {
                                utxo := v.utxos.GetByPubKey(member)
                                if utxo == nil {
                                        // Absent: Phase 1 random key or Phase 2 spent decoy — skip.
                                        continue
                                }
                                presentCount++
                                if utxo.AmountCommit == inp.AmountCommit {
                                        matchCount++
                                }
                                // Non-matching present members are active decoys with different
                                // amounts — tolerated under the "at least one matches" rule.
                        }

                        // C-0a: fabricated-input guard.
                        //
                        // In a legitimate transaction the real spender's UTXO is
                        // unspent, so it is always present in byPubKey regardless of
                        // pruning mode (pruning strips old block data, not the live
                        // UTXO set).  Decoys may be absent (Phase 1 random keys or
                        // Phase 2 spent UTXOs), but the real one must be there.
                        //
                        // An attacker can sign with a freshly-generated key pair that
                        // never appeared on-chain.  All 16 ring members are absent →
                        // inp.AmountCommit is unconstrained → attacker could inflate.
                        //
                        // Fix: reject any input whose entire ring is absent from byPubKey.
                        if presentCount == 0 {
                                return fmt.Errorf("tx %x: input %d all %d ring members are absent "+
                                        "from the UTXO set — fabricated-input inflation attack blocked (C-0a check)",
                                        txHashPrefix[:8], i, len(inp.Ring))
                        }

                        // C-0: at least one present ring member must match inp.AmountCommit.
                        // The real (unspent) spender is always in byPubKey; its commitment
                        // must equal the claimed inp.AmountCommit or the spend is fraudulent.
                        if matchCount == 0 {
                                return fmt.Errorf("tx %x: input %d no ring member found in UTXO set "+
                                        "has AmountCommit matching claimed %x — "+
                                        "forged commitment or wrong UTXO (C-0 check)",
                                        txHashPrefix[:8], i, inp.AmountCommit[:8])
                        }
                }
        }

        // 4. MLSAG ring signatures
        for i, inp := range tx.Inputs {
                sig := tx.Signatures[i]
                if sig == nil {
                        return fmt.Errorf("tx %x: nil signature at input %d", txHashPrefix[:8], i)
                }
                // The signed message is H(txHash || inputIndex)
                msg := ringSignMessage(txHash, uint32(i))
		var ok bool
		var err error
		if tx.Version == TxVersionCommitmentBinding {
			ok, err = crypto.MLSAGVerifyV4(msg, inp.Ring, inp.AmountCommit, int(inp.RealIndex), sig)
		} else {
			ok, err = crypto.MLSAGVerify(msg, inp.Ring, sig)
		}
                if err != nil {
                        return fmt.Errorf("tx %x: ring sig error at input %d: %w", txHashPrefix[:8], i, err)
                }
                if !ok {
                        return fmt.Errorf("tx %x: invalid ring signature at input %d", txHashPrefix[:8], i)
                }
                // Verify that the key image in the signature matches the input's key image
                if sig.KeyImage != inp.KeyImage {
                        return fmt.Errorf("tx %x: key image mismatch at input %d", txHashPrefix[:8], i)
                }
        }

        // 5. Range proofs for all outputs
        for i, proof := range tx.RangeProofs {
                ok, err := crypto.VerifyRange(proof)
                if err != nil {
                        return fmt.Errorf("tx %x: range proof error at output %d: %w", txHashPrefix[:8], i, err)
                }
                if !ok {
                        return fmt.Errorf("tx %x: invalid range proof at output %d", txHashPrefix[:8], i)
                }
                // Verify that the proof covers the correct commitment
                if proof.ValueCommit != tx.Outputs[i].AmountCommit {
                        return fmt.Errorf("tx %x: range proof commitment mismatch at output %d", txHashPrefix[:8], i)
                }
        }

        // 5b. Fee commitment binding.
        //
        // tx.Fee is the public plaintext fee in nAPRO.  A well-formed transaction
        // must commit to it with a zero blinding factor: C_fee = Commit(fee, 0).
        // Without this check an attacker can supply an arbitrary C_fee — notably a
        // commitment to a *negative* value — and the balance equation
        // ΣC_in = ΣC_out + C_fee still holds, because Pedersen commitments are
        // additively homomorphic and have no range restriction on C_fee itself.
        //
        // Example attack (reported, August 2026):
        //   in = 1 APRO, out1 = out2 = 1000 APRO, C_fee = C_in − C_out1 − C_out2
        //   → C_fee commits to −1999 APRO.  Balance: ✓ Range proofs on outputs: ✓
        //   Net effect: 1999 APRO minted from nothing.
        //
        // Fix: recompute the expected commitment from the public fee value and
        // require an exact match.  Zero-blind commitment = value * H in Pedersen,
        // which anyone can verify without knowing any secret.
        {
                var zeroFeeBlind crypto.BlindFactor
                expectedFeeCommit, feeErr := crypto.Commit(tx.Fee, zeroFeeBlind)
                if feeErr != nil {
                        return fmt.Errorf("tx %x: fee commitment derivation: %w", txHashPrefix[:8], feeErr)
                }
                if tx.FeeCommit != expectedFeeCommit {
                        return fmt.Errorf("tx %x: fee commitment mismatch — C_fee ≠ Commit(fee, 0); negative-fee inflation attack rejected",
                                txHashPrefix[:8])
                }
        }

        // 6. Commitment balance: ΣC_in = ΣC_out + C_fee
        inCommits := make([]crypto.Commitment, len(tx.Inputs))
        for i, inp := range tx.Inputs {
                inCommits[i] = inp.AmountCommit
        }
        outCommits := make([]crypto.Commitment, len(tx.Outputs))
        for i, out := range tx.Outputs {
                outCommits[i] = out.AmountCommit
        }
        balanced, err := crypto.CommitSum(inCommits, outCommits, tx.FeeCommit)
        if err != nil {
                return fmt.Errorf("tx %x: commitment sum error: %w", txHashPrefix[:8], err)
        }
        if !balanced {
                return fmt.Errorf("tx %x: commitment balance check failed (inputs ≠ outputs + fee)", txHashPrefix[:8])
        }

        return nil
}

// VerifyBlock verifies all transactions in a block.
// Applies inputs sequentially (no parallel to maintain UTXO consistency).
//
// Coinbase (zero-input) transactions are skipped: they carry no ring signatures
// or range proofs, so there is nothing to verify cryptographically.  Structural
// and policy checks for coinbase (≤1 per block, at index 0, amount ≤ scheduled
// reward) are enforced by validateCoinbasePolicy in the consensus engine before
// this function is called.
func (v *TxVerifier) VerifyBlock(block *Block) error {
        for i, tx := range block.Txs {
                if tx.IsCoinbase() {
                        continue // no crypto proofs on coinbase; policy checked separately
                }
                if err := v.VerifyTx(&tx); err != nil {
                        h := tx.Hash()
                        return fmt.Errorf("block %d tx[%d] %x: %w",
                                block.Header.Height, i, h[:8], err)
                }
        }
        return nil
}

// ringSignMessage computes the message hash for an MLSAG ring signature.
// msg = SHA3(txHash || inputIndex) — binds the signature to a specific input.
func ringSignMessage(txHash crypto.Hash32, inputIdx uint32) crypto.Hash32 {
        idx := []byte{byte(inputIdx >> 24), byte(inputIdx >> 16), byte(inputIdx >> 8), byte(inputIdx)}
        return crypto.HashBytes([]byte("aperod/ring-sign/v1"), txHash[:], idx)
}

// RingSignMessage is exported for use in tx building.
func RingSignMessage(txHash crypto.Hash32, inputIdx uint32) crypto.Hash32 {
        return ringSignMessage(txHash, inputIdx)
}
