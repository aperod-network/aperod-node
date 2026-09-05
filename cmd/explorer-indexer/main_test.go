package main

import (
	"testing"
)

func TestBackfillFromHeightFromEnv(t *testing.T) {
	t.Setenv("BACKFILL_FROM_HEIGHT", "0")
	height, err := backfillFromHeightFromEnv()
	if err != nil {
		t.Fatalf("parse zero height: %v", err)
	}
	if height == nil || *height != 0 {
		t.Fatalf("height = %v, want pointer to 0", height)
	}

	t.Setenv("BACKFILL_FROM_HEIGHT", "1760000")
	height, err = backfillFromHeightFromEnv()
	if err != nil || height == nil || *height != 1760000 {
		t.Fatalf("height = %v, %v; want 1760000, nil", height, err)
	}
}

func TestBackfillFromHeightFromEnvRejectsInvalidValue(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "not-a-height"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BACKFILL_FROM_HEIGHT", value)
			if _, err := backfillFromHeightFromEnv(); err == nil {
				t.Fatalf("BACKFILL_FROM_HEIGHT=%q unexpectedly accepted", value)
			}
		})
	}
}
