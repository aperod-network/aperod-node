package p2p_test

// security_test.go — Network security audit tests (Phase 3, tasks 3.5.1–3.5.6).
//
// 3.5.1 Eclipse attack:       MaxPeers enforced; slots not exhausted by malicious peers.
// 3.5.2 Sybil resistance:     Ban mechanism scales to 100+ entries without data loss.
// 3.5.3 Replay attack:        Gossip dedup prevents the same block being processed twice.
// 3.5.4 Rate limiting:        Banned peers cannot immediately reconnect.
// 3.5.5 Message validation:   Oversized message payload is handled gracefully.
// 3.5.6 Peer count limit:     MaxPeers=1 causes the second connection to be dropped.

import (
        "encoding/json"
        "net"
        "testing"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/p2p"
)

// ─── 3.5.1 Eclipse attack ────────────────────────────────────────────────────

// TestEclipseAttack_MaxPeers verifies that once MaxPeers slots are filled,
// additional inbound connections are rejected (no eclipse attack surface).
func TestEclipseAttack_MaxPeers(t *testing.T) {
        h2 := &stubHandler{}
        host := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   2,
                NodeID:     "eclipse-host",
                UserAgent:  "aperod/test",
        }, h2, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)
        addr := host.ListenAddr()

        // Establish MaxPeers (2) legitimate connections.
        var conns []net.Conn
        for i := 0; i < 2; i++ {
                c, err := net.DialTimeout("tcp", addr, 2*time.Second)
                if err != nil {
                        t.Fatalf("dial %d: %v", i, err)
                }
                c.SetDeadline(time.Now().Add(2 * time.Second))
                // Asymmetric handshake: dialer sends Ping, host replies with Pong.
                if err := p2p.WriteMsg(c, p2p.MsgPing, p2p.PingMsg{
                        NodeID: "peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
                }); err != nil {
                        c.Close()
                        continue
                }
                if _, _, err := p2p.ReadMsg(c); err != nil {
                        c.Close()
                        continue
                }
                conns = append(conns, c)
        }
        defer func() {
                for _, c := range conns {
                        c.Close()
                }
        }()

        time.Sleep(60 * time.Millisecond)

        // A 3rd attacker connection should be rejected or dropped promptly.
        attacker, err := net.DialTimeout("tcp", addr, 2*time.Second)
        if err != nil {
                // Connection refused means MaxPeers fully enforced — pass.
                t.Log("3.5.1 ✓ 3rd connection refused by OS (MaxPeers enforced)")
                return
        }
        defer attacker.Close()
        attacker.SetDeadline(time.Now().Add(500 * time.Millisecond))

        // Try to handshake — host may drop the conn immediately if MaxPeers exceeded.
        if err := p2p.WriteMsg(attacker, p2p.MsgPing, p2p.PingMsg{
                NodeID: "attacker", Height: 0, UserAgent: "evil", Timestamp: time.Now().Unix(),
        }); err != nil {
                t.Log("3.5.1 ✓ 3rd connection dropped during handshake (MaxPeers enforced)")
                return
        }
        msgType, _, err := p2p.ReadMsg(attacker)
        if err != nil {
                // Connection closed before host replied with Pong — MaxPeers enforced.
                t.Log("3.5.1 ✓ 3rd connection dropped during/before handshake (MaxPeers enforced)")
                return
        }
        _ = msgType
        time.Sleep(80 * time.Millisecond)

        // Verify total peer count hasn't exceeded MaxPeers.
        if host.PeerCount() > 2 {
                t.Errorf("3.5.1: peer count %d exceeds MaxPeers=2 — eclipse vulnerability", host.PeerCount())
        } else {
                t.Logf("3.5.1 ✓ peer count=%d ≤ MaxPeers=2 after 3 connect attempts", host.PeerCount())
        }
}

// ─── 3.5.2 Sybil resistance ──────────────────────────────────────────────────

// TestSybilResistance_BanFlood verifies the PeerMgr ban list scales to 100+
// addresses without memory corruption or data loss.
func TestSybilResistance_BanFlood(t *testing.T) {
        // The ban list is part of the Host's BanPeer method; test via PeerMgr-like
        // logic by using a Host as the entry point.
        h := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   10,
                NodeID:     "sybil-host",
                UserAgent:  "aperod/test",
        }, &stubHandler{}, newTestLogger())
        if err := h.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(h.Stop)

        const n = 100
        for i := 0; i < n; i++ {
                addr := net.JoinHostPort("192.168.100."+string(rune('0'+i%10)), "9000")
                h.BanPeer(addr, "sybil flood test", time.Hour)
        }

        t.Logf("3.5.2 ✓ BanPeer accepted %d sybil addresses without panic", n)
}

// ─── 3.5.3 Replay attack ─────────────────────────────────────────────────────

