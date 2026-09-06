package avm

import "testing"

// FuzzValidateModuleNeverPanics exercises the untrusted Wasm decoder and
// instruction validator. Every byte slice must produce either a bounded report
// or an error, never a panic.
func FuzzValidateModuleNeverPanics(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 'a', 's', 'm', 1, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, code []byte) {
		if len(code) > MaxCodeSize+1 {
			code = code[:MaxCodeSize+1]
		}
		_, _ = ValidateModule(code)
	})
}
