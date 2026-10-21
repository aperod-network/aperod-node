package main

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
)

func TestEndpointSignaturesDoNotAuthenticateCommittee(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	blockHash := crypto.HashStr("synthetic-canonical-endpoint")
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), blockHash.Bytes())
	signature, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Verify(message, signature) {
		t.Fatal("positive endpoint signature control failed")
	}
	original := store.FinalityCertificate{Version: 1, Height: 9, BlockHash: blockHash,
		Votes: []store.FinalityVote{{Validator: pub, Signature: signature}}}
	// A real valid sale authorization by the very same key is still scoped to
	// sale, not an independent source of historical consensus membership.
	checkpoint := sale.Checkpoint{Genesis: crypto.HashStr("synthetic-genesis"), Network: crypto.DevnetByte,
		Height: 10, Parent: blockHash, SnapshotRoot: crypto.HashStr("synthetic-sale"), SaleRemaining: 100,
		SaleAuthority: pub}
	saleSignature, err := priv.Sign(checkpoint.Message())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Attestations = []sale.Attestation{{Validator: pub, Signature: saleSignature}}
	if err := checkpoint.Verify(checkpoint.Genesis, blockHash, 10, []crypto.ValidatorPubKey{pub}); err != nil {
		t.Fatal(err)
	}
	migration := store.LPoDMigration{Version: 1, PositionLifecycleVersion: 1, Height: 10,
		Genesis: checkpoint.Genesis, TrustedValidators: []crypto.ValidatorPubKey{pub}}
	migration.ReconciliationRoot = migration.Root()
	migrationSignature, err := priv.Sign(migration.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	migration.Attestations = []store.LPoDAttestation{{Validator: pub, Signature: migrationSignature}}
	if err := migration.VerifyAuthorization(); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"self-signed-committee", "operator-snapshot", "sale-witness", "migration-witness", "all-claimed-members-sign"} {
		t.Run(variant, func(t *testing.T) {
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			document["committee_authenticated"] = true
			document["quorum_authenticated"] = true
			document["production_ready"] = true
			document["trust"] = "authenticated"
			document["committee"] = []crypto.ValidatorPubKey{pub}
			document[variant] = checkpoint
			if variant == "migration-witness" {
				document[variant] = migration
			}
			raw, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			var decoded store.FinalityCertificate
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if variant == "all-claimed-members-sign" {
				for i := 0; i < 2; i++ {
					signer, member, err := crypto.GenerateValidatorKey()
					if err != nil {
						t.Fatal(err)
					}
					sig, err := signer.Sign(message)
					if err != nil {
						t.Fatal(err)
					}
					decoded.Votes = append(decoded.Votes, store.FinalityVote{Validator: member, Signature: sig})
				}
			}
			result, err := endpointCertificateEvidence(&decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !result.SignatureValidVoteKeys[pub.Hex()] || len(result.SignatureValidVoteKeys) != len(decoded.Votes) || result.CommitteeAuthenticated || result.QuorumAuthenticated || result.ProductionReady ||
				result.Trust != archiveConsistencyTrust {
				t.Fatalf("scoped/untrusted evidence promoted authority: %#v", result)
			}
		})
	}
	t.Run("scoped-signature-is-not-endpoint-vote", func(t *testing.T) {
		invalid := original
		invalid.Votes = []store.FinalityVote{{Validator: pub, Signature: saleSignature}}
		if _, err := endpointCertificateEvidence(&invalid); err == nil {
			t.Fatal("sale signature accepted as finality vote")
		}
	})
	t.Run("duplicate-valid-vote", func(t *testing.T) {
		duplicate := original
		duplicate.Votes = append(duplicate.Votes, duplicate.Votes[0])
		if _, err := endpointCertificateEvidence(&duplicate); err == nil {
			t.Fatal("duplicate vote accepted")
		}
	})
	for _, variant := range []string{"nil", "empty", "bad-key", "wrong-version", "wrong-message"} {
		t.Run(variant, func(t *testing.T) {
			value := original
			value.Votes = append([]store.FinalityVote(nil), original.Votes...)
			candidate := &value
			switch variant {
			case "nil":
				candidate = nil
			case "empty":
				value.Votes = nil
			case "bad-key":
				value.Votes[0].Validator = crypto.ValidatorPubKey{1}
			case "wrong-version":
				value.Version = 2
			case "wrong-message":
				value.BlockHash = crypto.HashStr("different-canonical-endpoint")
			}
			result, err := endpointCertificateEvidence(candidate)
			if err == nil || result.CommitteeAuthenticated || result.QuorumAuthenticated || result.ProductionReady {
				t.Fatalf("invalid certificate accepted: %#v %v", result, err)
			}
		})
	}
}
