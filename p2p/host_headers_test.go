package p2p

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

type headerRequestTestHandler struct{}

func (headerRequestTestHandler) OnBlock(*core.Block)                   {}
func (headerRequestTestHandler) OnTransaction(*core.Transaction)       {}
func (headerRequestTestHandler) OnVote(VoteMsg)                        {}
func (headerRequestTestHandler) CurrentHeight() uint64                 { return 0 }
func (headerRequestTestHandler) CurrentTailHashes(int) []crypto.Hash32 { return nil }
func (headerRequestTestHandler) GetBlock(crypto.Hash32) *core.Block    { return nil }

func newHeaderRequestTestHost() *Host {
	return &Host{
		handler: headerRequestTestHandler{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func newHeaderRequestTestPeer(t *testing.T) (*Peer, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return &Peer{
		conn:          server,
		addr:          "header-test-peer",
		pendingBlocks: make(map[crypto.Hash32]pendingBlockEntry),
	}, client
}

func readHeaderTestMessage(t *testing.T, conn net.Conn) (MessageType, []byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var lengthBytes [4]byte
	if _, err := io.ReadFull(conn, lengthBytes[:]); err != nil {
		t.Fatalf("read message length: %v", err)
	}
	length := binary.BigEndian.Uint32(lengthBytes[:])
	if length == 0 || length > MaxMessageSize {
		t.Fatalf("invalid test message length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Fatalf("read message body: %v", err)
	}
	return MessageType(body[0]), body[1:]
}

func TestHandleHeadersRequestsEachHashOnce(t *testing.T) {
	host := newHeaderRequestTestHost()
	peer, client := newHeaderRequestTestPeer(t)
	var hash [32]byte
	hash[0] = 0xCC
	msg := HeadersMsg{Headers: []SerializedHeader{
		{Height: 1, Hash: hash},
		{Height: 1, Hash: hash},
	}}

	done := make(chan struct{})
	go func() {
		host.handleHeaders(peer, msg)
		close(done)
	}()

	messageType, payload := readHeaderTestMessage(t, client)
	if messageType != MsgGetBlock {
		t.Fatalf("message type = %v, want MsgGetBlock", messageType)
	}
	var request GetBlockMsg
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("decode GetBlock request: %v", err)
	}
	if request.Hash != crypto.Hash32(hash) {
		t.Fatalf("requested hash = %x, want %x", request.Hash, hash)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler sent another request for the duplicate hash")
	}

	peer.pendingBlocksMu.Lock()
	pendingCount := len(peer.pendingBlocks)
	peer.pendingBlocksMu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("pending block count = %d, want 1", pendingCount)
	}
}

func TestHandleHeadersCapsPendingBlockRequests(t *testing.T) {
	host := newHeaderRequestTestHost()
	peer, client := newHeaderRequestTestPeer(t)
	msg := HeadersMsg{Headers: make([]SerializedHeader, maxHeadersPerMessage+1)}
	for i := range msg.Headers {
		msg.Headers[i].Height = uint64(i + 1)
		msg.Headers[i].Hash[0] = byte((i + 1) >> 8)
		msg.Headers[i].Hash[1] = byte(i + 1)
	}

	done := make(chan struct{})
	go func() {
		host.handleHeaders(peer, msg)
		close(done)
	}()

	for range maxHeadersPerMessage {
		messageType, _ := readHeaderTestMessage(t, client)
		if messageType != MsgGetBlock {
			t.Fatalf("message type = %v, want MsgGetBlock", messageType)
		}
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop at the pending block request cap")
	}

	peer.pendingBlocksMu.Lock()
	pendingCount := len(peer.pendingBlocks)
	peer.pendingBlocksMu.Unlock()
	if pendingCount != maxHeadersPerMessage {
		t.Fatalf("pending block count = %d, want %d", pendingCount, maxHeadersPerMessage)
	}
}
