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
