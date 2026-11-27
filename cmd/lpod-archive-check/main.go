// lpod-archive-check checks a bounded interval in a disposable archive copy.
// It deliberately does NOT claim committee/quorum authentication.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/internal/checkedio"
	"github.com/aperod/aperod/store"
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
		err := evictArchiveCopy(*path)
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
	defer checkedio.LogClose(slog.Default(), db, "offline archive audit database")
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
	evidence, err := endpointCertificateEvidence(cert)
	if err != nil {
		fail("certificate: %v", err)
	}
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		fail("resource accounting: %v", err)
	}
	result := map[string]any{
		"start": *start, "end": *end, "blocks": count, "transactions": txs,
		"certificate_height": cert.Height, "certificate_hash": fmt.Sprintf("%x", cert.BlockHash),
		"signature_valid_vote_keys": evidence.SignatureValidVoteKeys,
		"committee_authenticated":   evidence.CommitteeAuthenticated,
		"quorum_authenticated":      evidence.QuorumAuthenticated,
		"production_ready":          evidence.ProductionReady,
		"trust":                     evidence.Trust,
		"input_block_operations":    after.Inblock - before.Inblock, "peak_rss_kib": after.Maxrss,
		"file_eviction_requested": *evict,
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fail("encode: %v", err)
	}
}

// evictArchiveCopy confines advisory operations to the operator-selected cold
// copy, including when an entry is replaced after directory enumeration.
// Successful advice does not establish that later reads reach physical media.
func evictArchiveCopy(path string) (retErr error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer checkedio.CloseReadResult(root, &retErr)
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refuse nonregular copy entry: %s", name)
		}
		return evictArchiveCopyFile(root, name)
	})
}

func evictArchiveCopyFile(root *os.Root, name string) (retErr error) {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer checkedio.CloseReadResult(f, &retErr)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("opened copy entry is not a regular file: %s", name)
	}
	syncErr := f.Sync()
	if syncErr != nil {
		return syncErr
	}
	// #nosec G115 -- This owned, still-open descriptor originated in root.Open; it is a native fd, not an arbitrary uintptr.
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
