// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type completionTransport func(*http.Request) (*http.Response, error)

func (f completionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type completionBody struct {
	io.ReadCloser
	first bool
	finish func()
	eof *atomic.Bool
}

func (b *completionBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		return io.ReadFull(b.ReadCloser, p[:len("{\"ok\":true}\n")])
	}
	b.finish()
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.eof.Store(true)
	}
	return n, err
}

func (b *completionBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func TestSnapshotRequestConsumesTerminalHTTPChunk(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	defer unblock()
	var sawEOF atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{\"ok\":true}\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("response completion fence was not released")
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 3 * time.Second
	transport := client.Transport
	client.Transport = completionTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(r)
		if err == nil {
			resp.Body = &completionBody{ReadCloser: resp.Body, finish: unblock, eof: &sawEOF}
		}
		return resp, err
	})
	status, body := snapshotRequest(t, client, http.MethodGet, srv.URL, nil)
	if status != http.StatusOK || body["ok"] != true || !sawEOF.Load() {
		t.Fatalf("HTTP snapshot helper returned before terminal EOF: status=%d body=%#v eof=%v", status, body, sawEOF.Load())
	}
}
