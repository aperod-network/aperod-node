// lpod-v3-registry-extract extracts the registry from a v2 startup snapshot
// without constructing the snapshot's UTXO collections in memory.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const tailLimit = 4 << 20

type anchoredRegistry struct {
	Height   uint64                `json:"height"`
	Hash     crypto.Hash32         `json:"hash"`
	Registry core.RegistrySnapshot `json:"registry"`
}

type snapshotHeader struct {
	Version   int       `json:"v"`
	TipHeight uint64    `json:"tip_height"`
	TipHash   string    `json:"tip_hash"`
	TxTotal   int64     `json:"tx_total"`
	SavedAt   time.Time `json:"saved_at"`
}

func main() {
	snapshot := flag.String("snapshot", "", "v2 gzip startup snapshot path")
	anchorHeight := flag.Uint64("anchor-height", 0, "expected snapshot height")
	anchorHash := flag.String("anchor-hash", "", "expected 32-byte snapshot hash in hex")
	output := flag.String("output", "", "new output JSON path")
	flag.Parse()
	if *snapshot == "" || *anchorHash == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: lpod-v3-registry-extract --snapshot PATH --anchor-height N --anchor-hash HEX --output PATH")
		os.Exit(2)
	}
	hash, err := parseHash(*anchorHash)
	if err != nil {
		fail(err)
	}
	if err := extract(*snapshot, *anchorHeight, hash, *output); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpod-v3-registry-extract:", err)
	os.Exit(1)
}

func parseHash(value string) (crypto.Hash32, error) {
	var hash crypto.Hash32
	if len(value) != 64 {
		return hash, fmt.Errorf("anchor hash must be exactly 32 bytes of hexadecimal")
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != len(hash) {
		return hash, fmt.Errorf("anchor hash must be exactly 32 bytes of hexadecimal")
	}
	copy(hash[:], raw)
	return hash, nil
}

func regularNoSymlink(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular non-symlink file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		file.Close()
		return nil, fmt.Errorf("%s changed or is not a regular file", path)
	}
	return file, nil
}

func verifySidecar(snapshotPath string) error {
	source, err := regularNoSymlink(snapshotPath)
	if err != nil {
		return err
	}
	defer source.Close()
	sidecar, err := regularNoSymlink(snapshotPath + ".sha256")
	if err != nil {
		return fmt.Errorf("checksum sidecar: %w", err)
	}
	defer sidecar.Close()
	checksum := sha256.New()
	if _, err := io.Copy(checksum, source); err != nil {
		return fmt.Errorf("hash snapshot: %w", err)
	}
	sidecarBytes, err := io.ReadAll(io.LimitReader(sidecar, 67))
	if err != nil {
		return fmt.Errorf("read checksum sidecar: %w", err)
	}
	switch {
	case len(sidecarBytes) == 64:
	case len(sidecarBytes) == 65 && sidecarBytes[64] == '\n':
	case len(sidecarBytes) == 66 && bytes.Equal(sidecarBytes[64:], []byte("\r\n")):
	default:
		return fmt.Errorf("checksum sidecar must contain 64 hexadecimal characters, optionally followed by LF or CRLF")
	}
	decoded, err := hex.DecodeString(string(sidecarBytes[:64]))
	if err != nil || !bytes.Equal(decoded, checksum.Sum(nil)) {
		return fmt.Errorf("snapshot SHA256 does not match its sidecar")
	}
	return nil
}

