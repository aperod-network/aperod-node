//go:build linux || darwin

package crypto

import "golang.org/x/sys/unix"

// MlockBytes locks the memory pages containing b into RAM, preventing them
// from being swapped to disk or appearing in a core dump.
// This is a best-effort operation: the call is non-fatal if it fails
// (e.g. when the process lacks CAP_IPC_LOCK or the kernel limits are low).
// Callers should still zero the buffer with ZeroBytes before releasing it.
func MlockBytes(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Mlock(b)
}

// MunlockBytes releases the mlock on b.  Safe to call even if MlockBytes
// was never called or returned an error.
func MunlockBytes(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Munlock(b)
}
