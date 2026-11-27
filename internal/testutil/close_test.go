package testutil

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
)

type recordingTest struct {
	testing.TB
	messages []string
}

func (*recordingTest) Helper() {}

func (t *recordingTest) Errorf(format string, args ...any) {
	t.messages = append(t.messages, fmt.Sprintf(format, args...))
}

type countedCloser struct {
	err   error
	calls int
}

func (c *countedCloser) Close() error {
	c.calls++
	return c.err
}

func TestCloseDoesNotHideFailures(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		for _, tc := range []struct {
			name          string
			err           error
			alreadyClosed bool
		}{
			{name: "success"},
			{name: "file already closed", err: os.ErrClosed, alreadyClosed: true},
			{name: "socket already closed", err: net.ErrClosed, alreadyClosed: true},
			{name: "database already closed", err: leveldb.ErrClosed, alreadyClosed: true},
			{name: "wrapped closed", err: fmt.Errorf("outer: %w", os.ErrClosed), alreadyClosed: true},
			{name: "storage failure", err: errors.New("injected I/O failure")},
			{name: "lookalike error", err: errors.New("file already closed")},
		} {
			t.Run(fmt.Sprintf("%s/cleanup=%t", tc.name, cleanup), func(t *testing.T) {
				recorder := &recordingTest{}
				closer := &countedCloser{err: tc.err}
				if cleanup {
					CleanupClose(recorder, closer)
				} else {
					Close(recorder, closer)
				}
				wantFailure := tc.err != nil && (!cleanup || !tc.alreadyClosed)
				if (len(recorder.messages) > 0) != wantFailure {
					t.Fatalf("failure reports=%v, wantFailure=%t", recorder.messages, wantFailure)
				}
				if closer.calls != 1 {
					t.Fatalf("close calls=%d, want 1", closer.calls)
				}
				if wantFailure && !strings.Contains(recorder.messages[0], tc.err.Error()) {
					t.Fatal("the original close failure was lost")
				}
			})
		}
	}
}

func TestCleanupCloseWithAnActualFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "resource")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingTest{}
	Close(recorder, file)
	CleanupClose(recorder, file)
	if len(recorder.messages) != 0 {
		t.Fatalf("valid close and repeated cleanup failed: %v", recorder.messages)
	}
	Close(recorder, file)
	if len(recorder.messages) != 1 {
		t.Fatal("explicit double-close was not reported")
	}
}
