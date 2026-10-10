package config

import "testing"

func TestDefaultConfigDisablesPeriodicSnapshot(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Snapshot.PeriodicSnapshotInterval != 0 {
		t.Fatalf("periodic snapshots must be opt-in on memory-limited nodes: got %d",
			cfg.Snapshot.PeriodicSnapshotInterval)
	}
	if cfg.Snapshot.ScanCheckpointInterval == 0 {
		t.Fatal("startup scan checkpoints must remain enabled")
	}
}