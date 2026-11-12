package consensus

import (
	"io"
	"log/slog"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func TestReconcileRewardAddressAdminMintIntents(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	defer db.Close()

	const rewardAddress = "aproRewardAddressForStartupReconciliation"
	records := map[string]store.AdminMintRecord{
		"queued-reward": {
			State: "intent", Address: rewardAddress, AmountNAPR: 500_000_000,
		},
		"queued-other": {
			State: "intent", Address: "aproDifferentDestination", AmountNAPR: 250_000_000,
		},
		"completed-reward": {
			State: "completed", Address: rewardAddress, AmountNAPR: 100_000_000,
		},
		"prepared-reward": {
			State: "prepared", Address: rewardAddress, AmountNAPR: 125_000_000,
		},
	}
	for key, record := range records {
		if err := db.StoreAdminMintRecord(key, record); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}

	engine := NewEngine(
		Config{RewardAddress: rewardAddress, Store: db},
		core.NewChain(),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	failed, err := engine.ReconcileRewardAddressAdminMintIntents()
	if err != nil {
		t.Fatalf("reconcile reward-address intents: %v", err)
	}
	if failed != 1 {
		t.Fatalf("reconciled %d intents, want 1", failed)
	}

	got, err := db.ListAdminMintRecords()
	if err != nil {
		t.Fatalf("list reconciled records: %v", err)
	}
	byKey := make(map[string]store.AdminMintRecord, len(got))
	for _, entry := range got {
		byKey[entry.IdempotencyKey] = entry.Record
	}
	if record := byKey["queued-reward"]; record.State != "failed" ||
		record.Failure != validatorRewardAddressMintFailure {
		t.Fatalf("reward intent was not clearly failed: %+v", record)
	}
	for _, key := range []string{"queued-other", "completed-reward", "prepared-reward"} {
		if byKey[key].State != records[key].State {
			t.Fatalf("%s changed state to %q, want %q", key, byKey[key].State, records[key].State)
		}
	}
}
