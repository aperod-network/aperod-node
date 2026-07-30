package p2p_test

// transport_test.go — Tests for the TLS authenticated transport (F-030).
//
// Verifies:
//   T-1: GenerateNodeTLSConfig produces a valid config and non-empty fingerprint.
//   T-2: Two TLS-enabled hosts can connect, complete the handshake, and reach
//        peer count = 1 on each side.
//   T-3: A plain-TCP client connecting to a TLS-enabled host is rejected before
//        any application data is exchanged.
//   T-4: Two hosts with different identities produce different fingerprints.
//   T-5: PeerFingerprint returns "" for a plain net.Conn (unit-test compatibility).

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// ─── T-1: Config generation ───────────────────────────────────────────────────

func TestTLS_GenerateNodeTLSConfig(t *testing.T) {
	cfg, fp, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("GenerateNodeTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("TLSConfig is nil")
	}
	if fp == "" {
		t.Fatal("fingerprint is empty")
	}
	if len(fp) != 64 {
		t.Errorf("fingerprint length = %d, want 64 (hex SHA-256)", len(fp))
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want TLS 1.3 (%d)", cfg.MinVersion, tls.VersionTLS13)
	}
	t.Logf("T-1 ✓ fingerprint=%s tls13=%v", fp, cfg.MinVersion == tls.VersionTLS13)
}

// ─── T-2: TLS host accepts a raw TLS peer that completes the Aperod handshake ─
//
// Two p2p.Host instances cannot be directly connected because both sides
// send MsgPing first — a known protocol constraint documented in host_test.go.
// Instead we start one TLS-enabled Host and use a raw *tls.Conn peer that
// follows the correct initiator role: receive MsgPing from the host, reply
// MsgPong.  This exercises:
//   • the full TLS 1.3 handshake with mutual certificates
//   • the Aperod application handshake running over the encrypted channel
//   • PeerFingerprint returning the peer's authenticated identity

func TestTLS_MutualAuth_HostAndRawPeer(t *testing.T) {
	// Host identity.
	cfgA, fpA, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config A: %v", err)
	}

	hostA := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "node-a",
		UserAgent:  "aperod/test",
		TLSConfig:  cfgA,
	}, &stubHandler{}, newTestLogger())
	if err := hostA.Start(); err != nil {
		t.Fatalf("hostA.Start: %v", err)
	}
	t.Cleanup(hostA.Stop)

	// Raw peer identity — a second independent TLS certificate.
	cfgB, fpB, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config B: %v", err)
	}

	// Dial using the raw TLS dialer so the full TLS handshake (including
	// mutual certificate exchange) completes before we send any Aperod data.
	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", hostA.ListenAddr(), cfgB,
	)
	if err != nil {
		t.Fatalf("T-2: tls.DialWithDialer failed — TLS handshake not established: %v", err)
	}
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck

	// The host sends MsgPing first; the raw peer must reply with MsgPong.
	msgType, _, err := p2p.ReadMsg(tlsConn)
	if err != nil {
		t.Fatalf("T-2: read from TLS conn: %v", err)
	}
	if msgType != p2p.MsgPing {
		t.Fatalf("T-2: expected MsgPing, got %v", msgType)
	}
	if err := p2p.WriteMsg(tlsConn, p2p.MsgPong, p2p.PingMsg{
		NodeID: "raw-tls-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-2: write pong: %v", err)
	}

	// Give the host time to register the peer.
	time.Sleep(100 * time.Millisecond)

	if hostA.PeerCount() != 1 {
		t.Errorf("T-2: hostA.PeerCount() = %d, want 1 — TLS peer not registered", hostA.PeerCount())
	}

	// The peer's fingerprint on our (client) side is the host's public key.
	clientFP := p2p.PeerFingerprint(tlsConn)
	if clientFP == "" {
		t.Error("T-2: PeerFingerprint returned empty string for TLS conn — cert missing?")
	}
	if clientFP == fpB {
		// Sanity: the host's fingerprint must differ from our own.
		t.Error("T-2: host fingerprint equals our own fingerprint — identity reuse?")
	}

	t.Logf("T-2 ✓ TLS peer connected: hostA peers=%d fpA=%s… fpB(peer)=%s… clientSawFP=%s…",
		hostA.PeerCount(), fpA[:8], fpB[:8], clientFP[:8])
}

// ─── T-3: Plain TCP client rejected by TLS host ───────────────────────────────

