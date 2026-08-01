package core_test

// vesting_enforcement_test.go — protocol-level enforcement of genesis vesting
// locks inside TxVerifier.  Verifies that spending a still-locked genesis UTXO
// is rejected at mempool entry (and that unlocked / non-genesis UTXOs pass).

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// buildTestVestingLock creates a VestingLock directly for testing,
// bypassing the address-decode path by using a synthetic spendPub.
// It injects a team allocation (cliff_linear, 1-year cliff, 4-year vest)
// mapped to the provided pub key and overrides nowFn so tests control time.
func buildTestVestingLock(teamPub crypto.Point32, genesisTime int64, nowFn func() int64) *core.VestingLock {
	const (
		cliffSeconds = int64(365 * 86400)       // 1 year
		vestSeconds  = int64(4 * 365 * 86400)   // 4 years after cliff
	)
	teamAlloc := &core.GenesisAlloc{
		Address: "", // not used directly in VestingLock
		Amount:  1_000_000_000 * core.BaseUnitsPerAPR, // 1 000 000 000 APRO
		Label:   "Team",
		Vesting: &core.VestingSchedule{
			Type:         core.VestingCliffLinear,
			CliffSeconds: cliffSeconds,
			VestSeconds:  vestSeconds,
		},
	}
	return core.NewVestingLockForTest(map[crypto.Point32]*core.GenesisAlloc{
		teamPub: teamAlloc,
	}, genesisTime, nowFn)
}

// TestVestingEnforcement_LockedGenesisRejected verifies that a transaction
// whose ring includes a still-locked genesis UTXO is rejected by VerifyTx.
func TestVestingEnforcement_LockedGenesisRejected(t *testing.T) {
	const genesisTime = int64(1_700_000_000)
	// "now" is one day after genesis — well within the 1-year cliff.
	now := genesisTime + 86400

	teamPub := crypto.Point32{0xAA, 0xBB, 0xCC}
	commit := crypto.Commitment{0x11}

	// Build UTXO set with the genesis "team" UTXO at height 0.
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:       crypto.Hash32{0x01},
		OutputIndex:  0,
		OneTimePub:   teamPub,
		AmountCommit: commit,
		BlockHeight:  0,
	})
	// Add remaining decoy ring members.
	for i := 1; i < crypto.RingSize; i++ {
		decoyPub := crypto.Point32{byte(0x10 + i)}
		utxos.Add(&core.UTXO{
			TxHash:      crypto.Hash32{byte(i + 10)},
			OutputIndex: 0,
			OneTimePub:  decoyPub,
			AmountCommit: commit,
			BlockHeight: 1,
		})
	}

	vl := buildTestVestingLock(teamPub, genesisTime, func() int64 { return now })

	v := core.NewTxVerifier(utxos)
	v.SetVestingLock(vl)

	// Build a transaction whose ring[0] is the locked genesis team UTXO.
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = teamPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i] = crypto.Point32{byte(0x10 + i)}
	}
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				KeyImage:     makeKeyImage(999),
				Ring:         ring,
				AmountCommit: commit,
			},
		},
		Outputs: []core.Output{
			{OneTimePub: crypto.Point32{0xFE}, AmountCommit: crypto.Commitment{}},
		},
		Fee:         500,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}

	err := v.VerifyTx(&tx)
	if err == nil {
		t.Fatal("VerifyTx should reject a transaction spending a locked genesis UTXO, got nil error")
	}
	if !strings.Contains(err.Error(), "locked genesis") {
		t.Errorf("error should mention 'locked genesis', got: %v", err)
	}
}

// TestVestingEnforcement_FullyVestedAllowed verifies that after full vesting
// the same ring (now containing a fully-vested genesis UTXO) is not blocked
// by the vesting check (may still fail MLSAG, but NOT for vesting reasons).
func TestVestingEnforcement_FullyVestedAllowed(t *testing.T) {
	const genesisTime = int64(1_700_000_000)
	const cliffSeconds = int64(365 * 86400)
	const vestSeconds = int64(4 * 365 * 86400)
	// "now" is well after cliff + vest (fully vested).
	now := genesisTime + cliffSeconds + vestSeconds + 86400

	teamPub := crypto.Point32{0xDD, 0xEE, 0xFF}
	commit := crypto.Commitment{0x22}

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash: crypto.Hash32{0x02}, OutputIndex: 0,
		OneTimePub: teamPub, AmountCommit: commit, BlockHeight: 0,
	})
	for i := 1; i < crypto.RingSize; i++ {
		decoyPub := crypto.Point32{byte(0x20 + i)}
		utxos.Add(&core.UTXO{
			TxHash: crypto.Hash32{byte(i + 20)}, OutputIndex: 0,
			OneTimePub: decoyPub, AmountCommit: commit, BlockHeight: 1,
		})
	}

	vl := buildTestVestingLock(teamPub, genesisTime, func() int64 { return now })
	v := core.NewTxVerifier(utxos)
	v.SetVestingLock(vl)

	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = teamPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i] = crypto.Point32{byte(0x20 + i)}
	}
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{KeyImage: makeKeyImage(888), Ring: ring, AmountCommit: commit},
		},
		Outputs: []core.Output{
			{OneTimePub: crypto.Point32{0xFD}, AmountCommit: crypto.Commitment{}},
		},
		Fee:         500,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}

	err := v.VerifyTx(&tx)
	// The vesting check should NOT fire.  MLSAG may still fail (dummy sig) but
	// the error must not mention "locked genesis".
	if err != nil && strings.Contains(err.Error(), "locked genesis") {
		t.Errorf("fully-vested UTXO should not be blocked by vesting check, got: %v", err)
	}
}

