package p2p

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

func TestReadMsgRejectsOversizedControlPayloadBeforeReadingBody(t *testing.T) {
	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()

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
	defer reader.Close()
	defer writer.Close()

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