func extract(snapshotPath string, height uint64, expected crypto.Hash32, outputPath string) error {
	if err := verifySidecar(snapshotPath); err != nil {
		return err
	}
	input, err := regularNoSymlink(snapshotPath)
	if err != nil {
		return err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("open gzip snapshot: %w", err)
	}

	headerPrefix, err := readHeaderPrefix(gz)
	if err != nil {
		gz.Close()
		return err
	}
	header, err := decodeHeader(headerPrefix)
	if err != nil {
		gz.Close()
		return err
	}
	if header.Version != 2 {
		gz.Close()
		return fmt.Errorf("snapshot version is %d, want v2", header.Version)
	}
	if header.TipHeight != height {
		gz.Close()
		return fmt.Errorf("snapshot height %d does not match expected anchor height %d", header.TipHeight, height)
	}
	headerHash, err := parseHash(header.TipHash)
	if err != nil || headerHash != expected {
		gz.Close()
		return fmt.Errorf("snapshot tip hash does not match expected anchor hash")
	}

	tail, scan, err := readTailAndScan(gz, headerPrefix)
	closeErr := gz.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("finish gzip snapshot (CRC/stream): %w", closeErr)
	}
	registry, err := extractRegistry(tail, scan)
	if err != nil {
		return err
	}

	result := anchoredRegistry{Height: height, Hash: expected, Registry: registry}
	out, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create output exclusively: %w", err)
	}
	enc := json.NewEncoder(out)
	writeErr := enc.Encode(result)
	closeErr = out.Close()
	if writeErr != nil {
		return fmt.Errorf("write output: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close output: %w", closeErr)
	}
	return nil
}

// readHeaderPrefix follows the v2 writer's fixed header order and stops before
// the UTXO object. Only the small scalar header is retained.
func readHeaderPrefix(r io.Reader) ([]byte, error) {
	var prefix []byte
	const marker = `,"utxos":{`
	window := make([]byte, 0, len(marker))
	for len(prefix) < 1<<20 {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("incomplete snapshot header: %w", err)
		}
		prefix = append(prefix, b[0])
		window = append(window, b[0])
		if len(window) > len(marker) {
			window = window[1:]
		}
		if string(window) == marker {
			return prefix[:len(prefix)-len(marker)], nil
		}
	}
	return nil, fmt.Errorf("snapshot header exceeds 1 MiB or lacks the v2 UTXO field")
}

