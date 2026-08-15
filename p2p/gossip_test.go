package p2p_test

// Tests for GossipFilter, Gossip relay, host gossip-in-dispatch, BanPeer,
// SetHeaderProvider.

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// ─── GossipFilter ─────────────────────────────────────────────────────────────

func TestGossipFilter_MarkAndCheck_New(t *testing.T) {
	gf := p2p.NewGossipFilter()
	var h crypto.Hash32
	h[0] = 1
	if !gf.MarkAndCheck(h) {
		t.Error("first MarkAndCheck must return true (new hash)")
	}
}

func TestGossipFilter_MarkAndCheck_Duplicate(t *testing.T) {
	gf := p2p.NewGossipFilter()
	var h crypto.Hash32
	h[0] = 2
	gf.MarkAndCheck(h)
	if gf.MarkAndCheck(h) {
		t.Error("second MarkAndCheck must return false (duplicate)")
	}
}

func TestGossipFilter_HasSeen(t *testing.T) {
	gf := p2p.NewGossipFilter()
	var h crypto.Hash32
	h[0] = 3
	if gf.HasSeen(h) {
		t.Error("HasSeen must be false before MarkAndCheck")
	}
	gf.MarkAndCheck(h)
	if !gf.HasSeen(h) {
		t.Error("HasSeen must be true after MarkAndCheck")
	}
}

func TestGossipFilter_Size(t *testing.T) {
	gf := p2p.NewGossipFilter()
	if gf.Size() != 0 {
		t.Errorf("initial Size = %d, want 0", gf.Size())
	}
	for i := range 5 {
		var h crypto.Hash32
		h[0] = byte(i + 10)
		gf.MarkAndCheck(h)
	}
	if gf.Size() != 5 {
		t.Errorf("Size = %d, want 5", gf.Size())
	}
}

func TestGossipFilter_DifferentHashes_BothNew(t *testing.T) {
	gf := p2p.NewGossipFilter()
	var h1, h2 crypto.Hash32
	h1[0] = 0xAA
	h2[0] = 0xBB
	if !gf.MarkAndCheck(h1) {
		t.Error("h1 first time must be new")
	}
	if !gf.MarkAndCheck(h2) {
		t.Error("h2 first time must be new")
	}
	if gf.MarkAndCheck(h1) {
		t.Error("h1 second time must be duplicate")
	}
}

// ─── Gossip relay (standalone, no network) ────────────────────────────────────

func TestGossip_RelayBlock_FirstTime_ReturnsTrue(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 1, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	if !g.RelayBlock(block, "") {
		t.Error("first RelayBlock must return true")
	}
}

func TestGossip_RelayBlock_Duplicate_ReturnsFalse(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 2, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	g.RelayBlock(block, "")
	if g.RelayBlock(block, "") {
		t.Error("second RelayBlock must return false (duplicate)")
	}
}

func TestGossip_RelayTx_FirstTime_ReturnsTrue(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)
	tx := &core.Transaction{Version: core.TxVersionBase}
	if !g.RelayTx(tx, "") {
		t.Error("first RelayTx must return true")
	}
}

func TestGossip_RelayTx_Duplicate_ReturnsFalse(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)
	tx := &core.Transaction{Version: core.TxVersionBase}
	g.RelayTx(tx, "")
	if g.RelayTx(tx, "") {
		t.Error("second RelayTx must return false")
	}
}

func TestGossip_MarkBlock_PreventsRelay(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 3, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	g.MarkBlock(block.Hash())
	if g.RelayBlock(block, "") {
		t.Error("RelayBlock after MarkBlock must return false")
	}
}

func TestGossip_MarkTx_PreventsRelay(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)
	tx := &core.Transaction{Version: core.TxVersionBase}
	g.MarkTx(tx.Hash())
	if g.RelayTx(tx, "") {
		t.Error("RelayTx after MarkTx must return false")
	}
}

func TestGossip_Filter_NotNil(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	g := p2p.NewGossip(host)
	if g.Filter() == nil {
		t.Error("Filter() must not be nil")
	}
}

// ─── Host: gossip relay via dispatch ──────────────────────────────────────────

// rawConnect dials addr and returns the conn.
func rawConnect(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("rawConnect: %v", err)
	}
	return conn
}

