package api

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/testutil"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func identityEvidenceTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestChainIdentityEvidenceExportsVerifiedGenesisAndOperatorOneOfOne(t *testing.T) {
	db, chain, checkpoint := makeIdentityEvidenceFixture(t, true, false, false)
	defer testutil.CleanupClose(t, db)

	server := NewServer(":0", chain, nil, nil, identityEvidenceTestLogger())
	server.SetStore(db)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/chain/identity-evidence", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var got chainIdentityEvidenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode evidence response: %v", err)
	}
	if got.SchemaVersion != "aperod-chain-identity-evidence-v3" ||
		got.TrustModel != "operator_1_of_1" ||
		got.Verification != "genesis_signature_and_checkpoint_operator_1_of_1_verified_by_serving_node" {
		t.Fatalf("unexpected evidence metadata: %+v", got)
	}
	if !got.Genesis.SignatureValidForHeader || !got.Genesis.SignatureValidForIndex ||
		!got.Genesis.HeaderHashMatchesIndex {
		t.Fatalf("genesis signature/index evidence is invalid: %+v", got.Genesis)
	}
	if got.Checkpoint.Height != checkpoint.Header.Height ||
		got.Checkpoint.HeaderHash != hashHex(checkpoint.Hash()) ||
		!got.Checkpoint.SignatureValidForHeader {
		t.Fatalf("checkpoint evidence does not match fixture: %+v", got.Checkpoint)
	}
	if got.Finality.RequiredVotes != 1 || got.Finality.ValidVotes != 1 ||
		got.Finality.Height != checkpoint.Header.Height ||
		got.Finality.BlockHash != hashHex(checkpoint.Hash()) ||
		len(got.Finality.Committee) != 1 || len(got.Finality.Votes) != 1 {
		t.Fatalf("finality quorum evidence is incorrect: %+v", got.Finality)
	}
	if len(got.Limitations) == 0 {
		t.Fatal("response omitted its verification limitations")
	}
	if got.Finality.RequiredVotes != 1 || got.Finality.ValidVotes != 1 ||
		got.Finality.Committee[0] != got.Checkpoint.SignatureAuthority ||
		got.Finality.Votes[0].Validator != got.Checkpoint.SignatureAuthority {
		t.Fatalf("evidence does not bind finality to the sole checkpoint operator: %+v", got.Finality)
	}
}

func TestMainnetIdentityEvidenceUsesSignedCheckpointWithoutCommitteeQuorum(t *testing.T) {
	db, chain, checkpoint := makeIdentityEvidenceFixture(t, false, false, false)
	defer testutil.CleanupClose(t, db)

	genesis := chain.GetByHeight(0)
	if genesis == nil {
		t.Fatal("fixture genesis is unavailable")
	}
	genesisIndexHash, found, err := db.GetCanonicalHash(0)
	if err != nil || !found {
		t.Fatalf("load genesis index: found=%t err=%v", found, err)
	}
	checkpointIndexHash, found, err := db.GetCanonicalHash(checkpoint.Header.Height)
	if err != nil || !found {
		t.Fatalf("load checkpoint index: found=%t err=%v", found, err)
	}

	server := NewServer(":0", chain, nil, nil, identityEvidenceTestLogger())
	server.SetStore(db)
	server.mainnetIdentity = &mainnetGenesisIdentity{
		genesisHeaderHash:    genesis.Hash(),
		genesisIndexHash:     genesisIndexHash,
		checkpointHeight:     checkpoint.Header.Height,
		checkpointHeaderHash: checkpoint.Hash(),
		checkpointIndexHash:  checkpointIndexHash,
		signedBootstrap: mainnetbootstrap.SignedStatement{
			Statement: mainnetbootstrap.Statement{
				TrustModel:                      mainnetbootstrap.TrustModel,
				GenesisHeaderHash:               hashHex(genesis.Hash()),
				GenesisIndexHash:                hashHex(genesisIndexHash),
				CheckpointHeight:                checkpoint.Header.Height,
				CheckpointHeaderHash:            hashHex(checkpoint.Hash()),
				CheckpointIndexHash:             hashHex(checkpointIndexHash),
				CheckpointValidatorPublicKeyHex: checkpoint.Header.ValidatorPub.Hex(),
				CheckpointSignatureLength:       len(checkpoint.Header.Signature),
			},
		},
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/chain/identity-evidence", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want 200 under the verified operator bootstrap policy", response.Code, response.Body.String())
	}

	var got chainIdentityEvidenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode operator bootstrap evidence: %v", err)
	}
	if got.SchemaVersion != "aperod-chain-identity-evidence-v3" ||
		got.TrustModel != mainnetbootstrap.TrustModel ||
		got.BootstrapAttestation == nil ||
		got.Checkpoint.HeaderHash != hashHex(checkpoint.Hash()) {
		t.Fatalf("operator bootstrap evidence is incomplete: %+v", got)
	}
	if got.Finality.Verified || got.Finality.VerificationStatus != "not_verified_by_operator_bootstrap_attestation" ||
		got.Finality.ValidVotes != 0 || len(got.Finality.Committee) != 0 {
		t.Fatalf("unverified finality was presented as quorum evidence: %+v", got.Finality)
	}
}

