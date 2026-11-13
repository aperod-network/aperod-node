package p2p_test

// Tests that exercise the host dispatch loop by doing a full handshake
// and then sending specific message types from the remote side.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// connectAndHandshake dials addr, performs the asymmetric handshake
// (dialer sends Ping, host replies with Pong), then runs fn(conn).
// The connection is closed after fn returns.
func connectAndHandshake(t *testing.T, addr string, fn func(conn net.Conn)) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Send ping to host (dialer goes first under the asymmetric protocol)
	if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "dispatch-test-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	// Read pong from host
	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("expected pong, got %v err=%v", msgType, err)
	}

	// Give host time to register peer
	time.Sleep(30 * time.Millisecond)

	if fn != nil {
		fn(conn)
	}
	time.Sleep(50 * time.Millisecond)
}

// readMsgSkipGetHeaders reads messages from conn, skipping the host's own
// MsgGetHeaders sync requests.  Since the handshake-race fix, the host sends
// one unconditional MsgGetHeaders to every peer right after registration, so
// raw test peers must skip it to reach the response they are asserting on.
func readMsgSkipGetHeaders(conn net.Conn) (p2p.MessageType, []byte, error) {
	for {
		msgType, data, err := p2p.ReadMsg(conn)
		if err != nil || msgType != p2p.MsgGetHeaders {
			return msgType, data, err
		}
	}
}

// startHost starts a test host and returns it + its bound address.
func startHost(t *testing.T) (*p2p.Host, *stubHandler, string) {
	t.Helper()
	h2 := &stubHandler{}
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "host",
		UserAgent:  "aperod/0.1",
	}, h2, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(host.Stop)
	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}
	return host, h2, addr
}

// ─── MsgPing/Pong (dispatch) ─────────────────────────────────────────────────

func TestDispatch_Ping(t *testing.T) {
	_, _, addr := startHost(t)
	connectAndHandshake(t, addr, func(conn net.Conn) {
		// After handshake, send another ping — host should respond with pong
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{NodeID: "X", Height: 0, Timestamp: time.Now().Unix()})
		msgType, _, _ := readMsgSkipGetHeaders(conn)
		if msgType != p2p.MsgPong {
			t.Errorf("expected pong after ping, got %v", msgType)
		}
	})
}

// ─── MsgGetPeers (dispatch) ────────────────────────────────────────────────────

func TestDispatch_GetPeers(t *testing.T) {
	_, _, addr := startHost(t)
	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgGetPeers, struct{}{})
		msgType, _, err := readMsgSkipGetHeaders(conn)
		if err != nil {
			t.Logf("ReadMsg after GetPeers: %v (may have closed)", err)
			return
		}
		if msgType != p2p.MsgPeers {
			t.Errorf("expected MsgPeers, got %v", msgType)
		}
	})
}

// ─── MsgPeers (addKnownPeers) ────────────────────────────────────────────────

func TestDispatch_Peers(t *testing.T) {
	_, _, addr := startHost(t)
	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		peers := p2p.PeersMsg{Addrs: []string{"8.8.8.8:7777", "1.1.1.1:7777"}}
		p2p.WriteMsg(conn, p2p.MsgPeers, peers)
		time.Sleep(30 * time.Millisecond) // let host process
	})
}

func TestPeerExchangeCannotFillKnownPeerListWithOneMessage(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	addrs := make([]string, 32)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("8.8.4.%d:30303", i+1)
	}
	p2p.HostTryPeerExchange(host, addrs)
	if got := p2p.HostKnownPeerCount(host); got > 16 {
		t.Fatalf("one peer exchange added %d addresses from one source; maximum is 16", got)
	}
	if got := p2p.HostKnownPeerCount(host); got != 16 {
		t.Fatalf("valid IP:port peers admitted = %d, want 16", got)
	}

	tooMany := append(append([]string(nil), addrs...), addrs...)
	p2p.HostTryPeerExchange(host, tooMany)
	if got := p2p.HostKnownPeerCount(host); got != 16 {
		t.Fatalf("oversized peer exchange changed list to %d entries", got)
	}
	oversizedMsg, err := json.Marshal(p2p.PeersMsg{Addrs: tooMany})
	if err != nil {
		t.Fatal(err)
	}
	if err := p2p.HostDispatchForTest(host, "192.0.2.1:30303", p2p.MsgPeers, oversizedMsg); err == nil {
		t.Fatal("oversized MsgPeers was not rejected")
	}

	invalidHost := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	p2p.HostTryPeerExchange(invalidHost, []string{"not-an-address", "example.invalid:30303", "10.0.0.1:0"})
	if got := p2p.HostKnownPeerCount(invalidHost); got != 0 {
		t.Fatalf("invalid peer addresses admitted: %d", got)
	}
}

