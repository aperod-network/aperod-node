// export_test.go — exposes internal Host methods for tests in package p2p_test.
// This file is compiled only during `go test` because it lives in package p2p
// (not p2p_test) alongside the implementation.

package p2p

import "time"

// BadBlockMaxTrackedIPs is the exported alias for badBlockMaxTrackedIPs,
// available to package p2p_test.
const BadBlockMaxTrackedIPs = badBlockMaxTrackedIPs

// BadBlockStrikeCount returns the current number of IPs tracked in the
// bad-block strike map.  Exported for testing only.
func (h *Host) BadBlockStrikeCount() int {
	h.badBlockMu.Lock()
	defer h.badBlockMu.Unlock()
	return len(h.badBlockCounts)
}

// RecordBadBlockStrike directly records one strike for ip, applying the same
// cap and expiry logic as the real dispatch path but without going through the
// network layer.  Returns the resulting strike count (0 if the cap was full
// and ip was not already tracked).  Exported for testing only.
func (h *Host) RecordBadBlockStrike(ip string) int {
	h.badBlockMu.Lock()
	defer h.badBlockMu.Unlock()
	strike := h.badBlockCounts[ip]
	if !strike.lastSeen.IsZero() && time.Since(strike.lastSeen) > badBlockStrikeTTL {
		strike.count = 0
	}
	_, alreadyTracked := h.badBlockCounts[ip]
	if alreadyTracked || len(h.badBlockCounts) < badBlockMaxTrackedIPs {
		strike.count++
		strike.lastSeen = time.Now()
		h.badBlockCounts[ip] = strike
	}
	return strike.count
}
