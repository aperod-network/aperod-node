package mainnetbootstrap

import (
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestHistoricalCommitteeOperatorScope(t *testing.T) {
	if BootstrapSignerPublicKeyHex == HistoricalCommitteeOperatorPublicKeyHex {
		t.Fatal("new bootstrap signatures must use a dedicated operator key")
	}
	if HistoricalCommitteeOperatorPublicKeyHex != GenesisSignerPublicKeyHex {
		t.Fatal("historical committee signer must remain pinned to the genesis identity")
	}
	draft := HistoricalCommitteeDraft()
	operator := draft.Approvals[0].Validator
	if operator.Hex() != HistoricalCommitteeOperatorPublicKeyHex || draft.CertificateHeight != 4_024_017 ||
		draft.CertificateBlockHash != draft.ApprovalBlockHash || draft.Version != store.FinalityCommitteeOperatorVersion {
		t.Fatal("unexpected Mainnet operator scope")
	}
	message, err := draft.SigningMessage()
	if err != nil {
		t.Fatal(err)
	}
	legacy := draft
	legacy.Version = store.FinalityCommitteeMigrationVersion
	legacyMessage, err := legacy.SigningMessage()
	if err != nil || message == legacyMessage {
		t.Fatalf("operator approval must have a separate signing domain: %v", err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	draft.Approvals[0].Signature = sig
	if VerifyHistoricalCommitteeOperator(draft, []crypto.ValidatorPubKey{operator}) == nil ||
		VerifyHistoricalCommitteeOperator(draft, []crypto.ValidatorPubKey{pub}) == nil {
		t.Fatal("an unrelated key cannot sign a Mainnet operator approval")
	}
	draft.Approvals[0].Validator = pub
	if VerifyHistoricalCommitteeOperator(draft, []crypto.ValidatorPubKey{pub}) == nil {
		t.Fatal("operator pin cannot be overridden by node config")
	}
}
