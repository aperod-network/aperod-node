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

func TestAuthorityForkRotationPreservesHistoricalSignerRanges(t *testing.T) {
	first, identity, firstReplacement := authorityFixture(t)
	secondReplacement, secondPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	rotated := &ValidatorAuthorityFork{
		Version:             2,
		Genesis:             first.Genesis,
		Height:              8,
		Parent:              crypto.HashBytes([]byte("rotation parent")),
		Identity:            append(crypto.ValidatorPubKey(nil), first.Identity...),
		Replacement:         secondPublic,
		RegistrySHA256:      crypto.HashBytes([]byte("rotation registry")),
		LegacyWitnessSHA256: first.LegacyWitnessSHA256,
		Previous:            first.Clone(),
	}
	rotated.Possession = authoritySign(t, secondReplacement, rotated.Message())
	if err := rotated.Validate(); err != nil {
		t.Fatalf("valid sequential rotation rejected: %v", err)
	}

	checks := []struct {
		height uint64
		signer crypto.ValidatorPubKey
	}{
		{height: 4, signer: identity.Public()},
		{height: 5, signer: firstReplacement.Public()},
		{height: 7, signer: firstReplacement.Public()},
		{height: 8, signer: secondPublic},
		{height: 100, signer: secondPublic},
	}
	for _, check := range checks {
		if got := rotated.Authority(identity.Public(), check.height); !got.Equals(check.signer) {
			t.Fatalf("height %d resolved signer %s, want %s", check.height, got.Hex(), check.signer.Hex())
		}
	}
	if got := rotated.LocalIdentity(firstReplacement.Public(), 7); !got.Equals(identity.Public()) {
		t.Fatal("first replacement lost its pre-rotation identity")
	}
	if got := rotated.LocalIdentity(firstReplacement.Public(), 8); got.Equals(identity.Public()) {
		t.Fatal("retired replacement retained authority after second rotation")
	}
	if got := rotated.LocalIdentity(secondPublic, 8); !got.Equals(identity.Public()) {
		t.Fatal("second replacement did not retain the collateral identity")
	}
	thirdReplacement, thirdPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	third := &ValidatorAuthorityFork{
		Version:             2,
		Genesis:             rotated.Genesis,
		Height:              12,
		Parent:              crypto.HashBytes([]byte("third rotation parent")),
		Identity:            append(crypto.ValidatorPubKey(nil), rotated.Identity...),
		Replacement:         thirdPublic,
		RegistrySHA256:      crypto.HashBytes([]byte("third rotation registry")),
		LegacyWitnessSHA256: rotated.LegacyWitnessSHA256,
		Previous:            rotated.Clone(),
	}
	third.Possession = authoritySign(t, thirdReplacement, third.Message())
	if err := third.Validate(); err != nil {
		t.Fatalf("valid third transition rejected: %v", err)
	}
	if !third.Authority(identity.Public(), 11).Equals(secondPublic) ||
		!third.Authority(identity.Public(), 12).Equals(thirdPublic) {
		t.Fatal("third transition did not preserve the preceding signer interval")
	}

	if err := rotated.CheckParent(BlockHeader{Height: 5, PrevHash: first.Parent}); err != nil {
		t.Fatalf("first activation parent rejected: %v", err)
	}
	if err := rotated.CheckParent(BlockHeader{Height: 8, PrevHash: rotated.Parent}); err != nil {
		t.Fatalf("second activation parent rejected: %v", err)
	}
	if err := rotated.CheckParent(BlockHeader{Height: 8, PrevHash: first.Parent}); err == nil {
		t.Fatal("wrong second activation parent accepted")
	}

	tampered := third.Clone()
	tampered.Previous.Replacement[0] ^= 1
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered historical transition accepted")
	}
}

func TestAuthorityForkRejectsInvalidRotationSequence(t *testing.T) {
	first, _, _ := authorityFixture(t)
	next, nextPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_, unrelatedIdentity, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	makeRotation := func(height uint64, identity crypto.ValidatorPubKey, genesis crypto.Hash32) *ValidatorAuthorityFork {
		f := &ValidatorAuthorityFork{
			Version:             2,
			Genesis:             genesis,
			Height:              height,
			Parent:              crypto.HashBytes([]byte("next parent")),
			Identity:            identity,
			Replacement:         nextPublic,
			RegistrySHA256:      crypto.HashBytes([]byte("next registry")),
			LegacyWitnessSHA256: first.LegacyWitnessSHA256,
			Previous:            first.Clone(),
		}
		f.Possession = authoritySign(t, next, f.Message())
		return f
	}
	if makeRotation(first.Height, first.Identity, first.Genesis).Validate() == nil {
		t.Fatal("duplicate activation height accepted")
	}
	if makeRotation(first.Height+1, unrelatedIdentity, first.Genesis).Validate() == nil {
		t.Fatal("changed collateral identity accepted")
	}
	if makeRotation(first.Height+1, first.Identity, crypto.HashBytes([]byte("other genesis"))).Validate() == nil {
		t.Fatal("changed genesis accepted")
	}
	if makeRotation(first.Height+1, first.Identity, first.Genesis).Validate() != nil {
		t.Fatal("valid forward rotation rejected")
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
