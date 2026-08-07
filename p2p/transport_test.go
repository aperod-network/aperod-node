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
// Uses the asymmetric handshake: raw TLS dialer (inbound peer) sends MsgPing
// first; TLS host (acceptor) replies with MsgPong.  This exercises:
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

	// Asymmetric handshake: dialer (us) sends MsgPing first; host replies MsgPong.
	if err := p2p.WriteMsg(tlsConn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "raw-tls-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-2: write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(tlsConn)
	if err != nil {
		t.Fatalf("T-2: read pong from TLS conn: %v", err)
	}
	if msgType != p2p.MsgPong {
		t.Fatalf("T-2: expected MsgPong, got %v", msgType)
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

	// Complete the Aperod handshake: dialer sends MsgPing, host replies MsgPong.
	if err := p2p.WriteMsg(goodConn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "good-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-6: write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(goodConn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("T-6: expected MsgPong from host, got %v err=%v", msgType, err)
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

	// Asymmetric handshake: dialer sends MsgPing, host replies MsgPong.
	if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "any-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-7: write ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil || msgType != p2p.MsgPong {
		t.Fatalf("T-7: expected MsgPong, got %v err=%v", msgType, err)
	}

	time.Sleep(100 * time.Millisecond)

	if host.PeerCount() != 1 {
		t.Errorf("T-7: open-network peer was NOT accepted (peer count = %d, want 1)", host.PeerCount())
	} else {
		t.Log("T-7 ✓ empty AllowedPeers = open network; arbitrary TLS peer accepted")
	}
}

// ─── T-9: LoadOrSaveP2PIdentity — persistence across calls ───────────────────
//
// Verifies that:
//   - First call (no file) generates a key, saves it (isNew=true).
//   - Second call loads the same key and returns the identical fingerprint
//     (isNew=false).
//   - Calling with resetIdentity=true regenerates the key (different
//     fingerprint, isNew=true).

func TestLoadOrSaveP2PIdentity_Persistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/p2p_identity.key"

	// First call — key file does not yet exist.
	cfg1, fp1, isNew1, err := p2p.LoadOrSaveP2PIdentity(keyPath, false)
	if err != nil {
		t.Fatalf("T-9 first call: %v", err)
	}
	if !isNew1 {
		t.Error("T-9: first call should return isNew=true (no prior key file)")
	}
	if cfg1 == nil || fp1 == "" {
		t.Fatal("T-9: first call returned nil config or empty fingerprint")
	}

	// Second call — key file now exists; must return the same fingerprint.
	cfg2, fp2, isNew2, err := p2p.LoadOrSaveP2PIdentity(keyPath, false)
	if err != nil {
		t.Fatalf("T-9 second call: %v", err)
	}
	if isNew2 {
		t.Error("T-9: second call should return isNew=false (key file already exists)")
	}
	if fp2 != fp1 {
		t.Errorf("T-9: fingerprint changed across restarts: %s → %s", fp1[:8], fp2[:8])
	}
	if cfg2 == nil {
		t.Fatal("T-9: second call returned nil config")
	}

	// Reset call — must generate a fresh key (different fingerprint).
	_, fp3, isNew3, err := p2p.LoadOrSaveP2PIdentity(keyPath, true)
	if err != nil {
		t.Fatalf("T-9 reset call: %v", err)
	}
	if !isNew3 {
		t.Error("T-9: reset call should return isNew=true")
	}
	if fp3 == fp1 {
		t.Error("T-9: reset did not change the fingerprint")
	}

	t.Logf("T-9 ✓ identity persisted: fp1=%s… fp2=%s… fp3(reset)=%s…", fp1[:8], fp2[:8], fp3[:8])
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

// ─── T-10: Banned IP rejected even when its certificate is on AllowedPeers ────
//
// Security regression guard: the ban-list check in handleConn MUST run before
// the AllowedPeers (TLS fingerprint) check.  If these checks are ever
// accidentally reordered, a banned attacker holding a valid certificate would
// slip through.
//
// Test sequence:
//  1. Generate the attacker's TLS identity (cfgAttacker / fpAttacker).
//  2. Start a host with AllowedPeers containing ONLY fpAttacker — so the cert
//     IS explicitly allowed.
//  3. Pre-ban the loopback IP "127.0.0.1" (bare IP) before any connection is
//     made — this simulates an operator banning the attacker's host.
//  4. Attacker dials with cfgAttacker (the allowed cert).
//  5. Assert PeerCount == 0: the ban gate fires first and the connection is
//     dropped regardless of the valid certificate.

func TestTLS_BannedIPRejectedDespiteValidCert(t *testing.T) {
	// Attacker identity — will be placed on AllowedPeers.
	cfgAttacker, fpAttacker, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("T-10: gen attacker config: %v", err)
	}

	// Host identity.
	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("T-10: gen host config: %v", err)
	}

	host := p2p.NewHost(p2p.Config{
		ListenAddr:   "127.0.0.1:0",
		MaxPeers:     5,
		NodeID:       "ban-test-host",
		UserAgent:    "aperod/test",
		TLSConfig:    cfgHost,
		AllowedPeers: []string{fpAttacker}, // attacker's cert IS on the allow-list
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("T-10: host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	// Pre-ban the loopback IP before the attacker connects.
	// Using the bare IP (no port) so that any ephemeral source port from
	// 127.0.0.1 is blocked — this tests the IP-level ban path in PeerMgr.
	host.BanPeer("127.0.0.1", "attacker ip banned", time.Hour)

	// Attacker dials with their allowed certificate.
	attackConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp", host.ListenAddr(), cfgAttacker,
	)
	if err != nil {
		// Host closed the connection before or during the TLS handshake — this
		// is an acceptable (and stronger) form of rejection.
		t.Logf("T-10 ✓ banned attacker rejected at/before TLS handshake: %v", err)
	} else {
		defer attackConn.Close()
		// TLS handshake completed; the host must close the connection immediately
		// at the ban check without sending any Aperod data.
		attackConn.SetDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		buf := make([]byte, 8)
		n, readErr := attackConn.Read(buf)
		if readErr != nil {
			t.Logf("T-10 ✓ banned attacker connection closed by host after TLS handshake (n=%d err=%v)", n, readErr)
		} else {
			t.Logf("T-10: host sent %d bytes to banned peer (unexpected — checking peer count)", n)
		}
	}

	time.Sleep(100 * time.Millisecond)

	if host.PeerCount() != 0 {
		t.Errorf("T-10: banned IP was registered despite holding a valid allowed certificate "+
			"(peer count=%d) — ban check must precede AllowedPeers check", host.PeerCount())
	} else {
		t.Logf("T-10 ✓ ban takes precedence over AllowedPeers: fpAttacker=%s… peer count=0",
			fpAttacker[:8])
	}
}

// ─── T-11: MaxPendingHandshakes — (N+1)th slow connection is rejected ─────────
//
// A malicious peer opens MaxPendingHandshakes raw TCP connections that never
// send any TLS ClientHello, holding the handshake goroutines blocked for up
// to 10 s each.  The (N+1)th connection must be closed immediately by
// acceptLoop before any goroutine is spawned for it.
//
// Slow connections are raw TCP (no TLS client) so the host's
// tlsConn.Handshake() blocks waiting for data — simulating a peer that stalls
// the handshake indefinitely.

func TestTLS_HandshakeFlood(t *testing.T) {
	const limit = 3

	cfgHost, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("T-11: gen host config: %v", err)
	}

	host := p2p.NewHost(p2p.Config{
		ListenAddr:           "127.0.0.1:0",
		MaxPeers:             20,
		NodeID:               "flood-test-node",
		UserAgent:            "aperod/test",
		TLSConfig:            cfgHost,
		MaxPendingHandshakes: limit,
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("T-11: host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	addr := host.ListenAddr()

	// Open `limit` raw TCP connections (no TLS).  The host accepts each one,
	// acquires a pending-handshake semaphore slot, and spawns a goroutine that
	// blocks in tlsConn.Handshake() waiting for the client's ClientHello.
	// This holds all semaphore slots.
	slowConns := make([]net.Conn, limit)
	for i := 0; i < limit; i++ {
		c, dialErr := net.DialTimeout("tcp", addr, 2*time.Second)
		if dialErr != nil {
			t.Fatalf("T-11: slow conn %d dial: %v", i, dialErr)
		}
		slowConns[i] = c
		t.Cleanup(func() { c.Close() })
	}

	// Give acceptLoop time to accept all slow connections and for each
	// goroutine to reach the blocking tlsConn.Handshake() call, consuming
	// all MaxPendingHandshakes slots.
	time.Sleep(150 * time.Millisecond)

	// The (limit+1)th connection must be rejected immediately by acceptLoop
	// (before any goroutine is launched) because the semaphore is full.
	extra, dialErr := net.DialTimeout("tcp", addr, 2*time.Second)
	if dialErr != nil {
		t.Fatalf("T-11: extra conn dial: %v", dialErr)
	}
	defer extra.Close()

	// Use a short deadline so we can distinguish an immediate close (EOF /
	// connection reset) from the connection simply hanging open — a timeout
	// here would mean the semaphore check is NOT working.
	extra.SetDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
	buf := make([]byte, 64)
	n, readErr := extra.Read(buf)
	if readErr == nil && n > 0 {
		// Some stacks send bytes (e.g. a TCP RST) before closing; drain and
		// wait for the actual close.
		_, readErr = extra.Read(buf)
	}

	if readErr == nil {
		t.Errorf("T-11: (limit+1)th connection still open — MaxPendingHandshakes not enforced")
		return
	}
	// A timeout means the connection was NOT rejected — it just hung.
	if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
		t.Errorf("T-11: (limit+1)th connection timed out instead of being rejected immediately — "+
			"MaxPendingHandshakes semaphore not working (timeout after 500 ms)")
		return
	}
	t.Logf("T-11 ✓ (limit+1)th connection rejected immediately (err=%v, bytes=%d)", readErr, n)

	// Slow connections did not complete the handshake, so PeerCount stays 0.
	if cnt := host.PeerCount(); cnt != 0 {
		t.Logf("T-11: note — PeerCount=%d (unexpected if slow conns completed handshake)", cnt)
	}
}

// ─── T-12: Connection survives 12 s of idle silence (ReadTimeout regression) ──
//
// With the old ReadTimeout = 5 s a passive peer that had nothing to send would
// be dropped after 5 s because ReadMsg would hit its deadline.  After raising
// ReadTimeout to 30 s and moving the keepalive ticker to 10 s, a connection
// that exchanges zero application messages must still be alive after 12 s.
//
// Two full TLS hosts are connected over real TCP (net.Pipe ignores deadlines).
// Neither side sends any application data after the initial Aperod handshake.
// The keepalive goroutine fires at ≈10 s, renewing the read deadline on both
// sides.  At t = 12 s both hosts must still report PeerCount == 1.

func TestTLS_ConnectionSurvivesIdleSilence(t *testing.T) {
	cfgA, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("T-12: gen config A: %v", err)
	}
	cfgB, _, err := p2p.GenerateNodeTLSConfig()
	if err != nil {
		t.Fatalf("T-12: gen config B: %v", err)
	}

	hostA := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "silence-node-a",
		UserAgent:  "aperod/test",
		TLSConfig:  cfgA,
	}, &stubHandler{}, newTestLogger())
	if err := hostA.Start(); err != nil {
		t.Fatalf("T-12: hostA.Start: %v", err)
	}
	t.Cleanup(hostA.Stop)

	hostB := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   5,
		NodeID:     "silence-node-b",
		UserAgent:  "aperod/test",
		TLSConfig:  cfgB,
	}, &stubHandler{}, newTestLogger())
	if err := hostB.Start(); err != nil {
		t.Fatalf("T-12: hostB.Start: %v", err)
	}
	t.Cleanup(hostB.Stop)

	// Connect B → A.  DialPeer is asynchronous; poll until both sides register
	// the peer (or fail after 3 s).
	hostB.DialPeer(hostA.ListenAddr())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hostA.PeerCount() == 1 && hostB.PeerCount() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if hostA.PeerCount() != 1 || hostB.PeerCount() != 1 {
		t.Fatalf("T-12: connection not established after 3 s: hostA peers=%d hostB peers=%d",
			hostA.PeerCount(), hostB.PeerCount())
	}

	// Sleep 12 s — no application data is sent by either side.
	// With the old ReadTimeout = 5 s the connection would have been torn down
	// around t = 5 s.  With ReadTimeout = 30 s and a 10 s keepalive ping both
	// peers remain connected.
	t.Log("T-12: sleeping 12 s to verify connection survives idle silence …")
	time.Sleep(12 * time.Second)

	if hostA.PeerCount() != 1 {
		t.Errorf("T-12: hostA lost peer after 12 s of silence (peers=%d, want 1) — ReadTimeout too tight or keepalive broken", hostA.PeerCount())
	}
	if hostB.PeerCount() != 1 {
		t.Errorf("T-12: hostB lost peer after 12 s of silence (peers=%d, want 1) — ReadTimeout too tight or keepalive broken", hostB.PeerCount())
	}
	t.Logf("T-12 ✓ both peers still connected after 12 s of idle silence: hostA=%d hostB=%d",
		hostA.PeerCount(), hostB.PeerCount())
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

