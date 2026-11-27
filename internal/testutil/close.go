// Package testutil provides checked resource cleanup for isolated node tests.
package testutil

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
)

// Close closes a test resource and reports any failure to the owning test.
// Use this at explicit lifecycle boundaries where successful closure matters.
func Close(t testing.TB, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("close test resource: %v", err)
	}
}

// CleanupClose closes a deferred test resource without hiding I/O failures.
// A resource explicitly closed earlier may report an already-closed sentinel;
// only those documented sentinels are acceptable during final cleanup.
func CleanupClose(t testing.TB, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil &&
		!errors.Is(err, os.ErrClosed) &&
		!errors.Is(err, net.ErrClosed) &&
		!errors.Is(err, leveldb.ErrClosed) {
		t.Errorf("cleanup test resource: %v", err)
	}
}
