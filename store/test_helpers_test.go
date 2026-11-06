package store

import (
	"errors"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
)

func closeStoreTestResource(t testing.TB, resource interface{ Close() error }) {
	t.Helper()
	if err := resource.Close(); err != nil && !errors.Is(err, leveldb.ErrClosed) {
		t.Errorf("close test resource: %v", err)
	}
}