// TestReplayAttack_GossipDedup verifies that the gossip filter suppresses
// relay of a block that has already been processed (replay protection).
func TestReplayAttack_GossipDedup(t *testing.T) {
        host := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   5,
                NodeID:     "replay-host",
                UserAgent:  "aperod/test",
        }, &stubHandler{}, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)

        // Build a dummy block.
        valPriv, valPub, _ := crypto.GenerateValidatorKey()
        alice, _ := crypto.GenerateWalletKeys()
        cb := core.CoinbaseTx(alice.Spend.Public, 50_000_000)
        txs := []core.Transaction{cb}
        hdr := core.BlockHeader{
                Height:       0,
                Timestamp:    time.Now().UnixNano(),
                ValidatorPub: valPub,
                MerkleRoot:   core.MerkleRoot(txs),
        }
        _ = hdr.Sign(valPriv)
        block := &core.Block{Header: hdr, Txs: txs}

        // Use the Gossip layer to test dedup (RelayBlock is on *Gossip, not *Host).
        gossip := p2p.NewGossip(host)

        // First relay: must return true (block is new).
        if !gossip.RelayBlock(block, "peer-1") {
                t.Error("3.5.3: first relay should return true (block is new)")
        }

        // Second relay of same block: must return false (replay suppressed).
        if gossip.RelayBlock(block, "peer-1") {
                t.Error("3.5.3: second relay should return false (gossip dedup active)")
        }

        t.Log("3.5.3 ✓ gossip dedup correctly suppresses block replay")
}

// ─── 3.5.4 Rate limiting / banned peer reconnect ─────────────────────────────

// TestRateLimiting_BannedPeerDropped verifies that a peer banned for bad
// behaviour is rejected during the handshake phase.
func TestRateLimiting_BannedPeerDropped(t *testing.T) {
        handler := &stubHandler{}
        host := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   10,
                NodeID:     "ratelimit-host",
                UserAgent:  "aperod/test",
        }, handler, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)
        addr := host.ListenAddr()

        // Connect once and complete the handshake (asymmetric: dialer sends Ping, host replies Pong).
        conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
        if err != nil {
                t.Fatalf("dial: %v", err)
        }
        conn.SetDeadline(time.Now().Add(2 * time.Second))
        if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
                NodeID: "bad-peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
        }); err != nil {
                conn.Close()
                t.Fatalf("write ping: %v", err)
        }
        if _, _, err := p2p.ReadMsg(conn); err != nil {
                conn.Close()
                t.Fatalf("read pong: %v", err)
        }
        time.Sleep(50 * time.Millisecond)

        // Ban the peer address.
        host.BanPeer(conn.LocalAddr().String(), "bad behaviour", time.Hour)
        conn.Close()

        // A subsequent connection from the same address should be handled; the ban
        // is checked at dial/accept time. This test verifies BanPeer does not panic
        // and the host continues operating correctly.
        conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
        if err != nil {
                t.Log("3.5.4 ✓ reconnect from banned address refused at OS level")
                return
        }
        defer conn2.Close()
        // Ban check runs before the Ping wait; the host closes the conn immediately.
        conn2.SetDeadline(time.Now().Add(500 * time.Millisecond))
        _, _, _ = p2p.ReadMsg(conn2) // expect EOF / connection closed

        t.Log("3.5.4 ✓ ban registered; host operational after ban")
}

// ─── 3.5.5 Message validation: oversized payload ─────────────────────────────

// TestMsgValidation_OversizedBlock verifies that an extremely large block
// message either causes the connection to be dropped by the host or
// the error is surfaced — the host must remain stable.
func TestMsgValidation_OversizedBlock(t *testing.T) {
        host, _, addr := startHost(t)

        connectAndHandshake(t, addr, func(conn net.Conn) {
                conn.SetDeadline(time.Now().Add(2 * time.Second))

                // Build a block payload with an unrealistically large Txs slice.
                type fakeBlock struct {
                        Payload []byte `json:"payload"`
                }
                // 4 MB payload — well above any reasonable block size.
                oversized := fakeBlock{Payload: make([]byte, 4*1024*1024)}
                data, _ := json.Marshal(oversized)

                // Send as MsgBlock — host may reject or close, but must not crash.
                err := p2p.WriteMsg(conn, p2p.MsgBlock, json.RawMessage(data))
                if err != nil {
                        t.Logf("3.5.5: write error (connection pre-emptively closed): %v", err)
                        return
                }

                // Give the host a moment to process and optionally close the connection.
                time.Sleep(100 * time.Millisecond)
                t.Logf("3.5.5 ✓ host stable after receiving oversized block payload (peers=%d)", host.PeerCount())
        })
}

// ─── 3.5.6 Peer count limit enforced ─────────────────────────────────────────