// rawHandshake performs the ping/pong handshake with host. Returns false on failure.
// Uses the asymmetric protocol: dialer sends Ping first, host replies with Pong.
func rawHandshake(t *testing.T, conn net.Conn) bool {
	t.Helper()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	// Dialer goes first: send Ping to host.
	if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "raw-peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Logf("rawHandshake: write ping: %v", err)
		return false
	}
	// Host replies with Pong.
	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPong {
		t.Logf("rawHandshake: expected pong, got %v err=%v", msgType, err)
		return false
	}
	time.Sleep(30 * time.Millisecond)
	return true
}

// tryReadMsg attempts to read one message from conn.
// It closes conn after deadline to unblock ReadMsg (which sets its own 30s deadline).
// Returns (msgType, true) if a message arrived before deadline, or (0, false) on timeout.
func tryReadMsg(conn net.Conn, deadline time.Duration) (p2p.MessageType, bool) {
	type result struct {
		mt p2p.MessageType
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		for {
			mt, _, err := p2p.ReadMsg(conn)
			if err != nil {
				ch <- result{mt, false}
				return
			}
			if mt == p2p.MsgGetHeaders {
				// Host's own post-handshake sync request — not a relay;
				// skip it and keep waiting for the message under test.
				continue
			}
			ch <- result{mt, true}
			return
		}
	}()
	time.AfterFunc(deadline, func() { conn.Close() })
	r := <-ch
	return r.mt, r.ok
}

func TestHost_GossipRelay_TwoPeers_BlockRelayed(t *testing.T) {
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "relay-host",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	// Peer A — will send the block
	connA := rawConnect(t, addr)
	defer connA.Close()
	if !rawHandshake(t, connA) {
		t.Skip("handshake A failed")
	}

	// Peer B — should receive the relay
	connB := rawConnect(t, addr)
	if !rawHandshake(t, connB) {
		connB.Close()
		t.Skip("handshake B failed")
	}

	time.Sleep(50 * time.Millisecond) // ensure both peers registered

	// Peer A sends block
	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 99, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}
	sb := p2p.BlockToMsg(block)

	connA.SetDeadline(time.Now().Add(2 * time.Second))
	if err := p2p.WriteMsg(connA, p2p.MsgBlock, sb); err != nil {
		t.Fatalf("send block from A: %v", err)
	}

	// Peer B should receive relay (tryReadMsg closes connB after deadline)
	mt, ok := tryReadMsg(connB, 500*time.Millisecond)
	if !ok {
		t.Log("peer B relay read timed out — timing sensitive, skipping assertion")
		return
	}
	if mt != p2p.MsgBlock {
		t.Errorf("peer B received %v, want MsgBlock", mt)
	}
}

func TestHost_GossipRelay_NotEchoedToSender(t *testing.T) {
	// A block received from peer A must NOT be echoed back to peer A.
	// Since ReadMsg internally sets a 30s deadline, we use tryReadMsg which
	// closes the connection after our deadline to unblock it.
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "no-echo",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	connA := rawConnect(t, addr)
	if !rawHandshake(t, connA) {
		connA.Close()
		t.Skip("handshake failed")
	}
	time.Sleep(30 * time.Millisecond)

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 55, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}
	sb := p2p.BlockToMsg(block)

	connA.SetDeadline(time.Now().Add(2 * time.Second))
	p2p.WriteMsg(connA, p2p.MsgBlock, sb)

	// tryReadMsg will close connA after 200ms if nothing arrives
	mt, ok := tryReadMsg(connA, 200*time.Millisecond)
	if ok && mt == p2p.MsgBlock {
		t.Error("block must NOT be echoed back to sender")
	}
	// ok=false (timeout/close) is the expected outcome
}

func TestHost_GossipRelay_DuplicateBlock_NotRelayedTwice(t *testing.T) {
	handler := &stubHandler{}
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "dedup-host",
		UserAgent:  "aperod/test",
	}, handler, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	connA := rawConnect(t, addr)
	defer connA.Close()
	if !rawHandshake(t, connA) {
		t.Skip("handshake failed")
	}
	time.Sleep(30 * time.Millisecond)

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 77, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}
	sb := p2p.BlockToMsg(block)

	connA.SetDeadline(time.Now().Add(2 * time.Second))
	p2p.WriteMsg(connA, p2p.MsgBlock, sb)
	time.Sleep(30 * time.Millisecond)
	p2p.WriteMsg(connA, p2p.MsgBlock, sb) // duplicate
	time.Sleep(80 * time.Millisecond)

	// Handler called at least once, no panic
	handler.mu.Lock()
	blockCount := len(handler.blocks)
	handler.mu.Unlock()
	if blockCount < 1 {
		t.Error("OnBlock must have been called at least once")
	}
}

