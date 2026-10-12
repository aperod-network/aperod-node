package core

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func authorityFixture(t *testing.T) (*ValidatorAuthorityFork, crypto.ValidatorPrivKey, crypto.ValidatorPrivKey) {
	t.Helper()
	old, oldPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	next, nextPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &ValidatorAuthorityFork{Version: 1, Height: 5, Identity: oldPub, Replacement: nextPub,
		Genesis: crypto.HashBytes([]byte("genesis")), Parent: crypto.HashBytes([]byte("parent")),
		RegistrySHA256: crypto.HashBytes([]byte("registry"))}
	f.Possession = authoritySign(t, next, f.Message())
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	return f, old, next
}

func authoritySign(t *testing.T, key crypto.ValidatorPrivKey, msg crypto.Hash32) []byte {
	t.Helper()
	sig, err := key.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func TestAuthorityForkEraAndIdentityBoundaries(t *testing.T) {
	f, old, next := authorityFixture(t)
	_, unrelated, _ := crypto.GenerateValidatorKey()
	msg := crypto.HashBytes([]byte("signed message"))
	for _, height := range []uint64{0, 4, 5, 99} {
		pub := f.Authority(old.Public(), height)
		if pub.Verify(msg, authoritySign(t, old, msg)) != (height < 5) {
			t.Fatalf("old signature at %d", height)
		}
		if pub.Verify(msg, authoritySign(t, next, msg)) != (height >= 5) {
			t.Fatalf("new signature at %d", height)
		}
		if !f.Authority(unrelated, height).Equals(unrelated) {
			t.Fatal("changed unrelated identity")
		}
	}
	if !f.LocalIdentity(next.Public(), 5).Equals(old.Public()) {
		t.Fatal("accounting identity changed")
	}
	if f.LocalIdentity(next.Public(), 4).Equals(old.Public()) {
		t.Fatal("replacement can impersonate before fork")
	}
}

func TestAuthorityForkPossessionBindsAllRuleFields(t *testing.T) {
	f, _, _ := authorityFixture(t)
	mutations := []func(*ValidatorAuthorityFork){
		func(x *ValidatorAuthorityFork) { x.Height++ },
		func(x *ValidatorAuthorityFork) { x.Parent[0] ^= 1 },
		func(x *ValidatorAuthorityFork) { x.Genesis[0] ^= 1 },
		func(x *ValidatorAuthorityFork) { x.RegistrySHA256[0] ^= 1 },
		func(x *ValidatorAuthorityFork) { x.LegacyWitnessSHA256[0] ^= 1 },
		func(x *ValidatorAuthorityFork) { x.Identity[0] ^= 1 },
		func(x *ValidatorAuthorityFork) { x.Replacement[0] ^= 1 },
	}
	for i, mutate := range mutations {
		copy := f.Clone()
		mutate(copy)
		if copy.Validate() == nil {
			t.Fatalf("accepted mutated rule %d", i)
		}
	}
	if f.Validate() != nil {
		t.Fatal("clone mutated original")
	}
}

func TestAuthorityForkBlockSignatureAndParent(t *testing.T) {
	f, old, next := authorityFixture(t)
	for _, height := range []uint64{4, 5, 6} {
		b := Block{Header: BlockHeader{Height: height, Timestamp: 1, PrevHash: f.Parent,
			ValidatorPub: old.Public(), MerkleRoot: MerkleRoot(nil)}}
		b.Header.Signature = authoritySign(t, next, b.Header.Hash())
		if (b.Validate(f.Authority(b.Header.ValidatorPub, height)) == nil) != (height >= 5) {
			t.Fatal("incorrect signature era")
		}
		b.Header.Signature = authoritySign(t, old, b.Header.Hash())
		if (b.Validate(f.Authority(b.Header.ValidatorPub, height)) == nil) != (height < 5) {
			t.Fatal("old key retained future authority")
		}
	}
	h := BlockHeader{Height: 5}
	if f.CheckParent(h) == nil {
		t.Fatal("accepted wrong parent")
	}
	h.PrevHash = f.Parent
	if f.CheckParent(h) != nil {
		t.Fatal("rejected approved parent")
	}
}

func TestAuthorityForkDoesNotRewriteCollateral(t *testing.T) {
	f, old, next := authorityFixture(t)
	reg := NewValidatorRegistry()
	reg.InitFromGenesis([]crypto.ValidatorPubKey{old.Public()}, 1_000_000*BaseUnitsPerAPR)
	initial, _ := reg.GetEntry(old.Public())
	before, _ := json.Marshal(reg.TakeSnapshot())
	for _, height := range []uint64{4, 5, 6} {
		identity := f.LocalIdentity(next.Public(), height)
		if height >= 5 {
			entry, ok := reg.GetEntry(identity)
			if !ok || entry.StakeNAPR != initial.StakeNAPR || entry.StakeAuthNonce != initial.StakeAuthNonce || entry.StakeGeneration != initial.StakeGeneration {
				t.Fatal("collateral state changed")
			}
		}
	}
	after, _ := json.Marshal(reg.TakeSnapshot())
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("registry changed")
	}
}

func TestAuthorityForkWithdrawalV2AllPaths(t *testing.T) {
	f, old, next := authorityFixture(t)
	for _, signer := range []crypto.ValidatorPrivKey{old, next} {
		for _, path := range []string{"admission", "shadow", "apply", "replay"} {
			t.Run(signer.Public().Hex()+"/"+path, func(t *testing.T) {
				r := NewValidatorRegistry()
				r.InitFromGenesis([]crypto.ValidatorPubKey{old.Public()}, 1_000_000*BaseUnitsPerAPR)
				if err := r.ConfigureStakeWithdrawalV2(f.Genesis, 2); err != nil {
					t.Fatal(err)
				}
				if err := r.ConfigureSignatureAuthority(f, 5); err != nil {
					t.Fatal(err)
				}
				entry, _ := r.GetEntry(old.Public())
				a := StakeWithdrawalAuthorizationV2{Action: StakePartialWithdraw, PubKey: old.Public(),
					Amount: BaseUnitsPerAPR, Genesis: f.Genesis, Nonce: entry.StakeAuthNonce + 1,
					Generation: entry.StakeGeneration, ExpiryHeight: 100,
					StakeRef: StakeReferenceV2(f.Genesis, old.Public(), entry.StakeGeneration, entry.StakeNAPR)}
				a.Signature = authoritySign(t, signer, StakeWithdrawalSignMsgV2(a))
				extra, err := EncodeStakeWithdrawalExtraV2(a)
				if err != nil {
					t.Fatal(err)
				}
				tx := Transaction{Version: TxVersionStake, Extra: extra}
				switch path {
				case "admission":
					err = r.ValidateStakeTx(tx)
				case "shadow":
					err = r.ValidateBlockStakeTxs([]Transaction{tx}, 5)
				case "apply":
					_, err = r.ApplyBlockStakeTxs([]Transaction{tx}, 5)
				case "replay":
					err = r.ReplayBlockStakeTxs([]Transaction{tx}, 5)
				}
				if (err == nil) != signer.Public().Equals(next.Public()) {
					t.Fatalf("signature authority not enforced: %v", err)
				}
			})
		}
	}
}
