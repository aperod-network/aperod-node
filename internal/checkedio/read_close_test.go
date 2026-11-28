package checkedio

import (
	"errors"
	"testing"
)

type readCloser struct {
	err   error
	calls int
}

func (c *readCloser) Close() error {
	c.calls++
	return c.err
}

func TestCloseReadResultPreservesReadAndCloseFailures(t *testing.T) {
	readFailure := errors.New("injected read failure")
	closeFailure := errors.New("injected close failure")
	for _, readErr := range []error{nil, readFailure} {
		for _, closeErr := range []error{nil, closeFailure} {
			closer := &readCloser{err: closeErr}
			value, err := readResultFixture(closer, readErr)
			if value != 42 || closer.calls != 1 {
				t.Fatalf("value=%d calls=%d", value, closer.calls)
			}
			if errors.Is(err, readFailure) != (readErr != nil) ||
				errors.Is(err, closeFailure) != (closeErr != nil) {
				t.Fatalf("read=%v close=%v result=%v", readErr, closeErr, err)
			}
			if readErr == nil && closeErr == nil && err != nil {
				t.Fatalf("valid read failed: %v", err)
			}
		}
	}
}

func readResultFixture(closer *readCloser, readErr error) (_ int, retErr error) {
	defer CloseReadResult(closer, &retErr)
	if closer.calls != 0 {
		panic("resource closed before the read completed")
	}
	return 42, readErr
}
