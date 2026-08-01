package core_test

// vesting_enforcement_test.go — protocol-level enforcement of genesis vesting
// locks inside TxVerifier.  Verifies that spending a still-locked genesis UTXO
// is rejected at mempool entry (and that unlocked / non-genesis UTXOs pass).

import (
	"fmt"
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

// TestVestingEnforcement_SurvivesRestartReplay is an integration test that
// simulates a node restart by creating a real genesis block via
// CreateGenesisBlock, then replaying it into a brand-new UTXOSet (exactly as
// the node does on startup when it scans the chain from height 0).
//
// It verifies three invariants that must hold after replay:
//
//  1. ApplyBlock populates byPubKey with the genesis UTXO so ring-member
//     lookups succeed (C-0 check passes).
//  2. BuildVestingLock (the production address-decode path) correctly maps the
//     team allocation's spendPub → GenesisAlloc — no synthetic helper is used.
//  3. VerifyTx rejects an attempt to spend the still-locked genesis UTXO with
//     an error mentioning "locked genesis" — enforcement is not bypassed by a
//     restart or chain replay.
//
// The test is run twice (two independent fresh UTXOSets) to confirm the result
// is idempotent across restarts.
func TestVestingEnforcement_SurvivesRestartReplay(t *testing.T) {
	// ── 1. Build genesis config with one cliff_linear team allocation ──────────
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}

	teamKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	teamAddr := crypto.AddressFromKeys(crypto.MainnetByte, teamKeys)

	const teamAmount = uint64(1_000_000) * core.BaseUnitsPerAPR // 1 M APRO

	genesis := &core.GenesisConfig{
		ChainID:       "test-restart-vesting",
		InitialSupply: 100_000_000,
		MinValidators: 1,
		BFTThreshold:  0.667,
		RingSize:      crypto.RingSize,
		// ValidatorPubKey is an ed25519.PublicKey ([]byte); hex-encode for YAML field.
		Validators: []string{fmt.Sprintf("%x", validatorPub)},
		Allocations: []core.GenesisAlloc{
			{
				Address: string(teamAddr),
				Amount:  teamAmount,
				Label:   "Team",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(365 * 86400),     // 1-year cliff
					VestSeconds:  int64(4 * 365 * 86400), // 4-year linear vest
				},
			},
		},
	}

	// ── 2. Create the genesis block (as the node does on first start) ─────────
	genesisBlock, err := core.CreateGenesisBlock(genesis, validatorPriv)
	if err != nil {
		t.Fatalf("CreateGenesisBlock: %v", err)
	}
	// Header.Timestamp is stored in nanoseconds; convert to seconds for vesting.
	genesisTimeSec := genesisBlock.Header.Timestamp / 1_000_000_000

	// Recover the team allocation's spendPub — this equals OneTimePub in the
	// genesis output (transparent mint: OneTimePub = spendPub, no stealth).
	_, spendPub, _, err := crypto.DecodeAddress(teamAddr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}

	// runRestart encapsulates one restart cycle: fresh UTXOSet + replay +
	// VestingLock build + spend attempt.  Called twice to confirm idempotency.
	runRestart := func(t *testing.T, label string, decoyBase byte, kiIdx int) {
		t.Helper()

		// ── 3. Simulate restart: fresh UTXOSet + chain replay from height 0 ───
		freshUTXOs := core.NewUTXOSet()
		if err := freshUTXOs.ApplyBlock(genesisBlock); err != nil {
			t.Fatalf("%s: ApplyBlock on fresh UTXOSet: %v", label, err)
		}

		// Confirm genesis output is reachable via byPubKey — proves that
		// ApplyBlock properly populates the index used by TxVerifier C-0 check.
		genesisUTXO := freshUTXOs.GetByPubKey(spendPub)
		if genesisUTXO == nil {
			t.Fatalf("%s: genesis UTXO not found via GetByPubKey after ApplyBlock — "+
				"byPubKey not populated on restart replay", label)
		}

		// ── 4. Build VestingLock via the production address-decode path ───────
		vl, err := core.BuildVestingLock(genesis, genesisTimeSec)
		if err != nil {
			t.Fatalf("%s: BuildVestingLock: %v", label, err)
		}
		if vl.LockedAllocsCount() != 1 {
			t.Fatalf("%s: expected 1 locked allocation, got %d", label, vl.LockedAllocsCount())
		}

		// ── 5. Populate ring decoys using the real genesis UTXO commitment ────
		// All ring members must share the same AmountCommit as inp.AmountCommit
		// so the C-0 commitment-binding check passes and execution reaches the
		// vesting check (which fires next).
		genesisCommit := genesisUTXO.AmountCommit
		for i := 1; i < crypto.RingSize; i++ {
			decoyPub := crypto.Point32{decoyBase + byte(i)}
			freshUTXOs.Add(&core.UTXO{
				TxHash:       crypto.Hash32{decoyBase + byte(i)},
				OutputIndex:  0,
				OneTimePub:   decoyPub,
				AmountCommit: genesisCommit,
				BlockHeight:  1,
			})
		}

		// ── 6. Wire TxVerifier with the VestingLock (as node startup does) ────
		v := core.NewTxVerifier(freshUTXOs)
		v.SetVestingLock(vl)

		// ── 7. Attempt to spend the locked genesis UTXO ───────────────────────
		ring := make([]crypto.RingMember, crypto.RingSize)
		ring[0] = spendPub // locked genesis UTXO
		for i := 1; i < crypto.RingSize; i++ {
			ring[i] = crypto.Point32{decoyBase + byte(i)}
		}
		tx := core.Transaction{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{
					KeyImage:     makeKeyImage(kiIdx),
					Ring:         ring,
					AmountCommit: genesisCommit,
				},
			},
			Outputs: []core.Output{
				{OneTimePub: crypto.Point32{0xF8, decoyBase}, AmountCommit: crypto.Commitment{}},
			},
			Fee:         500,
			Signatures:  []*crypto.MLSAGSignature{{}},
			RangeProofs: []*crypto.RangeProof{{}},
		}

		verifyErr := v.VerifyTx(&tx)
		if verifyErr == nil {
			t.Fatalf("%s: VerifyTx should reject locked genesis UTXO spend after restart replay, got nil", label)
		}
		if !strings.Contains(verifyErr.Error(), "locked genesis") {
			t.Errorf("%s: expected 'locked genesis' in error, got: %v", label, verifyErr)
		}
	}

	// Run the restart simulation twice to confirm idempotency.
	runRestart(t, "first restart", 0x70, 555)
	runRestart(t, "second restart", 0x80, 444)
}

