package checkedio

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type loggedCloser struct {
	calls int
	err   error
}

func (c *loggedCloser) Close() error {
	c.calls++
	return c.err
}

func TestLogClose_PreservesResultAndRecordsCleanupFailure(t *testing.T) {
	for _, failure := range []error{nil, errors.New("injected cleanup failure")} {
		var output bytes.Buffer
		log := slog.New(slog.NewTextHandler(&output, nil))
		closer := &loggedCloser{err: failure}
		result := func() int {
			defer LogClose(log, closer, "completed response")
			return 7
		}()
		if result != 7 || closer.calls != 1 {
			t.Fatalf("changed result or retried cleanup: result=%d closes=%d", result, closer.calls)
		}
		if failure != nil && (!strings.Contains(output.String(), failure.Error()) || !strings.Contains(output.String(), "completed response")) {
			t.Fatalf("cleanup failure was lost: %s", output.String())
		}
		if failure == nil && output.Len() != 0 {
			t.Fatalf("successful cleanup reported a failure: %s", output.String())
		}
	}
}
