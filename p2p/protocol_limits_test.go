package p2p

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/aperod/aperod/internal/testutil"
)

func TestReadMsgRejectsOversizedControlPayloadBeforeReadingBody(t *testing.T) {
	reader, writer := net.Pipe()
	defer testutil.CleanupClose(t, reader)
	defer testutil.CleanupClose(t, writer)

	writeErr := make(chan error, 1)
	go func() {
		var frameHeader [5]byte
		binary.BigEndian.PutUint32(frameHeader[:4], uint32(1+maxControlPayloadBytes+1))
		frameHeader[4] = byte(MsgPing)
		_, err := writer.Write(frameHeader[:])
		writeErr <- err
	}()

	_, _, err := readMsg(reader)
	if err == nil || !strings.Contains(err.Error(), "message payload too large") {
		t.Fatalf("readMsg error = %v, want oversized control payload rejection", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write frame header: %v", err)
	}
}

func TestWriteMsgRejectsOversizedControlPayload(t *testing.T) {
	reader, writer := net.Pipe()
	defer testutil.CleanupClose(t, reader)
	defer testutil.CleanupClose(t, writer)

	err := WriteMsg(writer, MsgPing, PingMsg{UserAgent: strings.Repeat("x", maxControlPayloadBytes)})
	if err == nil || !strings.Contains(err.Error(), "message payload too large") {
		t.Fatalf("WriteMsg error = %v, want oversized control payload rejection", err)
	}
}

func TestBlockAndTransactionKeepFullFrameBudget(t *testing.T) {
	want := uint32(MaxMessageSize - 1)
	for _, msgType := range []MessageType{MsgBlock, MsgTx} {
		if got := maxMessagePayloadBytes(msgType); got != want {
			t.Errorf("max payload for message type %d = %d, want %d", msgType, got, want)
		}
	}
	if got := maxMessagePayloadBytes(MsgHeaders); got != maxHeadersPayloadBytes {
		t.Fatalf("max header payload = %d, want %d", got, maxHeadersPayloadBytes)
	}
}

func TestWriteMsgN_PreservesExactWireBytesAndFullFrameBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		msgType MessageType
		payload interface{}
		size    int
	}{
		{"literal wire vector", MsgPing, map[string]int{"v": 7}, 12},
		{"full transaction budget", MsgTx, strings.Repeat("x", MaxMessageSize-3), MaxMessageSize + 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := net.Pipe()
			defer testutil.CleanupClose(t, reader)
			defer testutil.CleanupClose(t, writer)
			frame := make([]byte, tc.size)
			readErr := make(chan error, 1)
			go func() {
				_, err := io.ReadFull(reader, frame)
				readErr <- err
			}()
			n, err := writeMsgN(writer, tc.msgType, tc.payload)
			if err != nil || n != tc.size {
				t.Fatalf("write = (%d, %v), want (%d, nil)", n, err, tc.size)
			}
			if err := <-readErr; err != nil {
				t.Fatal(err)
			}
			if tc.msgType == MsgPing {
				want := []byte{0, 0, 0, 8, byte(MsgPing), '{', '"', 'v', '"', ':', '7', '}'}
				if !bytes.Equal(frame, want) {
					t.Fatalf("wire bytes changed: %x", frame)
				}
			} else {
				if binary.BigEndian.Uint32(frame[:4]) != MaxMessageSize || frame[4] != byte(MsgTx) {
					t.Fatal("full-size frame prefix or message type changed")
				}
				if frame[5] != '"' || frame[len(frame)-1] != '"' ||
					!bytes.Equal(frame[6:len(frame)-1], []byte(tc.payload.(string))) {
					t.Fatal("full-size JSON payload changed")
				}
			}
		})
	}
}

func TestWriteMsgN_RejectsOneByteAboveFullFrameBudgetWithoutWriting(t *testing.T) {
	reader, writer := net.Pipe()
	defer testutil.CleanupClose(t, reader)
	defer testutil.CleanupClose(t, writer)
	// JSON adds two quote bytes: this is exactly one byte above the maximum
	// body payload, not merely an oversized control-message fixture.
	n, err := writeMsgN(writer, MsgTx, strings.Repeat("x", MaxMessageSize-2))
	if n != 0 || err == nil || !strings.Contains(err.Error(), "message payload too large") {
		t.Fatalf("oversized transaction write = (%d, %v), want zero bytes and rejection", n, err)
	}
}