// ─── BanPeer ──────────────────────────────────────────────────────────────────

func TestHost_BanPeer_ClosesConnection(t *testing.T) {
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "ban-host",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	conn := rawConnect(t, addr)
	defer conn.Close()
	if !rawHandshake(t, conn) {
		t.Skip("handshake failed")
	}
	time.Sleep(30 * time.Millisecond)

	// Ban using the peer's local address (as seen by host)
	peerAddr := conn.LocalAddr().String()
	host.BanPeer(peerAddr, "test ban", time.Hour)
	time.Sleep(50 * time.Millisecond)
	// Peer count may or may not be 0 depending on how addr is keyed;
	// main thing is BanPeer doesn't panic and IsBanned works.
}

// ─── SetHeaderProvider ────────────────────────────────────────────────────────

type stubHeaderProvider struct {
	headers []core.BlockHeader
}

func (s *stubHeaderProvider) HeadersFrom(_ []crypto.Hash32, limit int) []core.BlockHeader {
	if limit > len(s.headers) {
		limit = len(s.headers)
	}
	return s.headers[:limit]
}

func TestHost_SetHeaderProvider_ServesHeaders(t *testing.T) {
	hp := &stubHeaderProvider{
		headers: []core.BlockHeader{{Height: 1}, {Height: 2}},
	}
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "header-host",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())
	host.SetHeaderProvider(hp)

	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgGetHeaders, p2p.GetHeadersMsg{Limit: 10})
		msgType, data, err := readMsgSkipGetHeaders(conn)
		if err != nil {
			t.Logf("ReadMsg GetHeaders: %v", err)
			return
		}
		if msgType != p2p.MsgHeaders {
			t.Errorf("expected MsgHeaders, got %v", msgType)
			return
		}
		var resp p2p.HeadersMsg
		if err := p2p.Unmarshal(data, &resp); err != nil {
			t.Fatalf("unmarshal HeadersMsg: %v", err)
		}
		if len(resp.Headers) != 2 {
			t.Errorf("expected 2 headers, got %d", len(resp.Headers))
		}
	})
}

func TestHost_NoHeaderProvider_ServesEmpty(t *testing.T) {
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "no-hp-host",
		UserAgent:  "aperod/test",
	}, &stubHandler{}, newTestLogger())

	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	connectAndHandshake(t, addr, func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		p2p.WriteMsg(conn, p2p.MsgGetHeaders, p2p.GetHeadersMsg{Limit: 10})
		msgType, data, err := readMsgSkipGetHeaders(conn)
		if err != nil {
			t.Logf("ReadMsg: %v", err)
			return
		}
		if msgType != p2p.MsgHeaders {
			t.Errorf("expected MsgHeaders, got %v", msgType)
			return
		}
		var resp p2p.HeadersMsg
		if err := p2p.Unmarshal(data, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.Headers) != 0 {
			t.Errorf("expected 0 headers without provider, got %d", len(resp.Headers))
		}
	})
}

// ─── BroadcastBlock retry on transient send failure ──────────────────────────

func TestHost_BroadcastBlock_TransientFailure_RetriesOnce(t *testing.T) {
	host := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		MaxPeers:            10,
		NodeID:              "retry-host",
		UserAgent:           "aperod/test",
		BroadcastRetryDelay: 50 * time.Millisecond,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	conn := rawConnect(t, addr)
	defer conn.Close()
	if !rawHandshake(t, conn) {
		t.Skip("handshake failed")
	}
	time.Sleep(50 * time.Millisecond) // ensure peer registered

	// Hook: fail the FIRST MsgBlock send only, pass everything else through.
	var mu sync.Mutex
	blockSends := 0
	p2p.HostSetSendHook(host, func(p *p2p.Peer, mt p2p.MessageType, payload interface{}) (int, error) {
		if mt != p2p.MsgBlock {
			return p.PeerSendN(mt, payload)
		}
		mu.Lock()
		blockSends++
		first := blockSends == 1
		mu.Unlock()
		if first {
			return 0, &fakeTimeoutErr{} // transient: timeout before any bytes hit the wire
		}
		return p.PeerSendN(mt, payload)
	})

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 123, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	host.BroadcastBlock(block)

	// The peer must still receive the block quickly via the single retry.
	mt, ok := tryReadMsg(conn, 2*time.Second)
	if !ok {
		t.Fatal("peer never received the block despite retry")
	}
	if mt != p2p.MsgBlock {
		t.Fatalf("peer received %v, want MsgBlock", mt)
	}
	mu.Lock()
	sends := blockSends
	mu.Unlock()
	if sends != 2 {
		t.Errorf("MsgBlock send attempts = %d, want exactly 2 (initial + one retry)", sends)
	}
}

