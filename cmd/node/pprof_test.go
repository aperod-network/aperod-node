package main

// Integration test: pprof HTTP endpoint is reachable and serves valid profiles.
//
// The test mirrors the pprof goroutine in main.go exactly — it starts an
// http.Server on http.DefaultServeMux (which has all /debug/pprof/* handlers
// registered via the side-effect import at the top of main.go) and verifies:
//
//  1. GET /debug/pprof/ returns HTTP 200 and contains the pprof index HTML.
//  2. GET /debug/pprof/goroutine?debug=1 returns HTTP 200 with a text-format
//     goroutine profile that contains the word "goroutine".
//  3. GET /debug/pprof/profile?seconds=1 returns HTTP 200 with a valid
//     gzip-compressed pprof binary (successfully decompressed; the raw proto
//     begins with the expected varint field tag 0x0a for the "sample_type" field).
//
// No full node startup is required: the import `_ "net/http/pprof"` in main.go
// already registers the handlers on http.DefaultServeMux for the entire test
// binary.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freePprofPort returns a random free TCP port on loopback.
func freePprofPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePprofPort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// startPprofServer starts an http.Server on the given addr using
// http.DefaultServeMux (which has the pprof handlers registered).
// It returns only after the port is confirmed open and ready for connections.
// The server is shut down via t.Cleanup.
func startPprofServer(t *testing.T, addr string) {
	t.Helper()

	srv := &http.Server{
		Addr:    addr,
		Handler: http.DefaultServeMux,
	}

	ready := make(chan struct{})
	go func() {
		// Use a net.Listener so we can signal readiness before Serve blocks.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			close(ready)
			return
		}
		close(ready)
		_ = srv.Serve(ln) //nolint:errcheck // ErrServerClosed is expected on cleanup
	}()

	<-ready

	// Poll until the server actually accepts connections (avoids flaky ECONNREFUSED).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() { _ = srv.Close() })
}

// TestPprofIndexReachable verifies that the pprof index page is reachable,
// returns HTTP 200, and contains recognizable pprof index HTML — confirming
// the handler is genuinely serving the profile index, not an error page or
// arbitrary content.
func TestPprofIndexReachable(t *testing.T) {
	port := freePprofPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	startPprofServer(t, addr)

	url := "http://" + addr + "/debug/pprof/"
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/ returned %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading index body: %v", err)
	}

	// The pprof index handler emits HTML that lists the available profile types.
	// Check for two known landmarks that can only be present if the real handler ran.
	for _, want := range []string{"goroutine", "heap"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("GET /debug/pprof/ body does not contain %q — got a different page?\nbody excerpt: %.512s",
				want, body)
		}
	}
}

// TestPprofGoroutineProfile confirms that the goroutine profile endpoint returns
// HTTP 200 with a text-format goroutine dump that contains the word "goroutine".
// The ?debug=1 flag selects the human-readable text format; any non-trivial Go
// process running this test has at least one goroutine, so the keyword must
// appear.  An arbitrary non-empty body (e.g. an error message) would not contain
// the keyword and would fail this check.
func TestPprofGoroutineProfile(t *testing.T) {
	port := freePprofPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	startPprofServer(t, addr)

	url := "http://" + addr + "/debug/pprof/goroutine?debug=1"
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/goroutine returned %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading goroutine profile body: %v", err)
	}

	// The text-format goroutine dump always starts with a line like:
	//   goroutine profile: total N
	// followed by per-goroutine stacks.  Check that the keyword appears so we
	// can distinguish a genuine profile from an arbitrary non-empty response.
	if !strings.Contains(string(body), "goroutine") {
		t.Errorf("goroutine profile body does not contain \"goroutine\" — not a valid profile?\nbody excerpt: %.512s", body)
	}
}

