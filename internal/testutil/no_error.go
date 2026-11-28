package testutil

import "testing"

// NoError marks a test as failed when a fixture operation returns an error.
// It does not call FailNow, so it may also report failures from test goroutines.
// Intentional rejection paths must assert their expected error instead.
func NoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("test fixture operation failed: %v", err)
	}
}