// ─── T-13: Silent peer evicted after pong deadline ────────────────────────────
//
// Verifies that a peer which never replies to keepalive MsgPing messages is
// evicted once 2× KeepaliveInterval elapses since the last MsgPong.  This
// detects half-open TCP connections (e.g. after a peer crash) that would
// otherwise hold a peer slot for up to ~2 h waiting for the OS TCP keepalive.
//
// Test sequence:
//  1. Start a host with a short KeepaliveInterval (100 ms) so the deadline
//     (200 ms) is reached quickly.
//  2. Connect a raw plain-TCP client that completes only the initial Aperod
//     handshake (sends MsgPing, receives MsgPong).
//  3. The client drains all incoming messages (to avoid write-blocking the
//     host) but never sends a MsgPong back.
//  4. After ≈3 keepalive ticks (~300 ms) the host's pong-deadline check must
//     close the connection.
//  5. Assert PeerCount drops to 0 within 1 s.

func TestKeepalive_SilentPeerEvicted(t *testing.T) {
	const (
		ka      = 100 * time.Millisecond // keepalive interval
		timeout = 2 * time.Second        // test wall-clock budget
	)

	host := p2p.NewHost(p2p.Config{
		ListenAddr:        "127.0.0.1:0",
		MaxPeers:          5,
		NodeID:            "pong-deadline-host",
		UserAgent:         "aperod/test",
		KeepaliveInterval: ka,
		// No TLSConfig — plain TCP for simplicity; TLS mutual-auth is tested
		// separately in T-2/T-6/T-10.
	}, &stubHandler{}, newTestLogger())
	if err := host.Start(); err != nil {
		t.Fatalf("T-13: host.Start: %v", err)
	}
	t.Cleanup(host.Stop)

	// Dial with a raw TCP connection (no TLS — host has no TLSConfig).
	conn, err := net.DialTimeout("tcp", host.ListenAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("T-13: dial: %v", err)
	}
	defer conn.Close()

	// Complete the Aperod handshake: dialer sends MsgPing, host replies MsgPong.
	conn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if err := p2p.WriteMsg(conn, p2p.MsgPing, p2p.PingMsg{
		NodeID: "silent-peer", Height: 0, UserAgent: "test",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("T-13: write handshake ping: %v", err)
	}
	msgType, _, err := p2p.ReadMsg(conn)
	if err != nil {
		t.Fatalf("T-13: read handshake pong: %v", err)
	}
	if msgType != p2p.MsgPong {
		t.Fatalf("T-13: expected MsgPong from host, got %v", msgType)
	}

	// Handshake complete — host registers the peer.
	time.Sleep(50 * time.Millisecond)
	if host.PeerCount() != 1 {
		t.Fatalf("T-13: peer not registered after handshake (count=%d, want 1)", host.PeerCount())
	}

	// Drain incoming MsgPing messages from the host's keepalive goroutine but
	// never reply with MsgPong.  Run in a background goroutine; stop when the
	// connection closes or the test ends.
	conn.SetDeadline(time.Time{}) //nolint:errcheck
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			conn.SetReadDeadline(time.Now().Add(timeout)) //nolint:errcheck
			typ, _, err := p2p.ReadMsg(conn)
			if err != nil {
				return // connection closed by host — expected
			}
			if typ != p2p.MsgPing {
				// Unexpected message type; keep draining.
				continue
			}
			// Intentionally do NOT send MsgPong — this is the whole point of
			// the test.
		}
	}()

	// Poll until PeerCount drops to 0 (host evicts the silent peer) or we time out.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if host.PeerCount() == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Wait for the drain goroutine to finish (it should have exited once the
	// host closed the connection).
	select {
	case <-drainDone:
	case <-time.After(500 * time.Millisecond):
		t.Log("T-13: drain goroutine did not finish — connection may still be open")
	}

	if host.PeerCount() != 0 {
		t.Errorf("T-13: silent peer was NOT evicted after pong deadline "+
			"(peer count=%d, want 0) — pong-deadline check not working",
			host.PeerCount())
	} else {
		t.Logf("T-13 ✓ silent peer evicted after pong deadline: "+
			"keepalive_interval=%s deadline=%s",
			ka, 2*ka)
	}
}
