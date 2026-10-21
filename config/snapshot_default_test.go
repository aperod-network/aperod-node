package config

import "testing"

func TestDefaultConfigEnablesBoundedPeriodicSnapshots(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Snapshot.PeriodicSnapshotInterval != 500 {
		t.Fatalf("bounded periodic snapshot interval = %d, want 500",
			cfg.Snapshot.PeriodicSnapshotInterval)
	}
	if cfg.Snapshot.ScanCheckpointInterval == 0 {
		t.Fatal("startup scan checkpoints must remain enabled")
	}
}
