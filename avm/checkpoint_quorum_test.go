package avm

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func checkpointWitnessSet(t *testing.T, count int) ([]CheckpointSigner, []crypto.ValidatorPubKey) {
	t.Helper()
	signers := make([]CheckpointSigner, count)
	authorities := make([]crypto.ValidatorPubKey, count)
	for i := range signers {
		privateKey, publicKey, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		signers[i] = privateKey
		authorities[i] = publicKey
	}
	return signers, authorities
}

func collectCheckpointSignatures(t *testing.T, draft *AVMCheckpoint, activationHeight uint64, signers ...CheckpointSigner) []CheckpointWitnessSignature {
	t.Helper()
	signatures := make([]CheckpointWitnessSignature, 0, len(signers))
	for _, signer := range signers {
		signature, err := SignCheckpointDraft(draft, activationHeight, signer)
		if err != nil {
			t.Fatalf("sign checkpoint draft: %v", err)
		}
		signatures = append(signatures, signature)
	}
	return signatures
}

func TestQuorumCheckpointRequiresThresholdAndVerifiesEverySignature(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 1, 0, "")
	tipHash := mustCanonicalHash(t, chain.db, 1)
	draft, err := BuildCheckpointDraftFromCanonical(
		context.Background(), chain.db, chain.activation, 1, tipHash,
	)
	if err != nil {
		t.Fatal(err)
	}

	signers, authorities := checkpointWitnessSet(t, 3)
	partial := collectCheckpointSignatures(t, draft, chain.activation, signers[0])
	if _, err := AttachCheckpointSignatures(draft, partial, chain.activation, authorities, 2); err == nil ||
		!strings.Contains(err.Error(), "insufficient witness signatures") {
		t.Fatalf("one signature unexpectedly met a 2-of-3 threshold: %v", err)
	}

	twoSignatures := collectCheckpointSignatures(t, draft, chain.activation, signers[0], signers[2])
	checkpoint, err := AttachCheckpointSignatures(draft, twoSignatures, chain.activation, authorities, 2)
	if err != nil {
		t.Fatalf("attach 2-of-3 signatures: %v", err)
	}
	if err := VerifyCheckpointQuorum(checkpoint, chain.activation, authorities, 2); err != nil {
		t.Fatalf("verify 2-of-3 checkpoint: %v", err)
	}

	outsider, _ := checkpointWitnessSet(t, 1)
	unauthorized := collectCheckpointSignatures(t, draft, chain.activation, signers[0], outsider[0])
	if _, err := AttachCheckpointSignatures(draft, unauthorized, chain.activation, authorities, 2); err == nil ||
		!strings.Contains(err.Error(), "unauthorized witness") {
		t.Fatalf("unauthorized witness unexpectedly counted: %v", err)
	}

	duplicate := collectCheckpointSignatures(t, draft, chain.activation, signers[0], signers[0])
	if _, err := AttachCheckpointSignatures(draft, duplicate, chain.activation, authorities, 2); err == nil ||
		!strings.Contains(err.Error(), "uniquely key ordered") {
		t.Fatalf("duplicate witness unexpectedly counted: %v", err)
	}

	threeSignatures := collectCheckpointSignatures(t, draft, chain.activation, signers[0], signers[1], signers[2])
	threeSignatures[2].Signature[0] ^= 0x01
	if _, err := AttachCheckpointSignatures(draft, threeSignatures, chain.activation, authorities, 2); err == nil ||
		!strings.Contains(err.Error(), "invalid witness signature") {
		t.Fatalf("invalid extra signature was ignored: %v", err)
	}
}

func TestQuorumCheckpointRejectsUnsignedDraftAtStartup(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 1, 0, "")
	tipHash := mustCanonicalHash(t, chain.db, 1)
	draft, err := BuildCheckpointDraftFromCanonical(
		context.Background(), chain.db, chain.activation, 1, tipHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unsigned-draft.json")
	if err := WriteCheckpointFile(path, draft); err != nil {
		t.Fatal(err)
	}
	_, authorities := checkpointWitnessSet(t, 3)
	if _, err := EnsureCanonicalStateWithQuorumCheckpoint(
		context.Background(), chain.db, chain.activation, 1, tipHash,
		path, authorities, 2, 10,
	); err == nil || !strings.Contains(err.Error(), "insufficient witness signatures") {
		t.Fatalf("unsigned draft authorized startup: %v", err)
	}
}

