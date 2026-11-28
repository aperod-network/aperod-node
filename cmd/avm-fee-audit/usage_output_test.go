package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type rejectedUsageWriter struct{ err error }

func (w rejectedUsageWriter) Write([]byte) (int, error) { return 0, w.err }

func TestUsage_WriteFailureIsNotReportedAsSuccessfulHelp(t *testing.T) {
	var positive bytes.Buffer
	if err := run([]string{"--help"}, &positive); err != nil ||
		!strings.Contains(positive.String(), "does not prove API source trust") {
		t.Fatalf("positive help control failed: %v / %s", err, positive.String())
	}
	injected := errors.New("injected usage output failure")
	if err := run([]string{"--help"}, rejectedUsageWriter{injected}); !errors.Is(err, injected) {
		t.Fatalf("help output failure was lost: %v", err)
	}
}