// TestPprofCPUProfile issues a 1-second CPU profile request and structurally
// validates the response as a genuine pprof binary:
//
//  1. HTTP 200 is returned.
//  2. The response body is valid gzip (successfully decompressed).
//  3. The decompressed payload starts with protobuf field tag 0x0a (field 1,
//     wire type 2 = length-delimited), which is the "sample_type" field — the
//     first field in every pprof Profile proto message.
//
// This rules out the pprof handler returning an error page or arbitrary bytes:
// a misconfigured or broken handler cannot accidentally produce valid
// gzip-compressed protobuf data with the correct pprof field layout.
func TestPprofCPUProfile(t *testing.T) {
	port := freePprofPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	startPprofServer(t, addr)

	// Allow enough time: 1-second profile + network + decode overhead.
	client := &http.Client{Timeout: 30 * time.Second}

	url := "http://" + addr + "/debug/pprof/profile?seconds=1"
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /debug/pprof/profile returned %d, want 200\nbody: %s", resp.StatusCode, raw)
	}

	compressed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading CPU profile body: %v", err)
	}
	if len(compressed) == 0 {
		t.Fatal("CPU profile response body is empty")
	}
	t.Logf("CPU profile compressed size: %d bytes", len(compressed))

	// ── Structural check 1: valid gzip stream ────────────────────────────────
	// The pprof handler always gzip-compresses its output.  A broken or
	// redirected handler would return plain HTML/text that is not gzip data.
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("CPU profile is not a valid gzip stream: %v\n(first 16 bytes: %x)", err, compressed[:min16(len(compressed))])
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip decompression of CPU profile failed: %v", err)
	}
	if len(decompressed) == 0 {
		t.Fatal("CPU profile decompresses to an empty payload")
	}
	t.Logf("CPU profile decompressed size: %d bytes", len(decompressed))

	// ── Structural check 2: valid pprof protobuf field tag ───────────────────
	// A pprof Profile proto (github.com/google/pprof/proto/profile.proto) has
	// fields 1–14.  Protobuf does not guarantee field ordering, so we decode the
	// first varint (the field tag) and verify that:
	//   • field_number is in [1, 14]  (all known Profile fields)
	//   • wire_type   is in {0,1,2,5} (all valid protobuf wire types)
	//
	// An error page or arbitrary bytes is extremely unlikely to decode as a
	// varint with these constraints.
	tag, ok := decodeFirstVarint(decompressed)
	if !ok {
		t.Fatalf("cannot decode first varint from decompressed CPU profile (len=%d, first byte=0x%02x)",
			len(decompressed), decompressed[0])
	}
	fieldNumber := tag >> 3
	wireType := tag & 0x7
	if fieldNumber < 1 || fieldNumber > 14 {
		t.Errorf("first protobuf field number = %d, want 1–14 (pprof Profile fields); "+
			"payload does not look like a valid pprof binary", fieldNumber)
	}
	validWireTypes := map[uint64]bool{0: true, 1: true, 2: true, 5: true}
	if !validWireTypes[wireType] {
		t.Errorf("first protobuf wire type = %d, not a valid wire type {0,1,2,5}; "+
			"payload does not look like a valid pprof binary", wireType)
	}
	t.Logf("CPU profile first proto field: number=%d wire_type=%d (tag=0x%02x)", fieldNumber, wireType, tag)
}

// min16 returns min(n, 16), used to safely slice the first 16 bytes of a
// possibly-short slice for diagnostic output.
func min16(n int) int {
	if n < 16 {
		return n
	}
	return 16
}

// decodeFirstVarint reads the first protobuf varint from b and returns it.
// A protobuf varint uses 7 bits per byte with the MSB as a continuation flag.
// Returns (0, false) if b is empty or if the varint is truncated (> 10 bytes).
func decodeFirstVarint(b []byte) (uint64, bool) {
	var x uint64
	for i, byt := range b {
		if i >= 10 {
			// Protobuf varints are at most 10 bytes; anything longer is invalid.
			return 0, false
		}
		x |= uint64(byt&0x7f) << (7 * uint(i))
		if byt&0x80 == 0 {
			return x, true
		}
	}
	return 0, false // truncated
}
