package wallet

import (
        "testing"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── NewBuilder validation ────────────────────────────────────────────────────

func TestNewBuilder_NilDerivedKeys(t *testing.T) {
        chain := core.NewChain()
        _, err := NewBuilder(nil, chain)
        if err == nil {
                t.Fatal("expected error for nil DerivedKeys")
        }
}

func TestNewBuilder_NilChain(t *testing.T) {
        mnemonic, err := GenerateMnemonic(Strength128)
        if err != nil {
                t.Fatalf("GenerateMnemonic: %v", err)
        }
        dk, err := DeriveFromMnemonic(mnemonic, "", 0, 0)
        if err != nil {
                t.Fatalf("DeriveFromMnemonic: %v", err)
        }
        _, err = NewBuilder(dk, nil)
        if err == nil {
                t.Fatal("expected error for nil chain")
        }
}

func TestNewBuilder_OK(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        b, err := NewBuilder(dk, chain)
        if err != nil {
                t.Fatalf("NewBuilder: %v", err)
        }
        if b == nil {
                t.Fatal("builder is nil")
        }
}

// ─── Address ─────────────────────────────────────────────────────────────────

func TestBuilder_Address(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        b, _ := NewBuilder(dk, chain)
        addr := b.Address()
        if err := crypto.Validate(addr); err != nil {
                t.Fatalf("builder.Address() is invalid: %v", err)
        }
}

// ─── EstimateFee ──────────────────────────────────────────────────────────────

func TestBuilder_EstimateFee(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        b, _ := NewBuilder(dk, chain)
        fee := b.EstimateFee(1, 2)
        if fee == 0 {
                t.Error("EstimateFee returned 0 for 1-in 2-out tx")
        }
        // More inputs → higher fee.
        fee2 := b.EstimateFee(3, 2)
        if fee2 <= fee {
                t.Errorf("expected fee2(%d) > fee(%d) for more inputs", fee2, fee)
        }
}

// ─── WithFeePerByte ───────────────────────────────────────────────────────────

func TestBuilder_WithFeePerByte(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        b1, _ := NewBuilder(dk, chain, WithFeePerByte(1))
        b10, _ := NewBuilder(dk, chain, WithFeePerByte(10))
        f1 := b1.EstimateFee(1, 2)
        f10 := b10.EstimateFee(1, 2)
        if f10 != f1*10 {
                t.Errorf("fee@10 (%d) should be 10× fee@1 (%d)", f10, f1)
        }
}

// ─── Balance (empty chain) ────────────────────────────────────────────────────

func TestBuilder_Balance_EmptyChain(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        b, _ := NewBuilder(dk, chain)
        balance, owned, err := b.Balance()
        if err != nil {
                t.Fatalf("Balance: %v", err)
        }
        if balance != 0 {
                t.Errorf("expected 0 balance on empty chain, got %d", balance)
        }
        if len(owned) != 0 {
                t.Errorf("expected 0 owned UTXOs on empty chain, got %d", len(owned))
        }
}

// ─── Send: no funds ──────────────────────────────────────────────────────────

func TestBuilder_Send_InsufficientFunds(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        b, _ := NewBuilder(dk, chain)

        // Recipient: generate a fresh wallet address.
        recvKeys, err := crypto.GenerateWalletKeys()
        if err != nil {
                t.Fatalf("GenerateWalletKeys: %v", err)
        }
        recipientAddr := crypto.EncodeAddress(crypto.MainnetByte, recvKeys.Spend.Public, recvKeys.View.Public)

        _, err = b.Send(1_000_000, recipientAddr)
        if err == nil {
                t.Fatal("expected error when no UTXOs")
        }
}

// ─── WithBroadcaster ──────────────────────────────────────────────────────────

func TestBuilder_WithBroadcaster_Option(t *testing.T) {
        dk, chain := mustDeriveAndChain(t)
        br := &mockBroadcaster{}
        b, err := NewBuilder(dk, chain, WithBroadcaster(br))
        if err != nil {
                t.Fatalf("NewBuilder: %v", err)
        }
        if b.broadcaster == nil {
                t.Error("broadcaster was not set by WithBroadcaster")
        }
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func mustDeriveAndChain(t *testing.T) (*DerivedKeys, *core.Chain) {
        t.Helper()
        mnemonic, err := GenerateMnemonic(Strength128)
        if err != nil {
                t.Fatalf("GenerateMnemonic: %v", err)
        }
        dk, err := DeriveFromMnemonic(mnemonic, "", 0, 0)
        if err != nil {
                t.Fatalf("DeriveFromMnemonic: %v", err)
        }
        return dk, core.NewChain()
}

type mockBroadcaster struct {
        called int
}

func (m *mockBroadcaster) Broadcast(_ *core.Transaction) error {
        m.called++
        return nil
}
