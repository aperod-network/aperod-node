package p2p

// Reservations cover all inbound pre-registration handshake work. A single
// source cannot consume the global budget; established-peer checks remain
// independently enforced at registration.
func (h *Host) reserveInboundHandshake(ip string) bool {
	if ip == "" {
		return false
	}
	h.handshakeMu.Lock()
	defer h.handshakeMu.Unlock()
	limit := 3
	if h.cfg.MaxPeersPerIP > 0 && h.cfg.MaxPeersPerIP < limit {
		limit = h.cfg.MaxPeersPerIP
	}
	if total := h.cfg.MaxPendingHandshakes; total > 1 && limit > total/2 {
		limit = total / 2
	}
	if h.pendingHandshakeIPs[ip] >= limit {
		return false
	}
	if h.cfg.MaxPendingHandshakes > 0 &&
		h.pendingHandshakes.Load() >= int64(h.cfg.MaxPendingHandshakes) {
		return false
	}
	if h.pendingHandshakeIPs == nil {
		h.pendingHandshakeIPs = make(map[string]int)
	}
	h.pendingHandshakeIPs[ip]++
	h.pendingHandshakes.Add(1)
	return true
}

func (h *Host) releaseInboundHandshake(ip string) {
	h.handshakeMu.Lock()
	defer h.handshakeMu.Unlock()
	n := h.pendingHandshakeIPs[ip]
	if n == 0 {
		return
	}
	if n == 1 {
		delete(h.pendingHandshakeIPs, ip)
	} else {
		h.pendingHandshakeIPs[ip] = n - 1
	}
	h.pendingHandshakes.Add(-1)
}
