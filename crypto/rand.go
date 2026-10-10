package crypto

import "crypto/rand"

// randRead fills b with cryptographically random bytes.
// Extracted so tests can override it (never do this in production).
var randRead = func(b []byte) (int, error) {
	return rand.Read(b)
}