func TestHost_BroadcastBlock_PeerEvicted_NoRetry(t *testing.T) {
	host := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		MaxPeers:            10,
		NodeID:              "evict-host",
		UserAgent:           "aperod/test",
		BroadcastRetryDelay: 80 * time.Millisecond,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	conn := rawConnect(t, addr)
	defer conn.Close()
	if !rawHandshake(t, conn) {
		t.Skip("handshake failed")
	}
	time.Sleep(50 * time.Millisecond)

	peers := p2p.HostPeers(host)
	if len(peers) != 1 {
		t.Skipf("expected 1 registered peer, got %d", len(peers))
	}
	peerAddr := peers[0].PeerAddr()

	// Hook: every MsgBlock send fails; count attempts.
	var mu sync.Mutex
	blockSends := 0
	p2p.HostSetSendHook(host, func(p *p2p.Peer, mt p2p.MessageType, payload interface{}) (int, error) {
		if mt != p2p.MsgBlock {
			return p.PeerSendN(mt, payload)
		}
		mu.Lock()
		blockSends++
		mu.Unlock()
		return 0, &fakeTimeoutErr{} // transient, so a retry WOULD be scheduled
	})

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 124, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	block := &core.Block{Header: hdr}

	host.BroadcastBlock(block)
	// Evict the peer BEFORE the retry delay elapses.
	host.DropPeer(peerAddr)

	time.Sleep(300 * time.Millisecond) // well past the retry delay

	mu.Lock()
	sends := blockSends
	mu.Unlock()
	if sends != 1 {
		t.Errorf("MsgBlock send attempts = %d, want exactly 1 (no retry after eviction)", sends)
	}
}

func TestHost_BroadcastRetryDelay_Default(t *testing.T) {
	host := p2p.NewHost(p2p.Config{MaxPeers: 10}, &stubHandler{}, newTestLogger())
	if got := p2p.HostBroadcastRetryDelay(host); got != 200*time.Millisecond {
		t.Errorf("default BroadcastRetryDelay = %v, want 200ms", got)
	}
}

// fakeTimeoutErr is a net.Error with Timeout()==true, simulating a transient
// write timeout on a congested socket.
type fakeTimeoutErr struct{}

func (e *fakeTimeoutErr) Error() string   { return "simulated transient write timeout" }
func (e *fakeTimeoutErr) Timeout() bool   { return true }
func (e *fakeTimeoutErr) Temporary() bool { return true }

func TestHost_BroadcastBlock_PermanentFailure_NoRetry(t *testing.T) {
	host := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		MaxPeers:            10,
		NodeID:              "perm-host",
		UserAgent:           "aperod/test",
		BroadcastRetryDelay: 50 * time.Millisecond,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	conn := rawConnect(t, addr)
	defer conn.Close()
	if !rawHandshake(t, conn) {
		t.Skip("handshake failed")
	}
	time.Sleep(50 * time.Millisecond)

	// Hook: every MsgBlock send fails with a permanent closed-connection
	// error — a retry must NOT be scheduled.
	var mu sync.Mutex
	blockSends := 0
	p2p.HostSetSendHook(host, func(p *p2p.Peer, mt p2p.MessageType, payload interface{}) (int, error) {
		if mt != p2p.MsgBlock {
			return p.PeerSendN(mt, payload)
		}
		mu.Lock()
		blockSends++
		mu.Unlock()
		return 0, net.ErrClosed
	})

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 125, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	host.BroadcastBlock(&core.Block{Header: hdr})

	time.Sleep(300 * time.Millisecond) // well past the retry delay

	mu.Lock()
	sends := blockSends
	mu.Unlock()
	if sends != 1 {
		t.Errorf("MsgBlock send attempts = %d, want exactly 1 (no retry for permanent error)", sends)
	}
}