// TestVestingEnforcement_NonGenesisUnaffected verifies that a ring composed
// entirely of non-genesis UTXOs passes the vesting check (no spurious rejections).
func TestVestingEnforcement_NonGenesisUnaffected(t *testing.T) {
	const genesisTime = int64(1_700_000_000)
	now := genesisTime + 86400

	teamPub := crypto.Point32{0x55, 0x66, 0x77}
	commit := crypto.Commitment{0x33}

	utxos := core.NewUTXOSet()
	// Add non-genesis UTXOs (height > 0) only.
	for i := 0; i < crypto.RingSize; i++ {
		pub := crypto.Point32{byte(0x30 + i)}
		utxos.Add(&core.UTXO{
			TxHash: crypto.Hash32{byte(i + 30)}, OutputIndex: 0,
			OneTimePub: pub, AmountCommit: commit, BlockHeight: 5,
		})
	}

	vl := buildTestVestingLock(teamPub, genesisTime, func() int64 { return now })
	v := core.NewTxVerifier(utxos)
	v.SetVestingLock(vl)

	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := 0; i < crypto.RingSize; i++ {
		ring[i] = crypto.Point32{byte(0x30 + i)}
	}
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{KeyImage: makeKeyImage(777), Ring: ring, AmountCommit: commit},
		},
		Outputs: []core.Output{
			{OneTimePub: crypto.Point32{0xFC}, AmountCommit: crypto.Commitment{}},
		},
		Fee:         500,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}

	err := v.VerifyTx(&tx)
	if err != nil && strings.Contains(err.Error(), "locked genesis") {
		t.Errorf("non-genesis ring should not trigger vesting check, got: %v", err)
	}
}

// TestVestingEnforcement_TimestampZeroBug documents why BuildVestingLock must
// receive the actual genesis block timestamp rather than GenesisConfig.Timestamp
// (which is 0 when the node generates the block on first start, e.g. testnet).
// With genesisTime=0 (Unix epoch), any recent "now" is billions of seconds past
// the cliff+vest window, so all tokens appear fully vested and enforcement is
// silently bypassed.  With the correct block timestamp enforcement is accurate.
func TestVestingEnforcement_TimestampZeroBug(t *testing.T) {
	vs := &core.VestingSchedule{
		Type:         core.VestingCliffLinear,
		CliffSeconds: int64(365 * 86400),       // 1-year cliff
		VestSeconds:  int64(4 * 365 * 86400),   // 4-year linear after cliff
	}
	total := uint64(1_000_000_000) * core.BaseUnitsPerAPR // 1 B APRO

	// Approximate "now" for August 2026.
	now := int64(1_754_000_000)

	// BUG scenario: genesisTime=0 (config default), elapsed ≈ 55 years >> 5-year
	// cliff+vest window → all tokens appear fully vested → enforcement bypassed.
	lockedEpoch := vs.LockedAmount(total, 0, now)
	if lockedEpoch != 0 {
		t.Logf("genesisTime=0: locked=%d (unexpectedly still locked — elapsed may be within window)", lockedEpoch)
	} else {
		t.Logf("genesisTime=0: lockedAmount=0 — BUG would silently allow spending locked tokens")
	}
	// Regardless, the correct genesisTime must give locked == total within cliff.

	// FIX scenario: actual genesis block time is 1 day before now.
	genesisTime := now - 86400
	lockedReal := vs.LockedAmount(total, genesisTime, now)
	if lockedReal != total {
		t.Errorf("with genesisTime=1 day ago, all tokens should be locked (within cliff); got locked=%d, want %d",
			lockedReal, total)
	}
}

// TestVestingEnforcement_MempoolRejectsLockedSpend verifies end-to-end that
// pool.Add() rejects a transaction whose ring includes a still-locked genesis
// UTXO — this is the mempool entry point used by P2P and API.
func TestVestingEnforcement_MempoolRejectsLockedSpend(t *testing.T) {
	const genesisTime = int64(1_700_000_000)
	now := genesisTime + 100 // well within cliff

	teamPub := crypto.Point32{0x99, 0x88, 0x77}
	commit := crypto.Commitment{0x44}

	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash: crypto.Hash32{0x03}, OutputIndex: 0,
		OneTimePub: teamPub, AmountCommit: commit, BlockHeight: 0,
	})
	for i := 1; i < crypto.RingSize; i++ {
		decoyPub := crypto.Point32{byte(0x40 + i)}
		utxos.Add(&core.UTXO{
			TxHash: crypto.Hash32{byte(i + 40)}, OutputIndex: 0,
			OneTimePub: decoyPub, AmountCommit: commit, BlockHeight: 2,
		})
	}

	vl := buildTestVestingLock(teamPub, genesisTime, func() int64 { return now })
	v := core.NewTxVerifier(utxos)
	v.SetVestingLock(vl)

	cfg := core.MempoolConfig{
		MaxSize:        10,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 0,
	}
	pool := core.NewMempool(cfg, silentLogger())
	pool.SetVerifier(v)

	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = teamPub
	for i := 1; i < crypto.RingSize; i++ {
		ring[i] = crypto.Point32{byte(0x40 + i)}
	}
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{KeyImage: makeKeyImage(666), Ring: ring, AmountCommit: commit},
		},
		Outputs: []core.Output{
			{OneTimePub: crypto.Point32{0xFB}, AmountCommit: crypto.Commitment{}},
		},
		Fee:         500,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}

	err := pool.Add(tx)
	if err == nil {
		t.Fatal("pool.Add() should reject a tx spending a locked genesis UTXO, got nil")
	}
	if !strings.Contains(err.Error(), "locked genesis") {
		t.Errorf("error should mention 'locked genesis', got: %v", err)
	}
	if pool.Count() != 0 {
		t.Errorf("pool should be empty after rejection, got %d entries", pool.Count())
	}
}
