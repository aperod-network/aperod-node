package main

import (
	"errors"
	"testing"
)

type rejectedMaintenancePreflight struct{ err error }

func (w rejectedMaintenancePreflight) Write([]byte) (int, error) { return 0, w.err }

func TestMaintenancePreflight_OutputFailureBlocksDispatch(t *testing.T) {
	injected := errors.New("injected preflight output failure")
	err := maintenancePreflightResolved(rejectedMaintenancePreflight{injected}, "operator-config", t.TempDir(), 0)
	if !errors.Is(err, injected) {
		t.Fatalf("maintenance preflight lost its output failure: %v", err)
	}
}