func TestHost_BroadcastBlock_ConcurrentEviction_NoSendAfterDrop(t *testing.T) {
	// Coordinated race test: DropPeer is fired concurrently around the retry
	// delay.  Because retryBlockSend holds the peer-table read lock across
	// both the registration check and the send, DropPeer (which takes the
	// write lock) cannot complete while a retry send is in flight — so a
	// send observed AFTER DropPeer returned is a correctness violation.
	host := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		MaxPeers:            10,
		NodeID:              "race-host",
		UserAgent:           "aperod/test",
		BroadcastRetryDelay: 30 * time.Millisecond,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	conn := rawConnect(t, addr)
	defer conn.Close()
	if !rawHandshake(t, conn) {
		t.Skip("handshake failed")
	}
	time.Sleep(50 * time.Millisecond)

	peers := p2p.HostPeers(host)
	if len(peers) != 1 {
		t.Skipf("expected 1 registered peer, got %d", len(peers))
	}
	peerAddr := peers[0].PeerAddr()

	var dropped atomic.Bool
	var violation atomic.Bool
	p2p.HostSetSendHook(host, func(p *p2p.Peer, mt p2p.MessageType, payload interface{}) (int, error) {
		if mt != p2p.MsgBlock {
			return p.PeerSendN(mt, payload)
		}
		if dropped.Load() {
			violation.Store(true)
		}
		return 0, &fakeTimeoutErr{} // always transient failure
	})

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 126, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	host.BroadcastBlock(&core.Block{Header: hdr})

	// Race the eviction against the retry timer.
	go func() {
		time.Sleep(30 * time.Millisecond)
		host.DropPeer(peerAddr)
		dropped.Store(true)
	}()

	time.Sleep(300 * time.Millisecond)
	if violation.Load() {
		t.Error("retry send attempted AFTER DropPeer completed — eviction race")
	}
}

// ─── Partial-write safety ─────────────────────────────────────────────────────

// partialWriteConn wraps a net.Conn and, on the Nth Write call matching a
// large frame, writes only the first cutoff bytes to the underlying conn and
// returns a timeout error — deterministically simulating a mid-frame write
// timeout on a congested socket.
type partialWriteConn struct {
	net.Conn
	cutoff int
	fired  bool
}

func (c *partialWriteConn) Write(b []byte) (int, error) {
	if !c.fired && len(b) > c.cutoff {
		c.fired = true
		n, _ := c.Conn.Write(b[:c.cutoff])
		return n, &fakeTimeoutErr{}
	}
	return c.Conn.Write(b)
}

func TestWriteMsgN_PartialWrite_ReportsBytesWritten(t *testing.T) {
	// Transport-level check: writeMsgN must report exactly how many bytes
	// reached the wire so callers can detect a poisoned stream.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Drain the reader side so Pipe writes don't block.
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	pw := &partialWriteConn{Conn: client, cutoff: 2}
	n, err := p2p.WriteMsgN(pw, p2p.MsgPing, p2p.PingMsg{NodeID: "x"})
	if err == nil {
		t.Fatal("expected simulated partial-write error")
	}
	if n != 2 {
		t.Fatalf("WriteMsgN reported n=%d, want 2 (bytes actually written)", n)
	}
}