func TestTLS_PlaintextClientRejected(t *testing.T) {
	tlsCfg, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config: %v", err)
	}
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "tls-node",
		UserAgent:  "aperod/test",
		TLSConfig:  tlsCfg,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	// Connect with a raw plain-TCP client (no TLS).
	conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
	if err != nil {
		t.Logf("T-3 ✓ plain-TCP connection refused at OS level: %v", err)
		return
	}
	defer conn.Close()

	// The TLS host must reject the plain-text connection during its eager
	// TLS handshake.  It should either close the connection or never respond
	// with valid application data.
	conn.SetDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 256)
	n, readErr := conn.Read(buf)

	if readErr != nil {
		// Connection closed — expected when TLS handshake fails.
		t.Logf("T-3 ✓ plain-TCP connection rejected by TLS host (read err: %v, n=%d)", readErr, n)
	} else {
		// Some TLS alert bytes may arrive before the close.
		t.Logf("T-3 ✓ TLS host sent %d bytes (likely TLS alert) then closed; no Aperod data", n)
	}

	// Key assertion: the host must not count the rejected connection as a peer.
	time.Sleep(100 * time.Millisecond)
	if host.PeerCount() > 0 {
		t.Errorf("T-3: plain-TCP client was accepted as a peer — TLS guard not working")
	}
}

// ─── T-4: Different identities → different fingerprints ──────────────────────

func TestTLS_DifferentIdentities(t *testing.T) {
	_, fp1, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config 1: %v", err)
	}
	_, fp2, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config 2: %v", err)
	}
	if fp1 == fp2 {
		t.Error("T-4: two independently generated identities have identical fingerprints")
	} else {
		t.Logf("T-4 ✓ distinct fingerprints: %s… vs %s…", fp1[:8], fp2[:8])
	}
}

// ─── T-5: PeerFingerprint on plain net.Conn returns "" ───────────────────────

// ─── T-6: AllowedPeers — unlisted fingerprint is rejected ────────────────────
//
// Start a TLS host with AllowedPeers set to a known good fingerprint (fpGood).
// Connect with a different identity (fpBad) and verify:
//   - The TLS handshake itself completes (mutual auth works).
//   - The host closes the connection before the Aperod application handshake
//     (peer is never registered in h.peers).
//
// Then connect with the allowed identity (fpGood) and verify the peer IS
// registered — confirming the allow-list only blocks unknown fingerprints.

func TestTLS_AllowedPeers_UnlistedRejected(t *testing.T) {
	// "good" identity — will be on the allow-list.
	cfgGood, fpGood, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config good: %v", err)
	}

	// Host identity.
	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config host: %v", err)
	}

	host := p2p.NewHost(p2p.Config{
		ListenAddr:   "127.0.0.1:0",
		MaxPeers:     5,
		NodeID:       "allowlist-node",
		UserAgent:    "aperod/test",
		TLSConfig:    cfgHost,
		AllowedPeers: []string{fpGood}, // only fpGood is allowed
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	addr := host.ListenAddr()

	// ── Attempt 1: connect with an UNLISTED identity ──────────────────────────
	cfgBad, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config bad: %v", err)
	}
	badConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", addr, cfgBad,
	)
	if err != nil {
		// Host closed the connection during TLS handshake itself — also acceptable.
		t.Logf("T-6 ✓ unlisted peer rejected at TLS handshake level: %v", err)
	} else {
		defer badConn.Close()
		badConn.SetDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		// The host should close the connection without sending a MsgPing.
		buf := make([]byte, 8)
		n, readErr := badConn.Read(buf)
		if readErr == nil && n > 0 {
			// Host unexpectedly sent data — check peer count below.
			t.Logf("T-6: host sent %d bytes to unlisted peer (unexpected)", n)
		} else {
			t.Logf("T-6 ✓ unlisted peer connection closed by host after TLS handshake (n=%d err=%v)", n, readErr)
		}
	}

	time.Sleep(100 * time.Millisecond)

	if host.PeerCount() != 0 {
		t.Errorf("T-6: unlisted peer was registered — AllowedPeers not enforced (peer count = %d)", host.PeerCount())
	}

	// ── Attempt 2: connect with the LISTED identity ───────────────────────────
	goodConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", addr, cfgGood,
	)
	if err != nil {
		t.Fatalf("T-6: allowed peer TLS dial failed: %v", err)
	}
	defer goodConn.Close()
	goodConn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

	// Complete the Aperod handshake: host sends MsgPing, peer replies MsgPong.
	msgType, _, err := p2p.ReadMsg(goodConn)
	if err != nil || msgType != p2p.MsgPing {
		t.Fatalf("T-6: expected MsgPing from host, got %v err=%v", msgType, err)
	}
	if err := p2p.WriteMsg(goodConn, p2p.MsgPong, p2p.PingMsg{
		NodeID: "good-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-6: write pong: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if host.PeerCount() != 1 {
		t.Errorf("T-6: allowed peer was NOT registered (peer count = %d, want 1)", host.PeerCount())
	} else {
		t.Logf("T-6 ✓ AllowedPeers enforced: unlisted=rejected allowed=registered fpGood=%s…", fpGood[:8])
	}
}

// ─── T-7: AllowedPeers empty = open network ───────────────────────────────────
//
// When AllowedPeers is empty (nil), all peers with valid TLS credentials
// should be accepted — existing default behaviour is preserved.