func TestPeerExchangeRejectsNonPublicAddresses(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	p2p.HostTryPeerExchange(host, []string{
		"127.0.0.1:30303",
		"10.1.2.3:30303",
		"169.254.2.3:30303",
		"192.0.2.10:30303",
		"203.0.113.10:30303",
		"[::1]:30303",
		"[::ffff:10.0.0.1]:30303",
		"[fc00::1]:30303",
		"[fe80::1]:30303",
		"[2001:db8::1]:30303",
		"8.8.8.8:30303",
		"[2606:4700:4700::1111]:30303",
	})
	if got := p2p.HostKnownPeerCount(host); got != 2 {
		t.Fatalf("public addresses admitted = %d, want 2", got)
	}
}

func TestUnsafeStalePeerEntryIsNotDialed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	p2p.HostAddKnownPeer(host, listener.Addr().String())
	host.DialPeer(listener.Addr().String())

	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err == nil {
		conn.Close()
		t.Fatal("dialed a loopback address injected into the peer list")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("Accept error = %v, want timeout", err)
	}
}

func TestRejectedTransactionIsNotRelayed(t *testing.T) {
	handler := &stubHandler{admitErr: errors.New("mempool rejected")}
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, handler, newTestLogger())
	hostConn, relayConn := net.Pipe()
	defer hostConn.Close()
	defer relayConn.Close()
	p2p.HostRegisterPeerForTest(host, "10.0.0.2:30303", hostConn)
	relayConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

	body, err := json.Marshal(core.Transaction{Fee: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := p2p.HostDispatchForTest(host, "10.0.0.1:30303", p2p.MsgTx, body); err != nil {
		t.Fatalf("dispatch rejected transaction: %v", err)
	}
	if _, _, err := p2p.ReadMsg(relayConn); err == nil {
		t.Fatal("rejected transaction was relayed")
	}
}

func TestAcceptedTransactionCanBeRelayed(t *testing.T) {
	handler := &stubHandler{}
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, handler, newTestLogger())
	hostConn, relayConn := net.Pipe()
	defer hostConn.Close()
	defer relayConn.Close()
	p2p.HostRegisterPeerForTest(host, "10.0.0.2:30303", hostConn)

	type result struct {
		msgType p2p.MessageType
		err     error
	}
	readResult := make(chan result, 1)
	go func() {
		msgType, _, err := p2p.ReadMsg(relayConn)
		readResult <- result{msgType: msgType, err: err}
	}()

	body, err := json.Marshal(core.Transaction{Fee: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := p2p.HostDispatchForTest(host, "10.0.0.1:30303", p2p.MsgTx, body); err != nil {
		t.Fatalf("dispatch accepted transaction: %v", err)
	}
	select {
	case got := <-readResult:
		if got.err != nil {
			t.Fatalf("read relayed transaction: %v", got.err)
		}
		if got.msgType != p2p.MsgTx {
			t.Fatalf("relayed message type = %v, want MsgTx", got.msgType)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted transaction was not relayed")
	}
}

// ─── MsgBlock (handler.OnBlock) ───────────────────────────────────────────────

func TestDispatch_Block(t *testing.T) {
	_, handler, addr := startHost(t)

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{
		Height:       0,
		ValidatorPub: pub,
		Timestamp:    time.Now().UnixNano(),
		MerkleRoot:   core.MerkleRoot(nil),
	}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		sb := p2p.BlockToMsg(block)
		p2p.WriteMsg(conn, p2p.MsgBlock, sb)
		time.Sleep(80 * time.Millisecond)
	})

	if handler.blockCount() == 0 {
		t.Error("OnBlock not called")
	}
}

// ─── MsgTx (handler.OnTransaction) ───────────────────────────────────────────

func TestDispatch_Tx(t *testing.T) {
	_, handler, addr := startHost(t)

	tx := core.Transaction{Version: core.TxVersionBase}

	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgTx, tx)
		time.Sleep(80 * time.Millisecond)
	})

	if handler.txCount() == 0 {
		t.Error("OnTransaction not called")
	}
}

// ─── MsgVote (handler.OnVote) ────────────────────────────────────────────────

func TestDispatch_Vote(t *testing.T) {
	_, handler, addr := startHost(t)

	var bh crypto.Hash32
	bh[0] = 0xFF
	vote := p2p.VoteMsg{BlockHash: bh, Height: 1, ValidatorPub: []byte{1}, Signature: []byte{2}}

	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgVote, vote)
		time.Sleep(80 * time.Millisecond)
	})

	if handler.voteCount() == 0 {
		t.Error("OnVote not called")
	}
}

// ─── MsgGetHeaders (handleGetHeaders → MsgHeaders) ───────────────────────────

func TestDispatch_GetHeaders(t *testing.T) {
	_, _, addr := startHost(t)
	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgGetHeaders, p2p.GetHeadersMsg{Limit: 100})
		msgType, _, err := readMsgSkipGetHeaders(conn)
		if err != nil {
			t.Logf("ReadMsg GetHeaders response: %v", err)
			return
		}
		if msgType != p2p.MsgHeaders {
			t.Errorf("expected MsgHeaders, got %v", msgType)
		}
	})
}

