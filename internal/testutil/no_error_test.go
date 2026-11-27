package testutil

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestNoErrorReportsEveryFailure(t *testing.T) {
	for _, failure := range []error{nil, net.ErrClosed, errors.New("injected fixture failure")} {
		recorder := &recordingTest{}
		NoError(recorder, failure)
		if failure == nil {
			if len(recorder.messages) != 0 {
				t.Fatalf("successful operation failed: %v", recorder.messages)
			}
		} else if len(recorder.messages) != 1 || !strings.Contains(recorder.messages[0], failure.Error()) {
			t.Fatalf("fixture failure was hidden: %v", recorder.messages)
		}
	}
}
