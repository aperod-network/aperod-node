package avm

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type observedRuntimeClose struct {
	err   error
	ctx   context.Context
	calls int
}

func (c *observedRuntimeClose) Close(ctx context.Context) error {
	c.ctx = ctx
	c.calls++
	return c.err
}

func TestRuntimeCleanupReportsFailureWithoutChangingComputedEffects(t *testing.T) {
	for _, failure := range []error{nil, errors.New("injected cache cleanup failure")} {
		closer := &observedRuntimeClose{err: failure}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, nil))
		result := func() string {
			defer closeRuntimeResource(ctx, closer, "test cache", logger)
			if closer.calls != 0 {
				t.Fatal("cleanup executed before the result was computed")
			}
			return "prepared effects"
		}()
		if result != "prepared effects" || closer.calls != 1 || closer.ctx != ctx {
			t.Fatalf("cleanup changed the result or context: result=%q calls=%d", result, closer.calls)
		}
		if failure == nil {
			if output.Len() != 0 {
				t.Fatalf("successful cleanup logged a failure: %s", output.String())
			}
		} else if !strings.Contains(output.String(), failure.Error()) ||
			!strings.Contains(output.String(), "test cache") {
			t.Fatalf("cleanup failure was not reported: %s", output.String())
		}
	}
}
