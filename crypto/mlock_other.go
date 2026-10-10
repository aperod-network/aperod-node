//go:build !linux && !darwin

package crypto

// MlockBytes is a no-op on platforms that do not support mlock(2).
func MlockBytes(_ []byte) error { return nil }

// MunlockBytes is a no-op on platforms that do not support mlock(2).
func MunlockBytes(_ []byte) error { return nil }
