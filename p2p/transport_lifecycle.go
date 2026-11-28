package p2p

import (
	"crypto/tls"
	"fmt"
	"io"
	"time"
)

// completeTLSHandshake fails before application traffic when either deadline
// cannot be established. Certificate and SPKI verification remain in tls.Config.
func completeTLSHandshake(conn *tls.Conn, timeout time.Duration) error {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set TLS handshake deadline: %w", err)
	}
	if err := conn.Handshake(); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear TLS handshake deadline: %w", err)
	}
	return nil
}

// closeTransport records teardown failures without retrying a frame or changing
// an already-decided peer rejection. Connection ownership stays with the caller.
func (h *Host) closeTransport(transport io.Closer, operation string) {
	if err := transport.Close(); err != nil {
		h.log.Debug("P2P transport close failed", "operation", operation, "err", err)
	}
}