// TestBuildVestingLock_DuplicateAddressReturnsError verifies that
// BuildVestingLock returns an error when the genesis config contains two
// non-immediate allocations with the same address (same spendPub).
// Previously the second entry silently overwrote the first, which could drop
// a cliff_linear schedule and allow immediate spending of locked tokens.
func TestBuildVestingLock_DuplicateAddressReturnsError(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = validatorPriv

	teamKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	teamAddr := crypto.AddressFromKeys(crypto.MainnetByte, teamKeys)

	genesis := &core.GenesisConfig{
		ChainID:       "test-dup-vesting",
		InitialSupply: 100_000_000,
		MinValidators: 1,
		BFTThreshold:  0.667,
		RingSize:      crypto.RingSize,
		Validators:    []string{fmt.Sprintf("%x", validatorPub)},
		Allocations: []core.GenesisAlloc{
			{
				Address: string(teamAddr),
				Amount:  500_000 * core.BaseUnitsPerAPR,
				Label:   "Team cliff_linear strict",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(365 * 86400),
					VestSeconds:  int64(4 * 365 * 86400),
				},
			},
			{
				// Same address, different (weaker) schedule — the second entry would
				// silently overwrite the first, replacing a 4-year vest with a 1-year
				// vest and letting the key-holder spend 3 years early.
				Address: string(teamAddr),
				Amount:  500_000 * core.BaseUnitsPerAPR,
				Label:   "Team cliff_linear weak duplicate",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(30 * 86400),  // only 30-day cliff
					VestSeconds:  int64(365 * 86400), // only 1-year vest
				},
			},
		},
	}

	const genesisTimeSec = int64(1_700_000_000)
	_, buildErr := core.BuildVestingLock(genesis, genesisTimeSec)
	if buildErr == nil {
		t.Fatal("BuildVestingLock should return an error for duplicate allocation addresses, got nil")
	}
	if !strings.Contains(buildErr.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", buildErr)
	}
}

