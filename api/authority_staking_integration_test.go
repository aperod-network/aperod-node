package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestDailyAuditSpansAuthorityRotationWithSignedStakeOperations(t *testing.T) {
	var retired crypto.ValidatorPrivKey
	db, s, _, _ := makeDailyAuditBenchmarkFixtureWithOptions(t, 8, 0, 0, 1, 0, 0, 0, false, true, &retired)
	defer db.Close()
	rec := httptest.NewRecorder()
	s.restLPoDAuditDaily(rec, httptest.NewRequest(http.MethodGet, "/?date=2024-01-02", nil))
	var result struct {
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		Coverage struct {
			Blocks int `json:"blocks"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" || result.Coverage.Blocks != 8 {
		t.Fatalf("full rotated daily audit: %s", rec.Body.String())
	}
	hash, found, err := db.GetCanonicalHash(3)
	if err != nil || !found {
		t.Fatal("missing stake-operation block")
	}
	raw, err := db.GetRawBlock(hash)
	if err != nil {
		t.Fatal(err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	record, err := db.LoadLPoDAuditAtBounded(hash, dailyJobMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	// The signer must be checked, not merely accepted because the identity
	// owns collateral. A different/retired authority must fail replay.
	stakeCount := 0
	for i := range block.Txs {
		if !block.Txs[i].IsStake() {
			continue
		}
		stakeCount++
		action, pub, amount, _, err := core.DecodeStakeExtra(block.Txs[i].Extra)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := retired.Sign(core.StakeSignMsg(action, pub, amount))
		if err != nil {
			t.Fatal(err)
		}
		block.Txs[i].Extra, err = core.EncodeStakeExtra(action, pub, amount, sig)
		if err != nil {
			t.Fatal(err)
		}
	}
	if stakeCount != 2 {
		t.Fatalf("fixture must exercise deposit and withdrawal, got %d", stakeCount)
	}
	if _, _, err := s.verifyDailyRegistryEvidence(context.Background(), &block, record, nil); err == nil || !strings.Contains(err.Error(), "replay canonical stake transactions") {
		t.Fatalf("retired signature must fail stake replay, got %v", err)
	}
}

func TestRotatedLocalStakingUsesCollateralIdentity(t *testing.T) {
	var retired crypto.ValidatorPrivKey
	db, s, _, _ := makeDailyAuditBenchmarkFixtureWithOptions(t, 8, 0, 0, 1, 0, 0, 0, false, true, &retired)
	defer db.Close()
	identity := s.validatorAuthorityFork.Identity
	got, err := s.localValidatorIdentity(true)
	if err != nil || !got.Equals(identity) {
		t.Fatalf("local collateral: %x, %v", got, err)
	}
	rec := httptest.NewRecorder()
	s.restMyValidator(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var result struct {
		PubKey string `json:"pub_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || result.PubKey != identity.Hex() {
		t.Fatalf("identity response: %s", rec.Body.String())
	}
	// Cross-identity checks precede the UTXO preflight. A retained identity
	// reaches preflight, while a signing-key-as-identity request is forbidden.
	registry := core.NewValidatorRegistry()
	s.SetRegistry(registry)
	for _, tc := range []struct {
		pub       crypto.ValidatorPubKey
		forbidden bool
	}{
		{identity, false}, {s.myKey.Public(), true},
	} {
		body := `{"pub_key":"` + tc.pub.Hex() + `","amount_napr":1,"utxo_txhash":"invalid","output_index":0}`
		rec := httptest.NewRecorder()
		s.restAdminStakeDeposit(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if (rec.Code == http.StatusForbidden) != tc.forbidden {
			t.Fatalf("target=%x status=%d body=%s", tc.pub, rec.Code, rec.Body.String())
		}
	}
	retiredKey, err := crypto.NewLockedValidatorKey(retired.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer retiredKey.Destroy()
	s.SetValidatorKey(retiredKey)
	if _, err := s.localValidatorIdentity(true); err == nil {
		t.Fatal("retired collateral signer accepted")
	}
	rec = httptest.NewRecorder()
	s.restMyValidator(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatal("retired local key advertised as active")
	}
}
