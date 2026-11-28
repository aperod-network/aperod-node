package main

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/testutil"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func TestMainnetAnchorIndexes(t *testing.T) {
	genesis, checkpoint, next, wrong := crypto.Hash32{1}, crypto.Hash32{2}, crypto.Hash32{3}, crypto.Hash32{4}
	statement := mainnetbootstrap.Statement{
		GenesisIndexHash:    hex.EncodeToString(genesis[:]),
		CheckpointHeight:    20,
		CheckpointIndexHash: hex.EncodeToString(checkpoint[:]),
	}
	for _, tt := range []struct {
		name   string
		index  map[uint64]crypto.Hash32
		height uint64
		tip    crypto.Hash32
		reject string
	}{
		{"first boot", map[uint64]crypto.Hash32{0: genesis, 20: checkpoint}, 20, checkpoint, ""},
		{"advanced tip without anchor bodies", map[uint64]crypto.Hash32{0: genesis, 20: checkpoint}, 21, next, ""},
		{"wrong genesis", map[uint64]crypto.Hash32{0: wrong, 20: checkpoint}, 20, checkpoint, "signed genesis"},
		{"wrong checkpoint", map[uint64]crypto.Hash32{0: genesis, 20: wrong}, 21, next, "signed checkpoint"},
		{"missing checkpoint", map[uint64]crypto.Hash32{0: genesis}, 21, next, "signed checkpoint"},
		{"old tip", map[uint64]crypto.Hash32{0: genesis, 20: checkpoint}, 19, wrong, "below the signed checkpoint"},
		{"wrong tip at checkpoint", map[uint64]crypto.Hash32{0: genesis, 20: checkpoint}, 20, wrong, "tip pointer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer testutil.CleanupClose(t, db)
			for h, hash := range tt.index {
				if err := db.RepairHeightIndex(h, hash); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.PutTip(tt.tip, tt.height); err != nil {
				t.Fatal(err)
			}
			err = checkMainnetAnchorIndexes(db, statement)
			if tt.reject == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.reject) {
				t.Fatalf("expected %q rejection, got %v", tt.reject, err)
			}
		})
	}
}
