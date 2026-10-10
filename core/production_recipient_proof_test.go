package core_test

import (
	"bytes"
	"testing"

	"github.com/aperod/aperod/core"
)

// TestProductionRecipientProofWireParity is HEAD-only: the reconstructed
// production baseline predates WithPaymentRecipientProof and must not receive
// this file. It confirms retaining the optional local proof does not alter the
// standard payment's serialized wire representation.
func TestProductionRecipientProofWireParity(t *testing.T) {
	beginProductionCompatTest(t)
	fixture := newProductionCompatFixture(t, 1_000_000_000)

	defaultResult := buildProductionCompatTx(
		t, "payment", fixture.alice, fixture.owned,
		fixture.bobAddr, fixture.aliceAddr, 30_000_000,
	)

	resetProductionCompatEntropy("build-seed:payment")
	proofBuilder := core.NewTxBuilder(
		fixture.alice.Spend.Private,
		fixture.alice.View.Private,
		fixture.alice.Spend.Public,
		fixture.owned,
		1,
	).WithPaymentRecipientProof()
	proofResult, err := proofBuilder.Build(30_000_000, fixture.bobAddr, fixture.aliceAddr)
	if err != nil {
		t.Fatalf("proof-enabled Build: %v", err)
	}

	defaultBytes := mustProductionCompatJSON(t, defaultResult.Tx)
	proofBytes := mustProductionCompatJSON(t, proofResult.Tx)
	if !bytes.Equal(defaultBytes, proofBytes) {
		t.Errorf("opt-in recipient proof changed serialized payment transaction bytes")
	}
	if defaultResult.Tx.Hash() != proofResult.Tx.Hash() {
		t.Errorf("opt-in recipient proof changed payment transaction hash")
	}
	if err := proofResult.VerifyPaymentRecipient(fixture.bobAddr); err != nil {
		t.Errorf("opt-in recipient proof verification: %v", err)
	}

	logProductionCompatTx(t, "payment-default", defaultResult)
	logProductionCompatTx(t, "payment-proof", proofResult)
}
