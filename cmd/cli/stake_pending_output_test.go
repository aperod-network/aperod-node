package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type failedPendingOutput struct{ calls int }

func (w *failedPendingOutput) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("injected terminal failure")
}

func TestPendingStakeOutput_FailureRetainsHashWithoutRetrying(t *testing.T) {
	var diagnostic, positive bytes.Buffer
	log := slog.New(slog.NewTextHandler(&diagnostic, nil))
	hash := strings.Repeat("ab", 32)
	reportPendingStake(&positive, log, hash)
	if !strings.Contains(positive.String(), hash) || diagnostic.Len() != 0 {
		t.Fatalf("successful pending output changed: %s / %s", positive.String(), diagnostic.String())
	}
	failed := &failedPendingOutput{}
	reportPendingStake(failed, log, hash)
	if failed.calls != 1 || !strings.Contains(diagnostic.String(), hash) ||
		!strings.Contains(diagnostic.String(), "do not resubmit") {
		t.Fatalf("pending submission became an ambiguous retry: attempts=%d diagnostic=%s", failed.calls, diagnostic.String())
	}
}