// ─── MsgHeaders (handleHeaders) ──────────────────────────────────────────────

func TestDispatch_Headers_WithBlock(t *testing.T) {
	// Handler returns nil for GetBlock, so host requests the block but handler
	// has nothing to serve — exercises the handleHeaders path.
	_, _, addr := startHost(t)

	var h [32]byte
	h[0] = 0xAA
	headers := p2p.HeadersMsg{
		Headers: []p2p.SerializedHeader{{
			Height: 1,
			Hash:   h,
		}},
	}

	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgHeaders, headers)
		// Host will try to send MsgGetBlock back to us — read it
		time.Sleep(50 * time.Millisecond)
		p2p.ReadMsg(conn) // may or may not arrive in time
	})
}

// ─── MsgGetBlock (handleGetBlock — block not found) ──────────────────────────

func TestDispatch_GetBlock_NotFound(t *testing.T) {
	_, _, addr := startHost(t)
	var hash crypto.Hash32
	hash[0] = 0xBB
	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgGetBlock, p2p.GetBlockMsg{Hash: hash})
		time.Sleep(50 * time.Millisecond)
	})
}

// ─── Unknown message type → dispatch error ───────────────────────────────────

func TestDispatch_UnknownMsgType(t *testing.T) {
	_, _, addr := startHost(t)
	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		// Write raw unknown message type 0xFF
		p2p.WriteMsg(conn, p2p.MessageType(0xFF), map[string]string{"x": "y"})
		time.Sleep(50 * time.Millisecond)
		// Host logs a dispatch error but doesn't disconnect
	})
}

// ─── Duplicate peer connection ────────────────────────────────────────────────

func TestDispatch_DuplicatePeer(t *testing.T) {
	// If two connections arrive from the same remote addr, the second is dropped.
	// This is hard to test with net.Dial since addr is ephemeral. We just verify
	// the host stays healthy after multiple sequential connections.
	_, _, addr := startHost(t)
	for range 3 {
		connectAndHandshake(t, addr, nil)
	}
}

// ─── Corrupt handshake (send garbage instead of pong) ────────────────────────

func TestHandshake_BadPong(t *testing.T) {
	_, _, addr := startHost(t)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	// Asymmetric handshake: dialer must send MsgPing first.
	// Send wrong type (MsgGetPeers instead of MsgPing) → host should drop conn.
	p2p.WriteMsg(conn, p2p.MsgGetPeers, struct{}{})
	time.Sleep(100 * time.Millisecond)
}

// ─── Sync state ──────────────────────────────────────────────────────────────

func TestSyncState_MarkAndPending(t *testing.T) {
	s := p2p.NewSyncState()
	if s.Pending() != 0 {
		t.Errorf("pending = %d, want 0", s.Pending())
	}

	var h crypto.Hash32
	h[0] = 1
	s.MarkRequested(h)
	if s.Pending() != 1 {
		t.Errorf("pending = %d, want 1 after request", s.Pending())
	}

	s.MarkReceived(h)
	if s.Pending() != 0 {
		t.Errorf("pending = %d, want 0 after receive", s.Pending())
	}
}

func TestSyncState_MultiplePending(t *testing.T) {
	s := p2p.NewSyncState()
	hashes := make([]crypto.Hash32, 5)
	for i := range hashes {
		hashes[i][0] = byte(i + 1)
		s.MarkRequested(hashes[i])
	}
	if s.Pending() != 5 {
		t.Errorf("pending = %d, want 5", s.Pending())
	}
	s.MarkReceived(hashes[0])
	s.MarkReceived(hashes[1])
	if s.Pending() != 3 {
		t.Errorf("pending = %d, want 3", s.Pending())
	}
}

// ─── blockToMsg / msgToBlock round-trip ──────────────────────────────────────

func TestBlockToMsg_RoundTrip(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{
		Height:       7,
		ValidatorPub: pub,
		Timestamp:    time.Now().UnixNano(),
		MerkleRoot:   core.MerkleRoot(nil),
	}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	sb := p2p.BlockToMsg(block)
	if sb.Header.Height != 7 {
		t.Errorf("BlockToMsg height = %d, want 7", sb.Header.Height)
	}

	recovered := p2p.MsgToBlock(sb)
	if recovered == nil {
		t.Fatal("MsgToBlock returned nil")
	}
	if recovered.Header.Height != block.Header.Height {
		t.Errorf("height mismatch: got %d, want %d", recovered.Header.Height, block.Header.Height)
	}
}

// ─── Unmarshal helper ─────────────────────────────────────────────────────────

func TestUnmarshal(t *testing.T) {
	data, _ := json.Marshal(map[string]int{"n": 42})
	var out map[string]int
	if err := p2p.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out["n"] != 42 {
		t.Errorf("n = %d, want 42", out["n"])
	}
}