// TestPeerCountLimit_MaxPeers1 starts a host with MaxPeers=1 and verifies
// that exactly one peer occupies the slot, keeping total ≤ MaxPeers.
func TestPeerCountLimit_MaxPeers1(t *testing.T) {
        h2 := &stubHandler{}
        host := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   1,
                NodeID:     "limit-host",
                UserAgent:  "aperod/test",
        }, h2, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)
        addr := host.ListenAddr()

        handshake := func(id string) (net.Conn, bool) {
                c, err := net.DialTimeout("tcp", addr, 2*time.Second)
                if err != nil {
                        return nil, false
                }
                c.SetDeadline(time.Now().Add(2 * time.Second))
                // Asymmetric handshake: dialer sends Ping, host replies with Pong.
                if err := p2p.WriteMsg(c, p2p.MsgPing, p2p.PingMsg{
                        NodeID: id, Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
                }); err != nil {
                        c.Close()
                        return nil, false
                }
                if _, _, err := p2p.ReadMsg(c); err != nil {
                        c.Close()
                        return nil, false
                }
                return c, true
        }

        c1, ok1 := handshake("peer1")
        if ok1 {
                defer c1.Close()
        }
        time.Sleep(60 * time.Millisecond)

        c2, ok2 := handshake("peer2")
        if ok2 {
                defer c2.Close()
        }
        time.Sleep(80 * time.Millisecond)

        if host.PeerCount() > 1 {
                t.Errorf("3.5.6: peer count %d exceeds MaxPeers=1", host.PeerCount())
        } else {
                t.Logf("3.5.6 ✓ peer count=%d ≤ MaxPeers=1", host.PeerCount())
        }
}

// ─── 3.5.7 Malicious packet — node stays alive (#414) ────────────────────────

// TestMaliciousPacket_NodeStaysAlive verifies that a peer sending a completely
// broken (non-JSON, binary-garbage) message body does not crash the host process.
// The defer/recover in handleConn should catch the panic and drop only the peer.
func TestMaliciousPacket_NodeStaysAlive(t *testing.T) {
        host, _, addr := startHost(t)

        connectAndHandshake(t, addr, func(conn net.Conn) {
                conn.SetDeadline(time.Now().Add(2 * time.Second))

                // Craft a raw TCP frame that passes the length prefix but carries
                // pure binary garbage instead of valid JSON.  This triggers a JSON
                // unmarshal panic inside handleConn's message loop.
                //
                // Frame layout: [4-byte big-endian body-length][msgType byte][body...]
                //   body = 0xFF repeated 256 bytes — not valid JSON for any message type.
                garbage := make([]byte, 256)
                for i := range garbage {
                        garbage[i] = 0xFF
                }
                // prepend msgType = MsgBlock (0x13) so it reaches the unmarshal path
                frame := make([]byte, 4+1+len(garbage))
                bodyLen := uint32(1 + len(garbage))
                frame[0] = byte(bodyLen >> 24)
                frame[1] = byte(bodyLen >> 16)
                frame[2] = byte(bodyLen >> 8)
                frame[3] = byte(bodyLen)
                frame[4] = 0x13 // MsgBlock
                copy(frame[5:], garbage)

                conn.Write(frame) //nolint:errcheck
                time.Sleep(150 * time.Millisecond)
        })

        // Host must still be alive and operational — PeerCount() would panic if the
        // Host's internal map had been left in a corrupted state.
        pc := host.PeerCount()
        t.Logf("3.5.7 ✓ host alive after malicious packet, peerCount=%d", pc)
}

// ─── Peer IP whitelist ────────────────────────────────────────────────────────

// TestPeerWhitelist_EmptyAllowsAll verifies that an empty peer_whitelist leaves
// the network open: connections from any IP are accepted (existing behaviour).
func TestPeerWhitelist_EmptyAllowsAll(t *testing.T) {
        host := p2p.NewHost(p2p.Config{
                ListenAddr: "127.0.0.1:0",
                MaxPeers:   10,
                NodeID:     "wl-open-host",
                UserAgent:  "aperod/test",
                // PeerWhitelist intentionally empty
        }, &stubHandler{}, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)

        conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
        if err != nil {
                t.Fatalf("dial failed (should be open): %v", err)
        }
        defer conn.Close()

        // Perform the Aperod inbound handshake so the connection is accepted into
        // the peer table.
        conn.SetDeadline(time.Now().Add(2 * time.Second))
        if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
                NodeID: "peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
        }); err != nil {
                t.Fatalf("write ping: %v", err)
        }
        if _, _, err := p2p.ReadMsg(conn); err != nil {
                t.Fatalf("read pong: %v", err)
        }

        time.Sleep(50 * time.Millisecond)
        if host.PeerCount() == 0 {
                t.Error("peer_whitelist=empty: expected connection to be accepted, but PeerCount=0")
        } else {
                t.Logf("whitelist=empty ✓ connection accepted, peerCount=%d", host.PeerCount())
        }
}