// TestGenesisValidate_DuplicateAddressRejected verifies that
// GenesisConfig.Validate() rejects a config whose allocations contain two
// entries with the same address.  This is the first defence — the config
// should be rejected at load time before BuildVestingLock is ever called.
func TestGenesisValidate_DuplicateAddressRejected(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = validatorPriv

	teamKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	teamAddr := crypto.AddressFromKeys(crypto.MainnetByte, teamKeys)

	genesis := &core.GenesisConfig{
		ChainID:       "test-dup-validate",
		InitialSupply: 100_000_000,
		MinValidators: 1,
		BFTThreshold:  0.667,
		RingSize:      crypto.RingSize,
		Validators:    []string{fmt.Sprintf("%x", validatorPub)},
		Allocations: []core.GenesisAlloc{
			{
				Address: string(teamAddr),
				Amount:  1_000_000 * core.BaseUnitsPerAPR,
				Label:   "First entry",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(365 * 86400),
					VestSeconds:  int64(4 * 365 * 86400),
				},
			},
			{
				Address: string(teamAddr),
				Amount:  500_000 * core.BaseUnitsPerAPR,
				Label:   "Duplicate entry",
				Vesting: nil,
			},
		},
	}

	if err := genesis.Validate(); err == nil {
		t.Fatal("Validate() should reject genesis with duplicate allocation addresses, got nil")
	} else if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", err)
	}
}

// TestGenesisValidate_SameSpendPubDifferentAddressRejected verifies that
// Validate() rejects two allocations whose addresses have distinct strings but
// share the same spend public key (different view key).  This is the
// "alias" attack: the attacker supplies addr1 = Encode(spendPub, viewA) and
// addr2 = Encode(spendPub, viewB).  Both pass the string-equality check but
// BuildVestingLock would overwrite the first alloc map entry with the second.
func TestGenesisValidate_SameSpendPubDifferentAddressRejected(t *testing.T) {
	// Construct two WalletKeyPairs that share the same spend public key but
	// use different view keys by directly calling EncodeAddress.
	keys1, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys2, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	// Addr1: keys1.Spend.Public + keys1.View.Public  (the real address)
	// Addr2: keys1.Spend.Public + keys2.View.Public  (alias: same spendPub, different viewPub)
	addr1 := crypto.EncodeAddress(crypto.MainnetByte, keys1.Spend.Public, keys1.View.Public)
	addr2 := crypto.EncodeAddress(crypto.MainnetByte, keys1.Spend.Public, keys2.View.Public)

	if addr1 == addr2 {
		t.Fatal("test setup error: expected two distinct address strings for same spendPub + different viewPub")
	}

	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = validatorPriv

	genesis := &core.GenesisConfig{
		ChainID:       "test-alias-spendpub",
		InitialSupply: 100_000_000,
		MinValidators: 1,
		BFTThreshold:  0.667,
		RingSize:      crypto.RingSize,
		Validators:    []string{fmt.Sprintf("%x", validatorPub)},
		Allocations: []core.GenesisAlloc{
			{
				Address: string(addr1),
				Amount:  1_000_000 * core.BaseUnitsPerAPR,
				Label:   "Team (real address)",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(365 * 86400),
					VestSeconds:  int64(4 * 365 * 86400),
				},
			},
			{
				// Different address string, same underlying spendPub — alias attack.
				Address: string(addr2),
				Amount:  1_000_000 * core.BaseUnitsPerAPR,
				Label:   "Team (alias — same spendPub, different viewPub)",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(30 * 86400),
					VestSeconds:  int64(365 * 86400),
				},
			},
		},
	}

	if err := genesis.Validate(); err == nil {
		t.Fatal("Validate() should reject genesis with two allocations sharing the same spendPub, got nil")
	} else if !strings.Contains(err.Error(), "spend public key") {
		t.Errorf("error should mention 'spend public key', got: %v", err)
	}
}

