package main

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
)

func TestVerifyBootstrapAVMBindsWitnessAndCanonicalIndexes(t *testing.T) {
	key, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	const activation, height = 5, 10
	genesis, tip := crypto.HashStr("genesis index"), crypto.HashStr("tip index")
	s := mainnetbootstrap.Statement{
		GenesisHeaderHash:    hex.EncodeToString(make([]byte, 32)),
		GenesisIndexHash:     hex.EncodeToString(genesis[:]),
		CheckpointHeight:     height,
		CheckpointHeaderHash: hex.EncodeToString(make([]byte, 32)),
		CheckpointIndexHash:  hex.EncodeToString(tip[:]),
	}
	draft, err := avm.NewQuorumCheckpointDraft(activation, height, genesis, tip, crypto.HashStr("writes"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := avm.SignCheckpointDraft(draft, activation, key)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := avm.AttachCheckpointSignatures(draft, []avm.CheckpointWitnessSignature{sig}, activation, []crypto.ValidatorPubKey{pub}, 1)
	if err != nil {
		t.Fatal(err)
	}
	pin := hex.EncodeToString(pub)
	if err := verifyBootstrapAVM(s, signed, activation, pin); err != nil {
		t.Fatal(err)
	}
	if err := verifyBootstrapAVM(s, draft, activation, pin); err == nil {
		t.Fatal("accepted unsigned draft")
	}
	if err := verifyBootstrapAVM(s, signed, activation+1, pin); err == nil {
		t.Fatal("accepted different activation height")
	}
	if err := verifyBootstrapAVM(s, signed, activation, mainnetbootstrap.BootstrapSignerPublicKeyHex); err == nil {
		t.Fatal("accepted an unauthorized witness")
	}
	for _, alter := range []func(*mainnetbootstrap.Statement){
		func(v *mainnetbootstrap.Statement) { v.GenesisIndexHash = v.GenesisHeaderHash },
		func(v *mainnetbootstrap.Statement) { v.CheckpointIndexHash = v.CheckpointHeaderHash },
		func(v *mainnetbootstrap.Statement) { v.CheckpointHeight++ },
	} {
		wrong := s
		alter(&wrong)
		if err := verifyBootstrapAVM(wrong, signed, activation, pin); err == nil {
			t.Fatal("accepted checkpoint from a different bootstrap anchor")
		}
	}
}