func TestChainIdentityEvidenceUsesCollateralIdentityAcrossSignerRotation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	defer testutil.CleanupClose(t, db)

	identityKey, identity, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("generate collateral identity: %v", err)
	}
	replacementKey, replacement, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("generate replacement signer: %v", err)
	}

	makeBlock := func(height uint64, parent crypto.Hash32, signer crypto.ValidatorPrivKey) *core.Block {
		block := &core.Block{Header: core.BlockHeader{
			Height:       height,
			PrevHash:     parent,
			MerkleRoot:   core.MerkleRoot(nil),
			Timestamp:    int64(height+1) * 1_000_000_000,
			ValidatorPub: identity,
		}}
		if err := block.Header.Sign(signer); err != nil {
			t.Fatalf("sign block at height %d: %v", height, err)
		}
		return block
	}
	putBlock := func(block *core.Block) {
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatalf("marshal block at height %d: %v", block.Header.Height, err)
		}
		if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
			t.Fatalf("store block at height %d: %v", block.Header.Height, err)
		}
	}

	genesis := makeBlock(0, crypto.Hash32{}, identityKey)
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("set genesis: %v", err)
	}
	putBlock(genesis)

	parent := makeBlock(1, genesis.Hash(), identityKey)
	if err := chain.AddBlock(parent); err != nil {
		t.Fatalf("add parent block: %v", err)
	}
	putBlock(parent)

	fork := &core.ValidatorAuthorityFork{
		Version:        1,
		Genesis:        genesis.Hash(),
		Height:         2,
		Parent:         parent.Hash(),
		Identity:       identity,
		Replacement:    replacement,
		RegistrySHA256: crypto.HashBytes([]byte("registry")),
	}
	fork.Possession, err = replacementKey.Sign(fork.Message())
	if err != nil {
		t.Fatalf("sign authority-fork possession: %v", err)
	}

	checkpoint := makeBlock(2, parent.Hash(), replacementKey)
	if err := chain.AddBlock(checkpoint); err != nil {
		t.Fatalf("add checkpoint: %v", err)
	}
	putBlock(checkpoint)
	committee := store.FinalityCommitteeSnapshot{
		Version:         1,
		Epoch:           checkpoint.Header.Height / core.EpochLength,
		Validators:      []crypto.ValidatorPubKey{identity},
		BFTThresholdPPM: 667_000,
	}
	if err := db.SaveFinalityCommitteeSnapshot(committee); err != nil {
		t.Fatalf("save committee snapshot: %v", err)
	}
	voteMessage := crypto.FinalityVoteMessage(
		crypto.FinalityVoteVersionHeightBound, checkpoint.Header.Height, checkpoint.Hash(),
	)
	voteSignature, err := replacementKey.Sign(voteMessage)
	if err != nil {
		t.Fatalf("sign finality vote: %v", err)
	}
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version:   1,
		Height:    checkpoint.Header.Height,
		BlockHash: checkpoint.Hash(),
		Votes: []store.FinalityVote{{
			Validator:   identity,
			Signature:   voteSignature,
			VoteVersion: crypto.FinalityVoteVersionHeightBound,
		}},
	}); err != nil {
		t.Fatalf("save finality certificate: %v", err)
	}

	server := NewServer(":0", chain, nil, nil, identityEvidenceTestLogger())
	server.SetStore(db)
	if err := server.SetAuditValidatorAuthorityFork(fork); err != nil {
		t.Fatalf("set authority fork: %v", err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/chain/identity-evidence", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var evidence chainIdentityEvidenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &evidence); err != nil {
		t.Fatalf("decode identity evidence: %v", err)
	}
	if evidence.Checkpoint.ValidatorPub != identity.Hex() ||
		evidence.Checkpoint.SignatureAuthority != replacement.Hex() ||
		len(evidence.Finality.Committee) != 1 ||
		evidence.Finality.Committee[0] != identity.Hex() ||
		len(evidence.Finality.Votes) != 1 ||
		evidence.Finality.Votes[0].Validator != identity.Hex() {
		t.Fatalf("identity and rotated signing authority were conflated: %+v", evidence)
	}
}

