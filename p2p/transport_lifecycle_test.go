package p2p

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/internal/testutil"
)

type deadlineLifecycleConn struct {
	net.Conn
	failAt int
	calls  int
	err    error
}

func (c *deadlineLifecycleConn) SetDeadline(deadline time.Time) error {
	c.calls++
	if c.calls == c.failAt {
		return c.err
	}
	return c.Conn.SetDeadline(deadline)
}

func TestTLSLifecycle_InitialDeadlineFailureStopsBeforeHandshake(t *testing.T) {
	injected := errors.New("injected initial deadline failure")
	// An attempted handshake would panic through the deliberately absent
	// underlying connection. The rejected deadline must be the first operation.
	conn := &deadlineLifecycleConn{failAt: 1, err: injected}
	err := completeTLSHandshake(tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS13}), time.Second)
	if !errors.Is(err, injected) || conn.calls != 1 {
		t.Fatalf("deadline failure was not preserved: calls=%d err=%v", conn.calls, err)
	}
}

func TestTLSLifecycle_ClearDeadlineAndPositiveControl(t *testing.T) {
	for _, failClear := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "clear_failure"}[failClear], func(t *testing.T) {
			serverConfig, serverFingerprint, err := GenerateNodeTLSConfig()
			if err != nil {
				t.Fatal(err)
			}
			clientConfig, clientFingerprint, err := GenerateNodeTLSConfig()
			if err != nil {
				t.Fatal(err)
			}
			serverConfig, err = TLSConfigWithAllowedPeerFingerprints(serverConfig, []string{clientFingerprint})
			if err != nil {
				t.Fatal(err)
			}
			clientConfig, err = TLSConfigWithAllowedPeerFingerprints(clientConfig, []string{serverFingerprint})
			if err != nil {
				t.Fatal(err)
			}
			// No application reader follows the handshake in this pipe fixture.
			serverConfig.SessionTicketsDisabled = true
			serverRaw, clientRaw := net.Pipe()
			defer testutil.CleanupClose(t, serverRaw)
			defer testutil.CleanupClose(t, clientRaw)
			injected := errors.New("injected deadline clear failure")
			serverConn := &deadlineLifecycleConn{Conn: serverRaw, err: injected}
			if failClear {
				serverConn.failAt = 2
			}
			result := make(chan error, 1)
			go func() {
				result <- completeTLSHandshake(tls.Server(serverConn, serverConfig), 2*time.Second)
			}()
			testutil.NoError(t, clientRaw.SetDeadline(time.Now().Add(2*time.Second)))
			if err := tls.Client(clientRaw, clientConfig).Handshake(); err != nil {
				t.Fatalf("mutually pinned client handshake failed: %v", err)
			}
			select {
			case err := <-result:
				if failClear && !errors.Is(err, injected) {
					t.Fatalf("clear failure was not preserved: %v", err)
				}
				if !failClear && err != nil {
					t.Fatalf("positive handshake failed: %v", err)
				}
				if serverConn.calls != 2 {
					t.Fatalf("got %d deadline operations, want set then clear", serverConn.calls)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("TLS lifecycle did not finish within the bounded fixture deadline")
			}
		})
	}
}

type lifecycleCloser struct {
	calls int
	err   error
}

func (c *lifecycleCloser) Close() error {
	c.calls++
	return c.err
}

func TestTransportCleanup_RecordsFailureWithoutRetry(t *testing.T) {
	for _, injected := range []error{nil, errors.New("injected close failure")} {
		var output bytes.Buffer
		host := &Host{log: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))}
		closer := &lifecycleCloser{err: injected}
		host.closeTransport(closer, "test teardown")
		if closer.calls != 1 {
			t.Fatalf("close was called %d times", closer.calls)
		}
		if injected != nil && (!strings.Contains(output.String(), injected.Error()) || !strings.Contains(output.String(), "test teardown")) {
			t.Fatalf("teardown failure was lost: %s", output.String())
		}
		if injected == nil && output.Len() != 0 {
			t.Fatalf("successful teardown logged a failure: %s", output.String())
		}
	}
}

type partialWriteCloseFailureConn struct {
	net.Conn
	writes   int
	closes   int
	writeErr error
	closeErr error
}

func (c *partialWriteCloseFailureConn) SetWriteDeadline(time.Time) error { return nil }

func (c *partialWriteCloseFailureConn) Write([]byte) (int, error) {
	c.writes++
	return 1, c.writeErr
}

func (c *partialWriteCloseFailureConn) Close() error {
	c.closes++
	return c.closeErr
}

func TestPeerSend_PartialWriteRetainsCloseFailureAndPoisonFence(t *testing.T) {
	writeErr := errors.New("injected partial write")
	closeErr := errors.New("injected poisoned close failure")
	conn := &partialWriteCloseFailureConn{writeErr: writeErr, closeErr: closeErr}
	peer := &Peer{conn: conn}
	wrote, err := peer.sendN(MsgPing, PingMsg{})
	if wrote != 1 || !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Fatalf("partial write or teardown evidence lost: wrote=%d err=%v", wrote, err)
	}
	wrote, err = peer.sendN(MsgPing, PingMsg{})
	if wrote != 0 || !errors.Is(err, net.ErrClosed) || conn.writes != 1 || conn.closes != 1 {
		t.Fatalf("poisoned stream was reused: wrote=%d err=%v writes=%d closes=%d",
			wrote, err, conn.writes, conn.closes)
	}
}