func decodeHeader(prefix []byte) (snapshotHeader, error) {
	var header snapshotHeader
	// The header's final fields and their order are emitted by the v2 writer;
	// close the truncated object so encoding/json can perform strict decoding.
	raw := append(append([]byte(nil), prefix...), '}')
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil {
		return header, fmt.Errorf("decode snapshot header: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return header, fmt.Errorf("snapshot header has trailing data")
	}
	return header, nil
}

type jsonScan struct {
	offset       uint64
	depth        int
	inString     bool
	escaped      bool
	stringStart  uint64
	stringToken  []byte
	rootRegistry uint64
	found        bool
	rootClosed   bool
	invalid      bool
}

func (s *jsonScan) consume(p []byte) {
	for _, b := range p {
		pos := s.offset
		s.offset++
		if s.inString {
			if s.escaped {
				s.escaped = false
				if len(s.stringToken) < len("registry")+1 {
					s.stringToken = append(s.stringToken, b)
				}
				continue
			}
			if b == '\\' {
				s.escaped = true
				if len(s.stringToken) < len("registry")+1 {
					s.stringToken = append(s.stringToken, b)
				}
				continue
			}
			if b == '"' {
				if s.depth == 1 && bytes.Equal(s.stringToken, []byte("registry")) {
					s.rootRegistry = s.stringStart
					s.found = true
				}
				s.inString = false
				continue
			}
			if len(s.stringToken) < len("registry")+1 {
				s.stringToken = append(s.stringToken, b)
			}
			continue
		}
		if s.rootClosed && !isJSONSpace(b) {
			s.invalid = true
		}
		switch b {
		case '"':
			s.inString = true
			s.stringStart = pos
			s.stringToken = s.stringToken[:0]
		case '{', '[':
			s.depth++
			if s.rootClosed {
				s.invalid = true
			}
		case '}', ']':
			s.depth--
			if s.depth < 0 {
				s.invalid = true
			}
			if b == '}' && s.depth == 0 {
				s.rootClosed = true
			}
		default:
			// Whitespace after the outer close is permitted; all other
			// post-document bytes were rejected above.
		}
	}
}

func isJSONSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

type tailCapture struct {
	tail []byte
	scan jsonScan
}

func (c *tailCapture) Write(p []byte) (int, error) {
	c.scan.consume(p)
	if len(p) >= tailLimit {
		c.tail = append(c.tail[:0], p[len(p)-tailLimit:]...)
	} else {
		excess := len(c.tail) + len(p) - tailLimit
		if excess > 0 {
			copy(c.tail, c.tail[excess:])
			c.tail = c.tail[:len(c.tail)-excess]
		}
		c.tail = append(c.tail, p...)
	}
	return len(p), nil
}

func readTailAndScan(r io.Reader, headerPrefix []byte) ([]byte, jsonScan, error) {
	// Reconstruct the already-consumed fixed writer prefix for the standard
	// JSON decoder. The decoder validates every UTXO token without retaining
	// the millions of decoded values; the tee keeps only the bounded tail.
	const utxoMarker = `,"utxos":{`
	stream := io.MultiReader(bytes.NewReader(headerPrefix), bytes.NewReader([]byte(utxoMarker)), r)
	capture := &tailCapture{tail: make([]byte, 0, tailLimit)}
	decoder := json.NewDecoder(io.TeeReader(stream, capture))
	decoder.UseNumber()
	if err := validateSnapshotJSON(decoder); err != nil {
		return nil, capture.scan, fmt.Errorf("validate snapshot JSON: %w", err)
	}
	if capture.scan.invalid || capture.scan.inString || capture.scan.depth != 0 ||
		!capture.scan.rootClosed {
		return nil, capture.scan, fmt.Errorf("snapshot JSON is incomplete or has trailing data")
	}
	return capture.tail, capture.scan, nil
}

func validateSnapshotJSON(decoder *json.Decoder) error {
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("snapshot root must be an object")
	}
	expected := []string{"v", "tip_height", "tip_hash", "tx_total", "saved_at", "utxos", "registry"}
	next := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("snapshot object contains a non-string key")
		}
		if next == 4 && key == "utxos" {
			next++ // Older v2 snapshots may omit saved_at.
		}
		if next >= len(expected) || key != expected[next] {
			return fmt.Errorf("unexpected snapshot field %q", key)
		}
		next++
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	if next != len(expected) {
		return fmt.Errorf("snapshot is missing a required field")
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return fmt.Errorf("snapshot root object is incomplete")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return fmt.Errorf("snapshot has trailing JSON data")
		}
		return fmt.Errorf("snapshot has trailing or invalid data: %w", err)
	}
	return nil
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			if _, ok := key.(string); !ok {
				return fmt.Errorf("object contains a non-string key")
			}
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("incomplete JSON object")
		}
	case '[':
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("incomplete JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func extractRegistry(tail []byte, scan jsonScan) (core.RegistrySnapshot, error) {
	var registry core.RegistrySnapshot
	tailStart := scan.offset - uint64(len(tail))
	if !scan.found || scan.rootRegistry < tailStart {
		return registry, fmt.Errorf("registry cannot be extracted: registry field is absent or exceeds the 4 MiB tail bound")
	}
	keyOffset := int(scan.rootRegistry - tailStart)
	if keyOffset < 0 || keyOffset+len(`"registry"`) > len(tail) ||
		string(tail[keyOffset:keyOffset+len(`"registry"`)]) != `"registry"` {
		return registry, fmt.Errorf("registry cannot be extracted from snapshot tail")
	}
	i := keyOffset + len(`"registry"`)
	for i < len(tail) && isJSONSpace(tail[i]) {
		i++
	}
	if i >= len(tail) || tail[i] != ':' {
		return registry, fmt.Errorf("registry field has invalid JSON syntax")
	}
	i++
	for i < len(tail) && isJSONSpace(tail[i]) {
		i++
	}
	decoder := json.NewDecoder(bytes.NewReader(tail[i:]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return registry, fmt.Errorf("decode registry snapshot: %w", err)
	}
	end := i + int(decoder.InputOffset())
	if end > len(tail) || !bytes.Equal(bytes.TrimSpace(tail[end:]), []byte("}")) {
		return registry, fmt.Errorf("registry is not followed by the complete snapshot JSON suffix")
	}
	return registry, nil
}