func TestChainIdentityEvidenceFailsClosedWithoutCertificate(t *testing.T) {
	db, chain, _ := makeIdentityEvidenceFixture(t, false, false, false)
	defer testutil.CleanupClose(t, db)

	server := NewServer(":0", chain, nil, nil, identityEvidenceTestLogger())
	server.SetStore(db)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/chain/identity-evidence", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
}

func TestChainIdentityEvidenceServesDiskBackedLegacyAndHeightBoundVotes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version uint8
		want    uint8
	}{
		{"implicit legacy", 0, crypto.FinalityVoteVersionLegacy},
		{"explicit legacy", crypto.FinalityVoteVersionLegacy, crypto.FinalityVoteVersionLegacy},
		{"height bound", crypto.FinalityVoteVersionHeightBound, crypto.FinalityVoteVersionHeightBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "chain.db")
			db, _, checkpoint := makeIdentityEvidenceFixtureAt(
				t, dbPath, true, false, true, tc.version)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := store.OpenReadOnly(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer testutil.CleanupClose(t, db)
			server := NewServer(":0", nil, nil, nil, identityEvidenceTestLogger())
			server.SetStore(db)
			listener := httptest.NewServer(server)
			defer listener.Close()

			response, err := listener.Client().Get(listener.URL + "/api/v1/chain/identity-evidence")
			if err != nil {
				t.Fatal(err)
			}
			defer testutil.CleanupClose(t, response.Body)
			if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected HTTP result: %s, cache-control %q",
					response.Status, response.Header.Get("Cache-Control"))
			}
			var got chainIdentityEvidenceResponse
			if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Checkpoint.CanonicalIndexHash != hashHex(checkpoint.Hash()) ||
				got.Finality.BlockHash != got.Checkpoint.CanonicalIndexHash ||
				got.Finality.Height != checkpoint.Header.Height ||
				got.Genesis.SignatureValidForHeader || !got.Genesis.SignatureValidForIndex ||
				len(got.Finality.Votes) != 1 || got.Finality.ValidVotes != 1 ||
				got.Finality.RequiredVotes != 1 {
				t.Fatalf("unexpected evidence: %+v", got)
			}
			vote := got.Finality.Votes[0]
			sig, err := hex.DecodeString(vote.Signature)
			if err != nil || vote.VoteVersion != tc.want ||
				vote.Validator != checkpoint.Header.ValidatorPub.Hex() ||
				!checkpoint.Header.ValidatorPub.Verify(
					crypto.FinalityVoteMessage(tc.want, checkpoint.Header.Height, checkpoint.Hash()), sig) {
				t.Fatalf("persisted signature or vote version changed: version %d, error %v", vote.VoteVersion, err)
			}
		})
	}
}

func TestChainIdentityEvidenceRejectsBadQuorumSignature(t *testing.T) {
	db, chain, _ := makeIdentityEvidenceFixture(t, true, true, false)
	defer testutil.CleanupClose(t, db)

	server := NewServer(":0", chain, nil, nil, identityEvidenceTestLogger())
	server.SetStore(db)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/chain/identity-evidence", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
}

func TestChainIdentityEvidenceReportsLegacyGenesisIndexHashSeparately(t *testing.T) {
	db, chain, _ := makeIdentityEvidenceFixture(t, true, false, true)
	defer testutil.CleanupClose(t, db)

	server := NewServer(":0", chain, nil, nil, identityEvidenceTestLogger())
	server.SetStore(db)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/chain/identity-evidence", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var got chainIdentityEvidenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode evidence response: %v", err)
	}
	if got.Genesis.HeaderHashMatchesIndex ||
		got.Genesis.SignatureValidForHeader ||
		!got.Genesis.SignatureValidForIndex {
		t.Fatalf("legacy index/hash distinction was not preserved: %+v", got.Genesis)
	}
}