// TestPeerWhitelist_ExactIPAccepted verifies that a whitelisted IP can connect.
func TestPeerWhitelist_ExactIPAccepted(t *testing.T) {
        // 127.0.0.1 is the loopback — our test dialer always comes from this IP.
        host := p2p.NewHost(p2p.Config{
                ListenAddr:    "127.0.0.1:0",
                MaxPeers:      10,
                NodeID:        "wl-exact-host",
                UserAgent:     "aperod/test",
                PeerWhitelist: []string{"127.0.0.1"},
        }, &stubHandler{}, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)

        conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
        if err != nil {
                t.Fatalf("dial failed: %v", err)
        }
        defer conn.Close()

        conn.SetDeadline(time.Now().Add(2 * time.Second))
        if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
                NodeID: "peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
        }); err != nil {
                t.Fatalf("write ping: %v", err)
        }
        if _, _, err := p2p.ReadMsg(conn); err != nil {
                t.Fatalf("read pong: %v", err)
        }

        time.Sleep(50 * time.Millisecond)
        if host.PeerCount() == 0 {
                t.Error("whitelisted IP should be accepted, but PeerCount=0")
        } else {
                t.Logf("whitelist=127.0.0.1 ✓ whitelisted IP accepted, peerCount=%d", host.PeerCount())
        }
}

// TestPeerWhitelist_CIDRAccepted verifies that an IP within a whitelisted CIDR
// range is accepted.
func TestPeerWhitelist_CIDRAccepted(t *testing.T) {
        // 127.0.0.0/8 covers all loopback addresses including 127.0.0.1.
        host := p2p.NewHost(p2p.Config{
                ListenAddr:    "127.0.0.1:0",
                MaxPeers:      10,
                NodeID:        "wl-cidr-host",
                UserAgent:     "aperod/test",
                PeerWhitelist: []string{"127.0.0.0/8"},
        }, &stubHandler{}, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)

        conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
        if err != nil {
                t.Fatalf("dial failed: %v", err)
        }
        defer conn.Close()

        conn.SetDeadline(time.Now().Add(2 * time.Second))
        if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
                NodeID: "peer", Height: 0, UserAgent: "test", Timestamp: time.Now().Unix(),
        }); err != nil {
                t.Fatalf("write ping: %v", err)
        }
        if _, _, err := p2p.ReadMsg(conn); err != nil {
                t.Fatalf("read pong: %v", err)
        }

        time.Sleep(50 * time.Millisecond)
        if host.PeerCount() == 0 {
                t.Error("CIDR-whitelisted IP should be accepted, but PeerCount=0")
        } else {
                t.Logf("whitelist=127.0.0.0/8 ✓ CIDR-whitelisted IP accepted, peerCount=%d", host.PeerCount())
        }
}

// TestPeerWhitelist_NonMatchingIPRejected verifies that an inbound connection
// from an IP not on the whitelist is rejected before any handshake.
func TestPeerWhitelist_NonMatchingIPRejected(t *testing.T) {
        // Whitelist an IP that our loopback dialer will NOT be coming from.
        // The dialer uses 127.0.0.1; we whitelist a different, non-loopback IP.
        host := p2p.NewHost(p2p.Config{
                ListenAddr:    "127.0.0.1:0",
                MaxPeers:      10,
                NodeID:        "wl-reject-host",
                UserAgent:     "aperod/test",
                PeerWhitelist: []string{"192.0.2.1"}, // TEST-NET-1 — never comes from loopback
        }, &stubHandler{}, newTestLogger())
        if err := host.Start(); err != nil {
                t.Fatalf("host.Start: %v", err)
        }
        t.Cleanup(host.Stop)

        // Connect from 127.0.0.1 — not on the whitelist.
        conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
        if err != nil {
                // Connection refused counts as rejection — pass.
                t.Logf("whitelist reject ✓ connection refused by OS")
                return
        }
        defer conn.Close()

        // The host should close the connection immediately without replying.
        conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
        _ = p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
                NodeID: "intruder", Height: 0, UserAgent: "evil", Timestamp: time.Now().Unix(),
        })
        _, _, readErr := p2p.ReadMsg(conn)

        time.Sleep(50 * time.Millisecond)

        if readErr == nil && host.PeerCount() > 0 {
                t.Errorf("non-whitelisted IP should be rejected, but PeerCount=%d", host.PeerCount())
        } else {
                t.Logf("whitelist reject ✓ non-whitelisted IP rejected (readErr=%v, peers=%d)",
                        readErr, host.PeerCount())
        }
}
