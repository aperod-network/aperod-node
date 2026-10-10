package p2p_test

// Tests for p2p wire protocol: message encode/decode, PeerMgr, message types.

import (
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// ─── PeerMgr tests (already exist in peermgr_test.go, adding protocol tests) ─

// ─── Protocol: encode/decode via loopback pipe ────────────────────────────────

// newPipe returns a pair of connected net.Conn using net.Pipe().
func newPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestProtocol_PingPong(t *testing.T) {
	a, b := newPipe(t)

	// Send PingMsg from a → b
	ping := p2p.PingMsg{
		NodeID:    "node-a",
		Height:    42,
		UserAgent: "aperod/0.1.0",
		Timestamp: time.Now().Unix(),
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- p2p.WriteMsg(a, p2p.MsgPing, ping)
	}()

	msgType, data, err := p2p.ReadMsg(b)
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if msgType != p2p.MsgPing {
		t.Errorf("msgType = %v, want MsgPing", msgType)
	}
	if len(data) == 0 {
		t.Error("expected non-empty payload")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("WriteMsg: %v", err)
	}
}

func TestProtocol_GetHeaders(t *testing.T) {
	a, b := newPipe(t)

	var h crypto.Hash32
	h[0] = 0xAB
	msg := p2p.GetHeadersMsg{
		KnownHashes: []crypto.Hash32{h},
		Limit:       100,
	}

	go p2p.WriteMsg(a, p2p.MsgGetHeaders, msg)
	msgType, _, err := p2p.ReadMsg(b)
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if msgType != p2p.MsgGetHeaders {
		t.Errorf("msgType = %v, want MsgGetHeaders", msgType)
	}
}

func TestProtocol_GetBlock(t *testing.T) {
	a, b := newPipe(t)

	var hash crypto.Hash32
	hash[0] = 0xCC
	msg := p2p.GetBlockMsg{Hash: hash}

	go p2p.WriteMsg(a, p2p.MsgGetBlock, msg)
	msgType, _, err := p2p.ReadMsg(b)
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if msgType != p2p.MsgGetBlock {
		t.Errorf("msgType = %v, want MsgGetBlock", msgType)
	}
}

func TestProtocol_VoteMsg(t *testing.T) {
	a, b := newPipe(t)

	var blockHash crypto.Hash32
	blockHash[0] = 0xDE
	msg := p2p.VoteMsg{
		BlockHash:    blockHash,
		Height:       5,
		ValidatorPub: []byte{1, 2, 3},
		Signature:    []byte{4, 5, 6},
	}

	go p2p.WriteMsg(a, p2p.MsgVote, msg)
	msgType, _, err := p2p.ReadMsg(b)
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if msgType != p2p.MsgVote {
		t.Errorf("msgType = %v, want MsgVote", msgType)
	}
}

func TestProtocol_PeersMsg(t *testing.T) {
	a, b := newPipe(t)

	msg := p2p.PeersMsg{Addrs: []string{"192.168.1.1:7777", "10.0.0.1:7777"}}
	go p2p.WriteMsg(a, p2p.MsgPeers, msg)
	msgType, _, err := p2p.ReadMsg(b)
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if msgType != p2p.MsgPeers {
		t.Errorf("msgType = %v, want MsgPeers", msgType)
	}
}

func TestProtocol_AllMessageTypes(t *testing.T) {
	types := []p2p.MessageType{
		p2p.MsgPing, p2p.MsgPong,
		p2p.MsgGetHeaders, p2p.MsgHeaders,
		p2p.MsgGetBlock, p2p.MsgBlock,
		p2p.MsgTx, p2p.MsgVote,
		p2p.MsgGetPeers, p2p.MsgPeers,
	}
	for _, mt := range types {
		a, b := newPipe(t)
		go p2p.WriteMsg(a, mt, map[string]string{"t": "test"})
		got, _, err := p2p.ReadMsg(b)
		if err != nil {
			t.Errorf("ReadMsg for %v: %v", mt, err)
			continue
		}
		if got != mt {
			t.Errorf("round-trip: got %v, want %v", got, mt)
		}
	}
}