func TestQuorumCheckpointRejectsTamperingAndV1Downgrade(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 1, 0, "")
	tipHash := mustCanonicalHash(t, chain.db, 1)
	draft, err := BuildCheckpointDraftFromCanonical(
		context.Background(), chain.db, chain.activation, 1, tipHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	signers, authorities := checkpointWitnessSet(t, 2)
	signatures := collectCheckpointSignatures(t, draft, chain.activation, signers...)
	checkpoint, err := AttachCheckpointSignatures(draft, signatures, chain.activation, authorities, 2)
	if err != nil {
		t.Fatal(err)
	}
	tampered := *checkpoint
	tampered.BlockHash = crypto.HashStr("forged tip")
	if err := VerifyCheckpointQuorum(&tampered, chain.activation, authorities, 2); err == nil ||
		!strings.Contains(err.Error(), "invalid witness signature") {
		t.Fatalf("tampered hash was not bound to signatures: %v", err)
	}

	legacy, err := SignCheckpoint(
		chain.activation, 1, checkpoint.GenesisHash, tipHash,
		checkpoint.WriteSetCommitment, checkpoint.Entries, chain.privateKey, chain.authorities,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpointQuorum(legacy, chain.activation, authorities, 2); err == nil ||
		!strings.Contains(err.Error(), "requires version 2") {
		t.Fatalf("v1 operator checkpoint was accepted in quorum mode: %v", err)
	}
}

func TestQuorumCheckpointBindsEverySignedField(t *testing.T) {
	signers, authorities := checkpointWitnessSet(t, 2)
	draft, err := NewQuorumCheckpointDraft(
		1,
		10,
		crypto.HashStr("genesis"),
		crypto.HashStr("block"),
		crypto.HashStr("write-set"),
		[]CheckpointEntry{{Key: []byte("k"), Value: []byte("v")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	signatures := collectCheckpointSignatures(t, draft, 1, signers...)
	valid, err := AttachCheckpointSignatures(draft, signatures, 1, authorities, 2)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name             string
		activationHeight uint64
		mutate           func(*AVMCheckpoint)
	}{
		{name: "activation height", activationHeight: 2, mutate: func(c *AVMCheckpoint) { c.ActivationHeight = 2 }},
		{name: "tip height", activationHeight: 1, mutate: func(c *AVMCheckpoint) { c.Height++ }},
		{name: "genesis hash", activationHeight: 1, mutate: func(c *AVMCheckpoint) { c.GenesisHash = crypto.HashStr("other genesis") }},
		{name: "block hash", activationHeight: 1, mutate: func(c *AVMCheckpoint) { c.BlockHash = crypto.HashStr("other block") }},
		{name: "write-set commitment", activationHeight: 1, mutate: func(c *AVMCheckpoint) { c.WriteSetCommitment = crypto.HashStr("other writes") }},
		{name: "state and commitment", activationHeight: 1, mutate: func(c *AVMCheckpoint) {
			c.Entries[0].Value = []byte("tampered")
			c.StateCommitment, _ = StateSnapshotCommitment(c.Entries)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tampered := cloneSignedCheckpoint(valid)
			tc.mutate(tampered)
			if err := VerifyCheckpointQuorum(tampered, tc.activationHeight, authorities, 2); err == nil ||
				!strings.Contains(err.Error(), "invalid witness signature") {
				t.Fatalf("mutated signed field was not rejected by signatures: %v", err)
			}
		})
	}
}

func TestQuorumCheckpointKeyRotationRequiresNewQuorum(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 1, 0, "")
	tipHash := mustCanonicalHash(t, chain.db, 1)
	draft, err := BuildCheckpointDraftFromCanonical(
		context.Background(), chain.db, chain.activation, 1, tipHash,
	)
	if err != nil {
		t.Fatal(err)
	}

	oldSigners, oldAuthorities := checkpointWitnessSet(t, 2)
	oldSignatures := collectCheckpointSignatures(t, draft, chain.activation, oldSigners...)
	oldCheckpoint, err := AttachCheckpointSignatures(draft, oldSignatures, chain.activation, oldAuthorities, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpointQuorum(oldCheckpoint, chain.activation, oldAuthorities, 2); err != nil {
		t.Fatal(err)
	}

	newSigners, newAuthorities := checkpointWitnessSet(t, 2)
	if err := VerifyCheckpointQuorum(oldCheckpoint, chain.activation, newAuthorities, 2); err == nil {
		t.Fatal("checkpoint signed by retired keys remained trusted after rotation")
	}
	newSignatures := collectCheckpointSignatures(t, draft, chain.activation, newSigners...)
	newCheckpoint, err := AttachCheckpointSignatures(draft, newSignatures, chain.activation, newAuthorities, 2)
	if err != nil {
		t.Fatalf("checkpoint signed by rotated keys: %v", err)
	}
	if err := VerifyCheckpointQuorum(newCheckpoint, chain.activation, newAuthorities, 2); err != nil {
		t.Fatalf("verify rotated-key checkpoint: %v", err)
	}
}

func cloneSignedCheckpoint(checkpoint *AVMCheckpoint) *AVMCheckpoint {
	cloned := *checkpoint
	cloned.Entries = cloneCheckpointEntries(checkpoint.Entries)
	cloned.WitnessSignatures = make([]CheckpointWitnessSignature, len(checkpoint.WitnessSignatures))
	for i, signature := range checkpoint.WitnessSignatures {
		cloned.WitnessSignatures[i] = CheckpointWitnessSignature{
			WitnessPublicKey: append(crypto.ValidatorPubKey(nil), signature.WitnessPublicKey...),
			Signature:        append([]byte(nil), signature.Signature...),
		}
	}
	return &cloned
}

func TestQuorumCheckpointRecoveryAndRestart(t *testing.T) {
	chain := newCheckpointTestChain(t)
	checkpointPath := filepath.Join(t.TempDir(), "avm-checkpoint.json")
	chain.appendDeployAndEmptyBlocks(t, 2, 0, "")
	draft, err := CreateQuorumCheckpointDraftFromCanonicalState(
		chain.db, chain.activation, 2, mustCanonicalHash(t, chain.db, 2),
	)
	if err != nil {
		t.Fatal(err)
	}
	signers, authorities := checkpointWitnessSet(t, 3)
	signatures := collectCheckpointSignatures(t, draft, chain.activation, signers[0], signers[2])
	checkpoint, err := AttachCheckpointSignatures(draft, signatures, chain.activation, authorities, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpointFile(checkpointPath, checkpoint); err != nil {
		t.Fatal(err)
	}

	prev := mustCanonicalHash(t, chain.db, 2)
	for height := uint64(3); height <= 4; height++ {
		prev = chain.appendBlock(t, height, prev, nil)
	}
	rebuilt, err := EnsureCanonicalStateWithQuorumCheckpoint(
		context.Background(), chain.db, chain.activation, 4, prev,
		checkpointPath, authorities, 2, 2,
	)
	if err != nil || rebuilt {
		t.Fatalf("quorum bounded recovery rebuilt=%v err=%v", rebuilt, err)
	}
	storedCheckpoint, found, err := ReadCheckpointFile(checkpointPath)
	if err != nil || !found || storedCheckpoint.Height != 2 || len(storedCheckpoint.WitnessSignatures) != 2 {
		t.Fatalf("recovery unexpectedly rewrote quorum checkpoint: found=%v checkpoint=%+v err=%v", found, storedCheckpoint, err)
	}

	if err := chain.db.Close(); err != nil {
		t.Fatal(err)
	}
	chain.db, err = store.Open(chain.dir)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err = EnsureCanonicalStateWithQuorumCheckpoint(
		context.Background(), chain.db, chain.activation, 4, prev,
		checkpointPath, authorities, 2, 2,
	)
	if err != nil || rebuilt {
		t.Fatalf("restart with quorum checkpoint rebuilt=%v err=%v", rebuilt, err)
	}
}

func TestQuorumCheckpointMissingAboveReplayLimitFailsClosed(t *testing.T) {
	chain := newCheckpointTestChain(t)
	chain.appendDeployAndEmptyBlocks(t, 3, 0, "")
	_, authorities := checkpointWitnessSet(t, 2)
	path := filepath.Join(t.TempDir(), "missing-quorum-checkpoint.json")
	if _, err := EnsureCanonicalStateWithQuorumCheckpoint(
		context.Background(),
		chain.db,
		chain.activation,
		3,
		mustCanonicalHash(t, chain.db, 3),
		path,
		authorities,
		2,
		2,
	); err == nil || !strings.Contains(err.Error(), "checkpoint is missing") {
		t.Fatalf("missing quorum checkpoint did not stop over-limit replay: %v", err)
	}
}
