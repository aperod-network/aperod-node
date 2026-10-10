// lpod-archive-check checks a bounded interval in a disposable archive copy.
// It deliberately does NOT claim committee/quorum authentication.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
	"golang.org/x/sys/unix"
)

func main() {
	path := flag.String("copied-db", "", "disposable, offline archive database")
	start := flag.Uint64("start", 0, "first included height (must be positive)")
	end := flag.Uint64("end", 0, "last included height (maximum 50000 blocks)")
	evict := flag.Bool("evict-copy", false, "advise eviction of regular files in the disposable copy only")
	flag.Parse()
	if *path == "" || *start == 0 || *end < *start || *end-*start >= 50000 {
		fail("require copied-db and a positive interval of at most 50000 blocks")
	}
	if *evict {
		err := filepath.WalkDir(*path, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("refuse nonregular entry: %s", path)
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = f.Sync()
			if err == nil {
				err = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
			}
			closeErr := f.Close()
			if err != nil {
				return err
			}
			return closeErr
		})
		if err != nil {
			fail("copy-only eviction: %v", err)
		}
	}
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		fail("resource accounting: %v", err)
	}
	db, err := store.OpenLPoDAuditReadOnly(*path)
	if err != nil {
		fail("open copy: %v", err)
	}
	defer db.Close()
	previous, found, err := db.GetCanonicalHash(*start - 1)
	if err != nil || !found {
		fail("missing preceding index: %v", err)
	}
	var count, txs uint64
	for h := *start; h <= *end; h++ {
		index, found, err := db.GetCanonicalHash(h)
		if err != nil || !found {
			fail("height %d missing index: %v", h, err)
		}
		raw, err := db.GetRawBlock(index)
		if err != nil || len(raw) == 0 {
			fail("height %d missing body: %v", h, err)
		}
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			fail("height %d decode: %v", h, err)
		}
		if b.Header.Height != h || b.Hash() != index || b.Header.PrevHash != previous ||
			!b.Header.VerifySignature() || b.Header.MerkleRoot != core.MerkleRoot(b.Txs) {
			fail("height %d failed height/hash/ancestry/signature/Merkle checks", h)
		}
		previous = index
		count++
		txs += uint64(len(b.Txs))
	}
	cert, err := db.LoadFinalityCertificate()
	if err != nil || cert == nil {
		fail("missing stored certificate: %v", err)
	}
	index, found, err := db.GetCanonicalHash(cert.Height)
	if err != nil || !found || cert.Version != 1 || index != cert.BlockHash || cert.Height < *end {
		fail("certificate does not canonically cover interval")
	}
	// Require exact endpoint, avoiding an unverified gap to a later certificate.
	if cert.Height != *end || cert.BlockHash != previous {
		fail("certificate height %d must equal interval end %d", cert.Height, *end)
	}
	seen := map[string]bool{}
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), cert.BlockHash.Bytes())
	for _, vote := range cert.Votes {
		key := vote.Validator.Hex()
		if seen[key] || !vote.Validator.Verify(message, vote.Signature) {
			fail("invalid or duplicate certificate vote")
		}
		seen[key] = true
	}
	if len(seen) == 0 {
		fail("empty certificate")
	}
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		fail("resource accounting: %v", err)
	}
	result := map[string]any{
		"start": *start, "end": *end, "blocks": count, "transactions": txs,
		"certificate_height": cert.Height, "certificate_hash": fmt.Sprintf("%x", cert.BlockHash),
		"signature_valid_vote_keys": seen, "committee_authenticated": false,
		"trust": "internal consistency only; no historical committee anchor or quorum proof",
		"input_block_operations": after.Inblock - before.Inblock, "peak_rss_kib": after.Maxrss,
		"file_eviction_requested": *evict,
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fail("encode: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}