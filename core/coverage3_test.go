package core_test

// Third round: target specific uncovered branches in core.

import (
        "testing"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── transaction.Validate: all error branches ─────────────────────────────────

func TestTransactionValidate_ZeroVersion(t *testing.T) {
        tx := core.Transaction{Version: 0, Outputs: []core.Output{{}}}
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject version 0")
        }
}

func TestTransactionValidate_NoOutputs(t *testing.T) {
        tx := core.Transaction{Version: core.TxVersionBase}
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject tx with no outputs")
        }
}

func TestTransactionValidate_TooManyOutputs(t *testing.T) {
        outs := make([]core.Output, 17)
        tx := core.Transaction{Version: core.TxVersionBase, Outputs: outs}
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject tx with >16 outputs")
        }
}

func TestTransactionValidate_ExtraTooBig(t *testing.T) {
        tx := core.Transaction{
                Version: core.TxVersionBase,
                Outputs: []core.Output{{}},
                Extra:   make([]byte, 256),
        }
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject tx with extra > 255 bytes")
        }
}

func TestTransactionValidate_Coinbase_Valid(t *testing.T) {
        wk, _ := crypto.GenerateWalletKeys()
        cb := core.CoinbaseTx(wk.Spend.Public, 1_000_000)
        if err := cb.Validate(); err != nil {
                t.Errorf("coinbase Validate: %v", err)
        }
}

func TestTransactionValidate_SigCountMismatch(t *testing.T) {
        var ki crypto.KeyImage
        ki[0] = 1
        ring := make([]crypto.RingMember, crypto.RingSize)
        tx := core.Transaction{
                Version: core.TxVersionBase,
                Inputs: []core.RingInput{{
                        KeyImage: ki,
                        Ring:     ring,
                }},
                Outputs:    []core.Output{{}, {}},
                Signatures: nil, // 0 sigs for 1 input
        }
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject sig count mismatch")
        }
}

func TestTransactionValidate_RangeProofCountMismatch(t *testing.T) {
        var ki crypto.KeyImage
        ki[0] = 1
        ring := make([]crypto.RingMember, crypto.RingSize)
        sig := &crypto.MLSAGSignature{}
        tx := core.Transaction{
                Version: core.TxVersionBase,
                Inputs: []core.RingInput{{
                        KeyImage: ki,
                        Ring:     ring,
                }},
                Outputs:     []core.Output{{}, {}},
                Signatures:  []*crypto.MLSAGSignature{sig},
                RangeProofs: nil, // 0 proofs for 2 outputs
        }
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject range proof count mismatch")
        }
}

func TestTransactionValidate_WrongRingSize(t *testing.T) {
        var ki crypto.KeyImage
        ki[0] = 1
        ring := make([]crypto.RingMember, 3) // wrong (need 11)
        sig := &crypto.MLSAGSignature{}
        proof := &crypto.RangeProof{}
        tx := core.Transaction{
                Version: core.TxVersionBase,
                Inputs: []core.RingInput{{
                        KeyImage: ki,
                        Ring:     ring,
                }},
                Outputs:     []core.Output{{}},
                Signatures:  []*crypto.MLSAGSignature{sig},
                RangeProofs: []*crypto.RangeProof{proof},
        }
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject wrong ring size")
        }
}

func TestTransactionValidate_ZeroKeyImage(t *testing.T) {
        ring := make([]crypto.RingMember, crypto.RingSize)
        sig := &crypto.MLSAGSignature{}
        proof := &crypto.RangeProof{}
        tx := core.Transaction{
                Version: core.TxVersionBase,
                Inputs: []core.RingInput{{
                        KeyImage: crypto.KeyImage{},
                        Ring:     ring,
                }},
                Outputs:     []core.Output{{}},
                Signatures:  []*crypto.MLSAGSignature{sig},
                RangeProofs: []*crypto.RangeProof{proof},
        }
        if err := tx.Validate(); err == nil {
                t.Error("Validate must reject zero key image")
        }
}

// ─── genesis: CreateGenesisBlock with allocations ─────────────────────────────

func TestCreateGenesisBlock_WithAllocations(t *testing.T) {
        priv, pub, _ := crypto.GenerateValidatorKey()
        gc := &core.GenesisConfig{
                ChainID:       "testnet-cov",
                InitialSupply: 21_000_000_000_000,
                BlockTimeMs:   2000,
                MinValidators: 1,
                BFTThreshold:  0.667,
                RingSize:      11,
                Validators:    []string{pub.Hex()},
                Allocations: []core.GenesisAlloc{
                        {Address: "addr1", Amount: 1_000_000},
                        {Address: "addr2", Amount: 500_000},
                },
        }
        block, err := core.CreateGenesisBlock(gc, priv)
        if err != nil {
                t.Fatalf("CreateGenesisBlock: %v", err)
        }
        if block == nil {
                t.Fatal("CreateGenesisBlock returned nil")
        }
        if block.Header.Height != 0 {
                t.Errorf("genesis height = %d, want 0", block.Header.Height)
        }
}
