package api

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type responseFailureWriter struct {
	header       http.Header
	status       int
	headerWrites int
	bodyWrites   int
}

func (w *responseFailureWriter) Header() http.Header { return w.header }
func (w *responseFailureWriter) WriteHeader(status int) {
	w.status = status
	w.headerWrites++
}
func (w *responseFailureWriter) Write([]byte) (int, error) {
	w.bodyWrites++
	return 0, io.ErrClosedPipe
}

func TestJSONResponse_PreservesWriteErrorWithoutSecondResponse(t *testing.T) {
	control := httptest.NewRecorder()
	if err := writeJSONResponse(control, http.StatusAccepted, map[string]bool{"accepted": true}); err != nil {
		t.Fatal(err)
	}
	if control.Code != http.StatusAccepted || control.Body.String() != "{\"accepted\":true}\n" {
		t.Fatalf("positive response changed: status=%d body=%q", control.Code, control.Body.String())
	}
	writer := &responseFailureWriter{header: make(http.Header)}
	err := writeJSONResponse(writer, http.StatusAccepted, map[string]bool{"accepted": true})
	if !errors.Is(err, io.ErrClosedPipe) || writer.headerWrites != 1 || writer.bodyWrites != 1 || writer.status != http.StatusAccepted {
		t.Fatalf("write failure was lost or retried: err=%v writer=%+v", err, writer)
	}
	writer = &responseFailureWriter{header: make(http.Header)}
	writeJSON(writer, http.StatusAccepted, map[string]bool{"accepted": true})
	if writer.headerWrites != 1 || writer.bodyWrites != 1 || writer.status != http.StatusAccepted {
		t.Fatalf("logging wrapper retried the response: %+v", writer)
	}
}

func TestJSONResponse_EncodingFailureIsNotReplacedWithAnotherStatus(t *testing.T) {
	writer := &responseFailureWriter{header: make(http.Header)}
	err := writeJSONResponse(writer, http.StatusAccepted, make(chan int))
	if err == nil || writer.headerWrites != 1 || writer.bodyWrites != 0 || writer.status != http.StatusAccepted {
		t.Fatalf("encoding error was hidden or a second status was attempted: err=%v writer=%+v", err, writer)
	}
}

type wsCleanupFailure struct {
	calls int
	err   error
}

func (c *wsCleanupFailure) Close() error {
	c.calls++
	return c.err
}

func TestWebSocketCleanup_RecordsFailureWithoutRetry(t *testing.T) {
	injected := errors.New("injected websocket cleanup failure")
	var output bytes.Buffer
	hub := &Hub{log: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	closer := &wsCleanupFailure{err: injected}
	hub.closeConnection(closer)
	if closer.calls != 1 || !strings.Contains(output.String(), injected.Error()) {
		t.Fatalf("close failure was lost or retried: calls=%d log=%s", closer.calls, output.String())
	}
}
