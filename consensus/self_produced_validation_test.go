package consensus

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestSelfProducedBlockRejectsExistingOutputKeyBeforeStateMutation(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	lockedKey, err := crypto.NewLockedValidatorKey(validatorPriv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lockedKey.Destroy()

	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().Add(-time.Second).UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(validatorPriv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: genesisHeader}); err != nil {
		t.Fatal(err)
	}

	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddress := crypto.AddressFromKeys(crypto.MainnetByte, recipient)
	rewardTx, err := core.BuildMintTx(rewardAddress, DefaultBlockRewardNAPR, 1)
	if err != nil {
		t.Fatal(err)
	}

	// The deterministic reward output collides with a retained canonical key.
	// The full block verifier must reject it before ApplyBlock can overwrite
	// the existing by-public-key entry.
	existingHash := crypto.Hash32{0x7a}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{
		TxHash:     existingHash,
		OneTimePub: rewardTx.Outputs[0].OneTimePub,
	})

	cfg := Config{
		BlockTime:                           20 * time.Millisecond,
		Validators:                          []crypto.ValidatorPubKey{validatorPub},
		MyKey:                               lockedKey,
		RewardAddress:                       string(rewardAddress),
		BlockRewardNAPR:                     DefaultBlockRewardNAPR,
		SecurityHardeningActivationHeight:   1,
		RewardAuthorizationActivationHeight: ^uint64(0),
		RingCTV4ActivationHeight:            ^uint64(0),
	}
	engine := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	if err := engine.tick(); err == nil {
		t.Fatal("expected self-produced block verification to reject the colliding reward output")
	}
	if chain.Height() != 0 {
		t.Fatalf("chain height = %d, want unchanged genesis height 0", chain.Height())
	}
	retained, err := utxos.LookupRingMember(rewardTx.Outputs[0].OneTimePub)
	if err != nil {
		t.Fatalf("lookup retained output: %v", err)
	}
	if retained == nil || retained.TxHash != existingHash {
		t.Fatalf("retained output = %#v, want original tx hash %x", retained, existingHash)
	}
}

func TestSelfProducedBlockFailsClosedWithoutTransactionVerifier(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	lockedKey, err := crypto.NewLockedValidatorKey(validatorPriv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lockedKey.Destroy()

	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().Add(-time.Second).UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(validatorPriv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: genesisHeader}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(Config{
		BlockTime:  20 * time.Millisecond,
		Validators: []crypto.ValidatorPubKey{validatorPub},
		MyKey:      lockedKey,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := engine.tick(); err == nil {
		t.Fatal("expected block production to fail closed without a transaction verifier")
	}
	if chain.Height() != 0 {
		t.Fatalf("chain height = %d, want unchanged genesis height 0", chain.Height())
	}
}