func TestChainIdentityEvidenceRejectsNonGET(t *testing.T) {
	server := NewServer(":0", nil, nil, nil, identityEvidenceTestLogger())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/chain/identity-evidence", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestVerifyOperatorOneOfOne(t *testing.T) {
	_, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	operator := pub.Hex()
	valid := identityFinalityEvidence{
		RequiredVotes: 1,
		ValidVotes:    1,
		Committee:     []string{operator},
		Votes:         []identityFinalityVote{{Validator: operator}},
	}
	if err := verifyOperatorOneOfOne(valid, pub); err != nil {
		t.Fatalf("valid single-operator evidence rejected: %v", err)
	}

	tests := []struct {
		name     string
		evidence identityFinalityEvidence
	}{
		{
			name: "multiple committee members",
			evidence: identityFinalityEvidence{
				RequiredVotes: 1,
				ValidVotes:    1,
				Committee:     []string{operator, operator},
				Votes:         []identityFinalityVote{{Validator: operator}},
			},
		},
		{
			name: "different vote signer",
			evidence: identityFinalityEvidence{
				RequiredVotes: 1,
				ValidVotes:    1,
				Committee:     []string{operator},
				Votes:         []identityFinalityVote{{Validator: "different"}},
			},
		},
		{
			name: "missing vote",
			evidence: identityFinalityEvidence{
				RequiredVotes: 1,
				ValidVotes:    0,
				Committee:     []string{operator},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := verifyOperatorOneOfOne(tt.evidence, pub); err == nil {
				t.Fatal("invalid 1-of-1 evidence was accepted")
			}
		})
	}

	_, otherPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyOperatorOneOfOne(valid, otherPub); err == nil {
		t.Fatal("evidence signed by a different operator was accepted")
	}
}

func makeIdentityEvidenceFixture(t *testing.T, withCertificate, badVote, legacyGenesisIndex bool) (*store.DB, *core.Chain, *core.Block) {
	t.Helper()
	return makeIdentityEvidenceFixtureAt(t, filepath.Join(t.TempDir(), "chain.db"),
		withCertificate, badVote, legacyGenesisIndex, crypto.FinalityVoteVersionHeightBound)
}

func makeIdentityEvidenceFixtureAt(t *testing.T, dbPath string, withCertificate, badVote, legacyGenesisIndex bool, voteVersion uint8) (*store.DB, *core.Chain, *core.Block) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	fail := func(format string, args ...any) {
		testutil.Close(t, db)
		t.Fatalf(format, args...)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		fail("generate validator key: %v", err)
	}

	makeBlock := func(height uint64, parent crypto.Hash32) *core.Block {
		block := &core.Block{Header: core.BlockHeader{
			Height:       height,
			PrevHash:     parent,
			MerkleRoot:   core.MerkleRoot(nil),
			Timestamp:    int64(height+1) * 1_000_000_000,
			ValidatorPub: pub,
		}}
		if err := block.Header.Sign(priv); err != nil {
			fail("sign block %d: %v", height, err)
		}
		return block
	}
	putBlock := func(block *core.Block) {
		raw, err := json.Marshal(block)
		if err != nil {
			fail("marshal block %d: %v", block.Header.Height, err)
		}
		if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
			fail("store block %d: %v", block.Header.Height, err)
		}
	}

	genesis := makeBlock(0, crypto.Hash32{})
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		fail("set genesis: %v", err)
	}
	putBlock(genesis)
	if legacyGenesisIndex {
		legacyIndexHash := crypto.HashBytes([]byte("test legacy genesis index"))
		signature, err := priv.Sign(legacyIndexHash)
		if err != nil {
			fail("sign legacy genesis index: %v", err)
		}
		genesis.Header.Signature = signature
		legacyRaw, err := json.Marshal(genesis)
		if err != nil {
			fail("marshal legacy-index genesis: %v", err)
		}
		if err := db.PutRawBlock(legacyIndexHash, 0, legacyRaw); err != nil {
			fail("store legacy-index genesis: %v", err)
		}
	}

	checkpoint := makeBlock(1, genesis.Hash())
	if err := chain.AddBlock(checkpoint); err != nil {
		fail("add checkpoint: %v", err)
	}
	putBlock(checkpoint)

	if withCertificate {
		committee := store.FinalityCommitteeSnapshot{
			Version:         1,
			Epoch:           checkpoint.Header.Height / core.EpochLength,
			Validators:      []crypto.ValidatorPubKey{pub},
			BFTThresholdPPM: 667_000,
		}
		if err := db.SaveFinalityCommitteeSnapshot(committee); err != nil {
			fail("save committee snapshot: %v", err)
		}
		voteMessage := crypto.FinalityVoteMessage(voteVersion, checkpoint.Header.Height, checkpoint.Hash())
		signature, err := priv.Sign(voteMessage)
		if err != nil {
			fail("sign finality vote: %v", err)
		}
		if badVote {
			signature[0] ^= 0xff
		}
		certificate := store.FinalityCertificate{
			Version:   1,
			Height:    checkpoint.Header.Height,
			BlockHash: checkpoint.Hash(),
			Votes: []store.FinalityVote{{
				Validator:   pub,
				Signature:   signature,
				VoteVersion: voteVersion,
			}},
		}
		if err := db.SaveFinalityCertificate(certificate); err != nil {
			fail("save finality certificate: %v", err)
		}
	}
	return db, chain, checkpoint
}
