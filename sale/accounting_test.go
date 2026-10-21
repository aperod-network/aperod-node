package sale

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

type fixture struct {
	c        Checkpoint
	keys     []crypto.ValidatorPrivKey
	trusted  []crypto.ValidatorPubKey
	merchant crypto.ValidatorPrivKey
	address  crypto.Address
}

func setup(t *testing.T) fixture {
	t.Helper()
	var f fixture
	for i := 0; i < 4; i++ {
		key, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		f.keys = append(f.keys, key)
		f.trusted = append(f.trusted, pub)
	}
	key, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	f.merchant = key
	wallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	f.address = crypto.AddressFromKeys(crypto.MainnetByte, wallet)
	f.c = Checkpoint{
		Genesis: crypto.HashBytes([]byte("sale-test-genesis")), Network: crypto.MainnetByte, Height: 101,
		Parent:         crypto.HashBytes([]byte("sale-test-parent")),
		SnapshotRoot:   crypto.HashBytes([]byte("sale-test-nominal-snapshot")),
		HistoricIssued: CapNAPRO / 2, OtherReserved: CapNAPRO / 4,
		SaleRemaining: CapNAPRO / 4, SaleAuthority: pub,
	}
	f.signCheckpoint(t)
	return f
}

func (f *fixture) signCheckpoint(t *testing.T) {
	t.Helper()
	f.c.Attestations = nil
	for i := 0; i < 3; i++ {
		sig, err := f.keys[i].Sign(f.c.Message())
		if err != nil {
			t.Fatal(err)
		}
		f.c.Attestations = append(f.c.Attestations, Attestation{Validator: f.trusted[i], Signature: sig})
	}
}

func (f fixture) ledger(t *testing.T) *Ledger {
	t.Helper()
	s, err := Begin(f.c, f.c.Genesis, f.c.Parent, f.c.Height, f.trusted)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f fixture) purchase(t *testing.T, order, payment string, amount uint64, height uint64, parent crypto.Hash32) Purchase {
	t.Helper()
	p := Purchase{Genesis: f.c.Genesis, Height: height, Parent: parent,
		OrderHash: crypto.HashBytes([]byte(order)), PaymentHash: crypto.HashBytes([]byte(payment)),
		Recipient: f.address, Amount: amount}
	sig, err := f.merchant.Sign(p.Message())
	if err != nil {
		t.Fatal(err)
	}
	p.Signature = sig
	return p
}

func TestForwardCheckpointQuorumAndCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture)
	}{
		{"two_of_four_is_not_quorum", func(f *fixture) { f.c.Attestations = f.c.Attestations[:2] }},
		{"duplicate_signer", func(f *fixture) { f.c.Attestations[2] = f.c.Attestations[0] }},
		{"empty_quorum", func(f *fixture) { f.c.Attestations = nil }},
		{"untrusted_signer", func(f *fixture) { f.trusted = f.trusted[1:] }},
		{"duplicate_trusted_authority", func(f *fixture) { f.trusted = append(f.trusted, f.trusted[0]) }},
		{"missing_trust_set", func(f *fixture) { f.trusted = nil }},
		{"signature_tampering", func(f *fixture) { f.c.Attestations[0].Signature[0] ^= 1 }},
		{"budget_changed_after_signing", func(f *fixture) { f.c.SaleRemaining-- }},
		{"authority_changed_after_signing", func(f *fixture) { f.c.SaleAuthority = f.trusted[0] }},
		{"network_changed_after_signing", func(f *fixture) { f.c.Network = crypto.TestnetByte }},
		{"signed_cap_violation", func(f *fixture) { f.c.SaleRemaining++; f.signCheckpoint(t) }},
		{"signed_reserved_overflow", func(f *fixture) { f.c.OtherReserved = ^uint64(0); f.signCheckpoint(t) }},
		{"signed_zero_budget", func(f *fixture) { f.c.SaleRemaining = 0; f.signCheckpoint(t) }},
		{"signed_zero_snapshot", func(f *fixture) { f.c.SnapshotRoot = zeroHash; f.signCheckpoint(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			tc.change(&f)
			if _, err := Begin(f.c, f.c.Genesis, f.c.Parent, f.c.Height, f.trusted); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
	f := setup(t)
	if _, err := Begin(f.c, f.c.Genesis, crypto.HashBytes([]byte("other-parent")), f.c.Height, f.trusted); err == nil {
		t.Fatal("wrong parent accepted")
	}
	if _, err := Begin(f.c, crypto.HashBytes([]byte("other-chain")), f.c.Parent, f.c.Height, f.trusted); err == nil {
		t.Fatal("wrong chain accepted")
	}
	if _, err := Begin(f.c, f.c.Genesis, f.c.Parent, f.c.Height+1, f.trusted); err == nil {
		t.Fatal("wrong activation height accepted")
	}
}

func TestExplorerNominalHeadlineCannotBeUsedAsSpendableSaleBudget(t *testing.T) {
	f := setup(t)
	// The live LPoD v3 witness declares 6.23B nominal pre-funding circulation.
	// A 6.864B APRO explorer available-allocation snapshot cannot *also* be
	// declared newly spendable, even before accounting for other reserves.
	f.c.HistoricIssued = 6_230_000_000 * 100_000_000
	f.c.OtherReserved = 0
	f.c.SaleRemaining = 6_864_595_139 * 100_000_000
	f.signCheckpoint(t)
	if _, err := Begin(f.c, f.c.Genesis, f.c.Parent, f.c.Height, f.trusted); err == nil {
		t.Fatal("nominal explorer headline double-counted against already declared circulation")
	}
}

func TestIndependentReplayRestoresSameDebitAndReplayIndex(t *testing.T) {
	f := setup(t)
	a := f.ledger(t)
	b := f.ledger(t)
	for i, order := range []string{"one", "two"} {
		height := uint64(101 + i)
		if a.Hash() != b.Hash() {
			t.Fatal("nodes diverged before replay")
		}
		p := f.purchase(t, order, "payment-"+order, 100, height, a.Hash())
		hash := crypto.HashBytes([]byte("block-" + order))
		var err error
		a, err = a.ApplyAccountedPurchases(height, a.Hash(), hash, []Purchase{p})
		if err != nil {
			t.Fatal(err)
		}
		b, err = b.ApplyAccountedPurchases(height, b.Hash(), hash, []Purchase{p})
		if err != nil {
			t.Fatal(err)
		}
		if a.Remaining() != b.Remaining() || a.Sold() != b.Sold() || !b.Seen(p.OrderHash) {
			t.Fatal("independent replay mismatch")
		}
	}
	duplicate := f.purchase(t, "one", "new-payment", 50, 103, a.Hash())
	if _, err := b.ApplyAccountedPurchases(103, b.Hash(), crypto.HashBytes([]byte("block-103")), []Purchase{duplicate}); err == nil {
		t.Fatal("replayed node lost spent-order index")
	}
}

func TestPurchaseDebitRollbackAndReplay(t *testing.T) {
	f := setup(t)
	s := f.ledger(t)
	p := f.purchase(t, "order-a", "payment-a", 100, 101, s.Hash())
	h := crypto.HashBytes([]byte("block-101"))
	next, err := s.ApplyAccountedPurchases(101, s.Hash(), h, []Purchase{p})
	if err != nil {
		t.Fatal(err)
	}
	if next.Remaining() != f.c.SaleRemaining-100 || next.Sold() != 100 || !next.Seen(p.OrderHash) {
		t.Fatal("incorrect debit")
	}
	if s.Seen(p.OrderHash) || s.Sold() != 0 || s.Remaining() != f.c.SaleRemaining {
		t.Fatal("parent mutated")
	}
	replay := f.purchase(t, "order-a", "another-payment", 100, 102, next.Hash())
	if _, err := next.ApplyAccountedPurchases(102, next.Hash(), crypto.HashBytes([]byte("block-102")), []Purchase{replay}); err == nil {
		t.Fatal("order replay accepted")
	}
	duplicatePayment := f.purchase(t, "different-order", "payment-a", 100, 102, next.Hash())
	if _, err := next.ApplyAccountedPurchases(102, next.Hash(), crypto.HashBytes([]byte("block-102")), []Purchase{duplicatePayment}); err == nil {
		t.Fatal("payment replay accepted")
	}
	// A branch rolled back to its immutable parent may consume the orphaned
	// order again. Neither the order nor payment index leaks from that branch.
	branch, err := s.ApplyAccountedPurchases(101, s.Hash(), crypto.HashBytes([]byte("alternate-101")), []Purchase{p})
	if err != nil || branch.Remaining() != next.Remaining() {
		t.Fatalf("rollback branch failed: %v", err)
	}
	if _, err := next.ApplyAccountedPurchases(101, s.Hash(), h, []Purchase{p}); err == nil {
		t.Fatal("canonical block replay accepted")
	}
}

func TestBatchIsAtomicAndBounded(t *testing.T) {
	f := setup(t)
	s := f.ledger(t)
	h := crypto.HashBytes([]byte("batch"))
	a := f.purchase(t, "a", "pa", 100, 101, s.Hash())
	b := f.purchase(t, "b", "pb", f.c.SaleRemaining, 101, s.Hash())
	if _, err := s.ApplyAccountedPurchases(101, s.Hash(), h, []Purchase{a, b}); err == nil {
		t.Fatal("overspend accepted")
	}
	if s.Seen(a.OrderHash) || s.Sold() != 0 {
		t.Fatal("failed batch mutated parent")
	}
	b.Amount = f.c.SaleRemaining - 100
	b.Signature, _ = f.merchant.Sign(b.Message())
	next, err := s.ApplyAccountedPurchases(101, s.Hash(), h, []Purchase{a, b})
	if err != nil || next.Remaining() != 0 || next.Sold() != f.c.SaleRemaining {
		t.Fatalf("exact exhaustion failed: %v", err)
	}
	c := f.purchase(t, "c", "pc", 1, 102, next.Hash())
	if _, err := next.ApplyAccountedPurchases(102, next.Hash(), crypto.HashBytes([]byte("next")), []Purchase{c}); err == nil {
		t.Fatal("empty allocation spent")
	}
	if _, err := s.ApplyAccountedPurchases(101, s.Hash(), h, []Purchase{a, a}); err == nil {
		t.Fatal("duplicate within batch accepted")
	}
}

func TestPurchaseAuthorizationBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Purchase)
	}{
		{"amount", func(p *Purchase) { p.Amount++ }},
		{"recipient", func(p *Purchase) { p.Recipient = "not-an-address" }},
		{"height", func(p *Purchase) { p.Height++ }},
		{"parent", func(p *Purchase) { p.Parent = crypto.HashBytes([]byte("wrong")) }},
		{"genesis", func(p *Purchase) { p.Genesis = crypto.HashBytes([]byte("other")) }},
		{"order", func(p *Purchase) { p.OrderHash = crypto.HashBytes([]byte("other")) }},
		{"payment", func(p *Purchase) { p.PaymentHash = crypto.HashBytes([]byte("other")) }},
		{"signature", func(p *Purchase) { p.Signature[0] ^= 1 }},
		{"unsigned", func(p *Purchase) { p.Signature = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			s := f.ledger(t)
			p := f.purchase(t, "a", "p", 100, 101, s.Hash())
			tc.change(&p)
			if _, err := s.ApplyAccountedPurchases(101, s.Hash(), crypto.HashBytes([]byte("block")), []Purchase{p}); err == nil {
				t.Fatal("tampered purchase accepted")
			}
		})
	}
}

func TestRecipientMustBeOnAttestedNetwork(t *testing.T) {
	f := setup(t)
	s := f.ledger(t)
	wallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	p := f.purchase(t, "order", "payment", 100, 101, s.Hash())
	p.Recipient = crypto.AddressFromKeys(crypto.TestnetByte, wallet)
	p.Signature, err = f.merchant.Sign(p.Message())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAccountedPurchases(101, s.Hash(), crypto.HashBytes([]byte("block")), []Purchase{p}); err == nil {
		t.Fatal("signed payout to another network accepted")
	}
}

func TestWitnessAuthorityIsCopied(t *testing.T) {
	f := setup(t)
	s := f.ledger(t)
	p := f.purchase(t, "a", "p", 100, 101, s.Hash())
	for i := range f.c.SaleAuthority {
		f.c.SaleAuthority[i] = 0
	}
	if _, err := s.ApplyAccountedPurchases(101, s.Hash(), crypto.HashBytes([]byte("block")), []Purchase{p}); err != nil {
		t.Fatalf("caller mutated internal authority: %v", err)
	}
}