func TestTLS_AllowedPeers_EmptyMeansOpen(t *testing.T) {
	cfgPeer, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config peer: %v", err)
	}
	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config host: %v", err)
	}

	// No AllowedPeers set — open network.
	host := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "open-node",
		UserAgent:  "aperod/test",
		TLSConfig:  cfgHost,
		// AllowedPeers intentionally omitted
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", host.ListenAddr(), cfgPeer,
	)
	if err != nil {
		t.Fatalf("T-7: tls dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPing {
		t.Fatalf("T-7: expected MsgPing, got %v err=%v", msgType, err)
	}
	if err := p2p.WriteMsg(conn, p2p.MsgPong, p2p.PingMsg{
		NodeID: "any-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-7: write pong: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if host.PeerCount() != 1 {
		t.Errorf("T-7: open-network peer was NOT accepted (peer count = %d, want 1)", host.PeerCount())
	} else {
		t.Log("T-7 ✓ empty AllowedPeers = open network; arbitrary TLS peer accepted")
	}
}

func TestTLS_PeerFingerprint_PlainConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	fp := p2p.PeerFingerprint(a)
	if fp != "" {
		t.Errorf("T-5: PeerFingerprint on plain net.Conn = %q, want \"\"", fp)
	}
	t.Log("T-5 ✓ PeerFingerprint returns empty string for plain net.Conn")
}

// ─── T-8: Forged-cert peer cannot inject a block ──────────────────────────────
//
// Security regression test: a peer that presents a self-signed certificate
// that is NOT on the host's AllowedPeers list must not be able to inject a
// block into the chain via MsgBlock, even though the TLS handshake itself
// succeeds (mutual TLS authenticates the cert as valid, but the fingerprint
// is not recognised).
//
// Done looks like:
//   - handler.OnBlock is never called after the attacker sends MsgBlock.
//   - The host registers 0 peers (the attacker was dropped).

func TestTLS_ForgedCert_CannotInjectBlock(t *testing.T) {
	// "good" identity placed on the allow-list (never actually connects here).
	_, fpGood, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config good: %v", err)
	}

	// Host identity.
	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config host: %v", err)
	}

	handler := &stubHandler{}
	host := p2p.NewHost(p2p.Config{
		ListenAddr:   "127.0.0.1:0",
		MaxPeers:     5,
		NodeID:       "secure-node",
		UserAgent:    "aperod/test",
		TLSConfig:    cfgHost,
		AllowedPeers: []string{fpGood}, // attacker's fingerprint is NOT here
	}, handler, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	// Attacker generates a fresh self-signed certificate (unknown fingerprint).
	cfgAttacker, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("gen config attacker: %v", err)
	}

	attackConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", host.ListenAddr(), cfgAttacker,
	)
	if err != nil {
		// Host may have closed the connection at TLS handshake level — still a
		// successful rejection; block injection is impossible.
		t.Logf("T-8 ✓ attacker rejected at TLS handshake level: %v", err)
		time.Sleep(100 * time.Millisecond)
		if host.PeerCount() != 0 {
			t.Errorf("T-8: attacker registered as peer after TLS rejection (count=%d)", host.PeerCount())
		}
		if len(handler.blocks) != 0 {
			t.Errorf("T-8: OnBlock called %d time(s) despite TLS rejection", len(handler.blocks))
		}
		return
	}
	defer attackConn.Close()

	// TLS handshake completed — the host will now reject the connection at the
	// application layer (AllowedPeers check) and close it.  The attacker
	// attempts to inject a block by sending MsgBlock immediately.
	attackConn.SetDeadline(time.Now().Add(time.Second)) //nolint:errcheck

	// Build a minimal but structurally valid block to send.
	priv, pub, genErr := crypto.GenerateValidatorKey()
	if genErr != nil {
		t.Fatalf("T-8: generate validator key: %v", genErr)
	}
	hdr := core.BlockHeader{
		Height:       1,
		ValidatorPub: pub,
		Timestamp:    time.Now().UnixNano(),
		MerkleRoot:   core.MerkleRoot(nil),
	}
	_ = hdr.Sign(priv)
	injectedBlock := &core.Block{Header: hdr}
	sb := p2p.BlockToMsg(injectedBlock)

	// Write MsgBlock — ignore write errors; the connection may already be
	// closing on the host side.
	_ = p2p.WriteMsg(attackConn, p2p.MsgBlock, sb)

	// Give the host enough time to process the write (if it even reads it).
	time.Sleep(200 * time.Millisecond)

	// ── Key assertions ────────────────────────────────────────────────────────

	if host.PeerCount() != 0 {
		t.Errorf("T-8: attacker was registered as a peer (count=%d, want 0)", host.PeerCount())
	}

	if len(handler.blocks) != 0 {
		t.Errorf("T-8: OnBlock was called %d time(s) — forged-cert peer injected a block", len(handler.blocks))
	} else {
		t.Logf("T-8 ✓ forged-cert peer could not inject block: OnBlock=0 peers=%d", host.PeerCount())
	}
}