// TestBuildVestingLock_SameSpendPubDifferentAddressReturnsError verifies that
// BuildVestingLock also returns an error (second line of defence) when two
// non-immediate allocations share a spendPub via distinct address strings.
// This covers the case where the genesis config was not run through Validate()
// before BuildVestingLock is called (e.g. programmatic construction in tooling).
func TestBuildVestingLock_SameSpendPubDifferentAddressReturnsError(t *testing.T) {
	keys1, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys2, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr1 := crypto.EncodeAddress(crypto.MainnetByte, keys1.Spend.Public, keys1.View.Public)
	addr2 := crypto.EncodeAddress(crypto.MainnetByte, keys1.Spend.Public, keys2.View.Public)

	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = validatorPriv

	genesis := &core.GenesisConfig{
		ChainID:       "test-alias-build",
		InitialSupply: 100_000_000,
		MinValidators: 1,
		BFTThreshold:  0.667,
		RingSize:      crypto.RingSize,
		Validators:    []string{fmt.Sprintf("%x", validatorPub)},
		Allocations: []core.GenesisAlloc{
			{
				Address: string(addr1),
				Amount:  500_000 * core.BaseUnitsPerAPR,
				Label:   "Alloc A",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(365 * 86400),
					VestSeconds:  int64(4 * 365 * 86400),
				},
			},
			{
				Address: string(addr2),
				Amount:  500_000 * core.BaseUnitsPerAPR,
				Label:   "Alloc B (alias)",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(30 * 86400),
					VestSeconds:  int64(365 * 86400),
				},
			},
		},
	}

	const genesisTimeSec = int64(1_700_000_000)
	_, buildErr := core.BuildVestingLock(genesis, genesisTimeSec)
	if buildErr == nil {
		t.Fatal("BuildVestingLock should return an error for two allocations sharing the same spendPub, got nil")
	}
	if !strings.Contains(buildErr.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", buildErr)
	}
}

// TestGenesisValidate_UniqueAddressesPass verifies that Validate() does not
// reject a valid config where all allocation addresses are distinct.
func TestGenesisValidate_UniqueAddressesPass(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = validatorPriv

	keys1, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys2, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr1 := crypto.AddressFromKeys(crypto.MainnetByte, keys1)
	addr2 := crypto.AddressFromKeys(crypto.MainnetByte, keys2)

	genesis := &core.GenesisConfig{
		ChainID:       "test-unique-validate",
		InitialSupply: 100_000_000,
		MinValidators: 1,
		BFTThreshold:  0.667,
		RingSize:      crypto.RingSize,
		Validators:    []string{fmt.Sprintf("%x", validatorPub)},
		Allocations: []core.GenesisAlloc{
			{
				Address: string(addr1),
				Amount:  1_000_000 * core.BaseUnitsPerAPR,
				Label:   "Team",
				Vesting: &core.VestingSchedule{
					Type:         core.VestingCliffLinear,
					CliffSeconds: int64(365 * 86400),
					VestSeconds:  int64(4 * 365 * 86400),
				},
			},
			{
				Address: string(addr2),
				Amount:  500_000 * core.BaseUnitsPerAPR,
				Label:   "Advisors",
				Vesting: nil,
			},
		},
	}

	if err := genesis.Validate(); err != nil {
		t.Errorf("Validate() should accept genesis with unique allocation addresses, got: %v", err)
	}
}

