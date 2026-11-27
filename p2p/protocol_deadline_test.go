package p2p

import (
	"errors"
	"net"
	"testing"
	"time"
)

type failedDeadlineConn struct {
	// A nil transport deliberately panics if framing attempts I/O after the
	// deadline setup failure, so the test cannot pass on an unrelated I/O error.
	net.Conn
	err error
}

func (c *failedDeadlineConn) SetReadDeadline(time.Time) error  { return c.err }
func (c *failedDeadlineConn) SetWriteDeadline(time.Time) error { return c.err }

type observedMarshal struct{ called *bool }

func (p observedMarshal) MarshalJSON() ([]byte, error) {
	*p.called = true
	return []byte(`{}`), nil
}

func TestMessageDeadlineFailureStopsIO(t *testing.T) {
	failure := errors.New("injected deadline setup failure")
	conn := &failedDeadlineConn{err: failure}
	called := false
	written, err := writeMsgN(conn, MsgPing, observedMarshal{called: &called})
	if !errors.Is(err, failure) || written != 0 || called {
		t.Fatalf("write did not fail before serialization/I/O: bytes=%d marshal=%t err=%v", written, called, err)
	}
	_, payload, err := readMsg(conn)
	if !errors.Is(err, failure) || payload != nil {
		t.Fatalf("read did not preserve the deadline failure: payload=%x err=%v", payload, err)
	}
}
