//go:build !js || !wasm

package main

// The command is intended for GOOS=js GOARCH=wasm.  Keeping a native entry
// point makes package inspection and ordinary `go build ./cmd/wallet-wasm`
// well-defined without exposing a second implementation.
func main() {}