// TestVestingEnforcement_PrunedGenesisStart simulates a light-pruning startup
// where the genesis block's TxData has been stripped by PruneBlocksOlderThan,
// so the genesis UTXO is absent from byPubKey (it was never replayed into the
// in-memory UTXOSet because ApplyBlock over a pruned block adds no outputs).
//
// This is the scenario described in the task: does vesting enforcement still
// work correctly, or does the C-0 ring-member presence check fire first with
// a misleading "not found in UTXO set" error?
//
// Finding: because the vesting check (3b in VerifyTx) now runs BEFORE the C-0
// check (3c), VestingLock alone is sufficient.  byPubKey does NOT need to be
// pre-seeded for vesting to fire — the VestingLock reads only from its own
// allocs map (built from genesis config), independent of the UTXO set state.
//
// The test also confirms the complementary case: a non-genesis ring member
// that IS absent from byPubKey fails with a C-0 error (not a vesting error),
// which is the correct behaviour for a genuinely missing UTXO.
func TestVestingEnforcement_PrunedGenesisStart(t *testing.T) {
	const genesisTime = int64(1_700_000_000)
	// "now" is 1 day after genesis — well within the 1-year cliff.
	now := genesisTime + 86400

	teamPub := crypto.Point32{0xCA, 0xFE, 0xBA, 0xBE}
	commit := crypto.Commitment{0x55}

	// ── Pruned-start UTXOSet: genesis block TxData was stripped ───────────────
	// Simulate what happens when PruneBlocksOlderThan strips the genesis block:
	// ApplyBlock on the pruned block adds no outputs (b.Txs is empty), so the
	// genesis UTXO is absent from byPubKey.  We replicate this by building a
	// UTXOSet with only recent-block decoys and NO genesis entry.
	prunedUTXOs := core.NewUTXOSet()
	// Add recent-block decoy UTXOs (height > 0) — these ARE in byPubKey.
	for i := 1; i < crypto.RingSize; i++ {
		decoyPub := crypto.Point32{byte(0x50 + i)}
		prunedUTXOs.Add(&core.UTXO{
			TxHash:       crypto.Hash32{byte(0x50 + i)},
			OutputIndex:  0,
			OneTimePub:   decoyPub,
			AmountCommit: commit,
			BlockHeight:  100, // recent block, not genesis
		})
	}
	// Confirm: genesis UTXO is NOT reachable via byPubKey — this is the
	// pruned-start state we are testing against.
	if prunedUTXOs.GetByPubKey(teamPub) != nil {
		t.Fatal("test setup error: genesis UTXO should be absent from pruned UTXOSet")
	}

	// ── VestingLock is always available (built from genesis config) ───────────
	vl := buildTestVestingLock(teamPub, genesisTime, func() int64 { return now })
	v := core.NewTxVerifier(prunedUTXOs)
	v.SetVestingLock(vl)

	// ── Sub-test A: spend attempt whose ring[0] is the locked genesis pub ─────
	//
	// With the old check ordering (C-0 before vesting), this would produce
	// "not found in UTXO set — C-0 full check" because teamPub is absent from
	// byPubKey.  After the fix (vesting before C-0), it must produce a
	// "locked genesis" error regardless of pruning state.
	t.Run("locked_genesis_pub_absent_from_byPubKey", func(t *testing.T) {
		ring := make([]crypto.RingMember, crypto.RingSize)
		ring[0] = teamPub // locked genesis UTXO — NOT in byPubKey
		for i := 1; i < crypto.RingSize; i++ {
			ring[i] = crypto.Point32{byte(0x50 + i)} // present in byPubKey
		}
		tx := core.Transaction{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{KeyImage: makeKeyImage(333), Ring: ring, AmountCommit: commit},
			},
			Outputs: []core.Output{
				{OneTimePub: crypto.Point32{0xF7}, AmountCommit: crypto.Commitment{}},
			},
			Fee:         500,
			Signatures:  []*crypto.MLSAGSignature{{}},
			RangeProofs: []*crypto.RangeProof{{}},
		}

		err := v.VerifyTx(&tx)
		if err == nil {
			t.Fatal("VerifyTx must reject locked genesis spend in pruned-start scenario, got nil")
		}
		// Must be a vesting error, not a C-0 error.
		if !strings.Contains(err.Error(), "locked genesis") {
			t.Errorf("expected 'locked genesis' error (vesting check before C-0), got: %v", err)
		}
		if strings.Contains(err.Error(), "C-0") {
			t.Errorf("must NOT be a C-0 error when genesis is pruned — vesting should fire first; got: %v", err)
		}
	})

	// ── Sub-test B: non-genesis ring member absent from byPubKey ─────────────
	//
	// A ring member that was never in the genesis config and is also absent
	// from byPubKey (fabricated / never-existed UTXO) must still fail C-0 —
	// this confirms that moving vesting before C-0 does not weaken the C-0 guard.
	t.Run("fabricated_non_genesis_pub_still_fails_C0", func(t *testing.T) {
		fabricatedPub := crypto.Point32{0xDE, 0xAD, 0xBE, 0xEF}
		ring := make([]crypto.RingMember, crypto.RingSize)
		ring[0] = fabricatedPub // not in genesis allocs AND not in byPubKey
		for i := 1; i < crypto.RingSize; i++ {
			ring[i] = crypto.Point32{byte(0x50 + i)} // present in byPubKey
		}
		tx := core.Transaction{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{KeyImage: makeKeyImage(222), Ring: ring, AmountCommit: commit},
			},
			Outputs: []core.Output{
				{OneTimePub: crypto.Point32{0xF6}, AmountCommit: crypto.Commitment{}},
			},
			Fee:         500,
			Signatures:  []*crypto.MLSAGSignature{{}},
			RangeProofs: []*crypto.RangeProof{{}},
		}

		err := v.VerifyTx(&tx)
		if err == nil {
			t.Fatal("VerifyTx must reject fabricated ring member, got nil")
		}
		// Must be a C-0 error (not a vesting error — fabricated pub is not in genesis allocs).
		if !strings.Contains(err.Error(), "C-0") {
			t.Errorf("expected C-0 error for fabricated ring member, got: %v", err)
		}
		if strings.Contains(err.Error(), "locked genesis") {
			t.Errorf("fabricated non-genesis pub should not trigger vesting check, got: %v", err)
		}
	})

	// ── Sub-test C: mempool rejects locked genesis spend even when pruned ─────
	//
	// Confirms the full mempool path, not just TxVerifier directly.
	t.Run("mempool_rejects_locked_spend_pruned_start", func(t *testing.T) {
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
			ring[i] = crypto.Point32{byte(0x50 + i)}
		}
		tx := core.Transaction{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{KeyImage: makeKeyImage(111), Ring: ring, AmountCommit: commit},
			},
			Outputs: []core.Output{
				{OneTimePub: crypto.Point32{0xF5}, AmountCommit: crypto.Commitment{}},
			},
			Fee:         500,
			Signatures:  []*crypto.MLSAGSignature{{}},
			RangeProofs: []*crypto.RangeProof{{}},
		}

		err := pool.Add(tx)
		if err == nil {
			t.Fatal("pool.Add() must reject locked genesis spend in pruned-start scenario, got nil")
		}
		if !strings.Contains(err.Error(), "locked genesis") {
			t.Errorf("mempool error must mention 'locked genesis', got: %v", err)
		}
		if pool.Count() != 0 {
			t.Errorf("pool must be empty after rejection, got %d entries", pool.Count())
		}
	})
}
