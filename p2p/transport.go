package p2p

// transport.go — TLS authenticated transport for the Aperod P2P layer.
//
// Each node generates an ephemeral Ed25519 identity on startup and wraps its
// TCP listener / outbound dials with TLS 1.3.  Mutual authentication is
// enforced: both sides present a self-signed certificate and record the remote
// peer's public-key fingerprint as its authenticated identity.
//
// There is no certificate authority.  Trust is established via the fingerprint
// (SHA-256 of the peer's SubjectPublicKeyInfo), which is logged on connect and
// can be compared against a validator allow-list.  This eliminates passive
// eavesdropping and active message injection from anyone who can reach port
// 30303.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"time"
)

// GenerateNodeTLSConfig creates an ephemeral Ed25519 identity and returns a
// *tls.Config together with the node's authenticated fingerprint (hex-encoded
// SHA-256 of the SPKI public key).
//
// The config is suitable for both the server side (tls.NewListener) and the
// client side (tls.DialWithDialer):
//
//   - Server: ClientAuth=RequireAnyClientCert — every client must present a
//     certificate; the chain is NOT verified (self-signed peers are fine).
//   - Client: InsecureSkipVerify=true — chain verification is skipped; callers
//     use PeerFingerprint to authenticate the remote identity.
//   - MinVersion: TLS 1.3 — older protocols are forbidden.
func GenerateNodeTLSConfig() (*tls.Config, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("p2p/transport: generate ed25519 key: %w", err)
	}
	return nodeConfigFromKey(priv, pub)
}

// NodeTLSConfigFromKey builds a *tls.Config from an existing Ed25519 key pair.
// The fingerprint is the hex-encoded SHA-256 of the SubjectPublicKeyInfo.
func NodeTLSConfigFromKey(priv ed25519.PrivateKey, pub ed25519.PublicKey) (*tls.Config, string, error) {
	return nodeConfigFromKey(priv, pub)
}

func nodeConfigFromKey(priv ed25519.PrivateKey, pub ed25519.PublicKey) (*tls.Config, string, error) {
	cert, err := selfSignedCert(priv, pub)
	if err != nil {
		return nil, "", err
	}
	fingerprint, err := spkiFingerprint(pub)
	if err != nil {
		return nil, "", err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Server side: require the client to present a certificate.
		// RequireAnyClientCert means the cert must be sent but the chain is
		// not verified — appropriate for self-signed peer certs.
		ClientAuth: tls.RequireAnyClientCert,
		// Client side: skip chain verification; PeerFingerprint handles
		// identity confirmation.
		InsecureSkipVerify: true, //nolint:gosec // intentional — fingerprint auth
		MinVersion:         tls.VersionTLS13,
	}
	return cfg, fingerprint, nil
}

// PeerFingerprint returns the authenticated public-key fingerprint of the
// remote side of conn after a completed TLS handshake.
//
// The fingerprint is the hex-encoded SHA-256 of the peer's DER-encoded
// SubjectPublicKeyInfo (SPKI) — the same format used by node.yaml
// `allowed_peers` lists and the admin panel.
//
// Returns "" when conn is a plain net.Conn (no TLS — used in unit tests).
func PeerFingerprint(conn net.Conn) string {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	fp, err := spkiFingerprint(state.PeerCertificates[0].PublicKey)
	if err != nil {
		return ""
	}
	return fp
}

// spkiFingerprint computes SHA-256(MarshalPKIXPublicKey(pub)) as a hex string.
func spkiFingerprint(pub interface{}) (string, error) {
	rawPub, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("p2p/transport: marshal public key: %w", err)
	}
	h := sha256.Sum256(rawPub)
	return hex.EncodeToString(h[:]), nil
}

// selfSignedCert creates a self-signed X.509 certificate for the given
// Ed25519 key pair.  The certificate is valid for 10 years from now.
func selfSignedCert(priv ed25519.PrivateKey, pub ed25519.PublicKey) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("p2p/transport: serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "aperod-node"},
		NotBefore:    time.Now().Add(-time.Minute), // small back-date for clock skew
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("p2p/transport: create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("p2p/transport: parse certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}