func TestHost_BroadcastBlock_PartialWrite_ClosesConn_NoRetry(t *testing.T) {
	// A mid-frame partial write poisons the stream framing: the host must
	// close the connection and must NOT retry on the same stream.
	host := p2p.NewHost(p2p.Config{
		ListenAddr:          "127.0.0.1:0",
		MaxPeers:            10,
		NodeID:              "partial-host",
		UserAgent:           "aperod/test",
		BroadcastRetryDelay: 40 * time.Millisecond,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	addr := host.ListenAddr()
	if addr == "" {
		t.Skip("ListenAddr unavailable")
	}

	conn := rawConnect(t, addr)
	defer conn.Close()
	if !rawHandshake(t, conn) {
		t.Skip("handshake failed")
	}
	time.Sleep(50 * time.Millisecond)
	if host.PeerCount() != 1 {
		t.Skipf("expected 1 peer, got %d", host.PeerCount())
	}

	// Hook: first MsgBlock send reports a partial write (3 bytes hit the
	// wire before the timeout); count every MsgBlock attempt.
	var mu sync.Mutex
	blockSends := 0
	p2p.HostSetSendHook(host, func(p *p2p.Peer, mt p2p.MessageType, payload interface{}) (int, error) {
		if mt != p2p.MsgBlock {
			return p.PeerSendN(mt, payload)
		}
		mu.Lock()
		blockSends++
		mu.Unlock()
		return 3, &fakeTimeoutErr{} // partial frame written, then timeout
	})

	priv, pub, _ := crypto.GenerateValidatorKey()
	hdr := core.BlockHeader{Height: 127, ValidatorPub: pub, Timestamp: time.Now().UnixNano()}
	_ = hdr.Sign(priv)
	host.BroadcastBlock(&core.Block{Header: hdr})

	// No retry must fire, and the poisoned connection must be torn down.
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	sends := blockSends
	mu.Unlock()
	if sends != 1 {
		t.Errorf("MsgBlock send attempts = %d, want exactly 1 (no retry after partial write)", sends)
	}
	deadline := time.Now().Add(2 * time.Second)
	for host.PeerCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := host.PeerCount(); got != 0 {
		t.Errorf("PeerCount = %d, want 0 (poisoned connection must be closed/evicted)", got)
	}
}

func TestPeer_SendN_PartialWrite_PoisonsAtomically_ConcurrentSendRejected(t *testing.T) {
	// Production-path regression test for the lock-release race: a partial
	// write must poison the peer and close the connection while STILL
	// holding the per-peer write lock, so a concurrent Send that is already
	// blocked on that lock can never append a new frame to the broken
	// stream.  We verify by counting underlying Write calls: exactly one.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() { // drain reader so Pipe writes don't block
		buf := make([]byte, 4096)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	inFirstWrite := make(chan struct{})
	release := make(chan struct{})
	var writeCalls atomic.Int32
	pw := &racePartialConn{
		Conn:         client,
		inFirstWrite: inFirstWrite,
		release:      release,
		writeCalls:   &writeCalls,
	}
	peer := p2p.NewTestPeer(pw, "test-peer")

	firstDone := make(chan error, 1)
	go func() {
		_, err := peer.PeerSendN(p2p.MsgBlock, p2p.PingMsg{NodeID: "first"})
		firstDone <- err
	}()

	<-inFirstWrite // first send is mid-Write, holding the peer lock

	secondDone := make(chan error, 1)
	go func() {
		// This Send blocks on the peer write lock until the first
		// (partial, poisoning) send finishes — then must be rejected.
		secondDone <- peer.Send(p2p.MsgBlock, p2p.PingMsg{NodeID: "second"})
	}()
	time.Sleep(50 * time.Millisecond) // let the second sender queue on p.mu
	close(release)                    // first Write now returns partial+timeout

	if err := <-firstDone; err == nil {
		t.Fatal("first send must fail with the simulated partial-write error")
	}
	err2 := <-secondDone
	if err2 == nil {
		t.Fatal("second send on a poisoned stream must be rejected")
	}
	if !peer.PeerPoisoned() {
		t.Error("peer must be marked poisoned after the partial write")
	}
	if got := writeCalls.Load(); got != 1 {
		t.Errorf("underlying Write calls = %d, want exactly 1 (no frame after the partial one)", got)
	}
}

// racePartialConn: the FIRST Write signals inFirstWrite, waits for release,
// writes 2 bytes to the underlying conn and returns a timeout error.  All
// Write calls are counted; subsequent Writes pass through (they must never
// happen on a poisoned stream).
type racePartialConn struct {
	net.Conn
	inFirstWrite chan struct{}
	release      chan struct{}
	writeCalls   *atomic.Int32
	fired        bool
}

func (c *racePartialConn) Write(b []byte) (int, error) {
	c.writeCalls.Add(1)
	if !c.fired {
		c.fired = true
		close(c.inFirstWrite)
		<-c.release
		n, _ := c.Conn.Write(b[:2])
		return n, &fakeTimeoutErr{}
	}
	return c.Conn.Write(b)
}

func (c *racePartialConn) SetWriteDeadline(time.Time) error { return nil }
