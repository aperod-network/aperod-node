// recover-tip restores the LevelDB tip pointer to a specific block height.
// Use this when the chain tip was accidentally overwritten by a node restart
// that produced blocks from genesis instead of the real canonical tip.
//
// Usage:
//   cd /opt/aperod/blockchain
//   go run ./cmd/recover-tip/ --db /opt/aperod/data/testnet/chain.db --height 973102
//
// The node MUST be stopped before running this tool.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

var (
	prefixHeight = []byte("h/") // h/<uint64-big-endian> → 32-byte hash
	prefixMeta   = []byte("m/") // m/<key> → value
)

func heightKey(h uint64) []byte {
	key := make([]byte, len(prefixHeight)+8)
	copy(key, prefixHeight)
	binary.BigEndian.PutUint64(key[len(prefixHeight):], h)
	return key
}

func metaKey(k string) []byte {
	return append(append([]byte{}, prefixMeta...), []byte(k)...)
}

func main() {
	dbPath := flag.String("db", "", "Path to chain.db directory (required)")
	height := flag.Uint64("height", 0, "Target block height to restore tip to (required)")
	flag.Parse()

	if *dbPath == "" || *height == 0 {
		fmt.Fprintln(os.Stderr, "Usage: recover-tip --db <chain.db path> --height <block height>")
		os.Exit(1)
	}

	log.Printf("Opening LevelDB at %s ...", *dbPath)
	db, err := leveldb.OpenFile(*dbPath, &opt.Options{
		BlockCacheCapacity: 32 * opt.MiB,
		WriteBuffer:        16 * opt.MiB,
	})
	if err != nil {
		log.Fatalf("FATAL: cannot open LevelDB: %v", err)
	}
	defer db.Close()

	// Read current tip for display.
	curHashB, _ := db.Get(metaKey("tip/hash"), nil)
	curHtB, _ := db.Get(metaKey("tip/height"), nil)
	if curHashB != nil && len(curHtB) == 8 {
		curHt := binary.LittleEndian.Uint64(curHtB)
		log.Printf("Current tip: height=%d hash=%x", curHt, curHashB[:8])
	} else {
		log.Printf("Current tip: not found / corrupt")
	}

	// Look up the hash for the requested height via the canonical height index.
	hKey := heightKey(*height)
	hashBytes, err := db.Get(hKey, nil)
	if err == leveldb.ErrNotFound || len(hashBytes) == 0 {
		log.Fatalf("FATAL: no height-index entry for block %d — block not in DB", *height)
	}
	if err != nil {
		log.Fatalf("FATAL: reading height index for %d: %v", *height, err)
	}
	if len(hashBytes) != 32 {
		log.Fatalf("FATAL: height index entry for %d is %d bytes, want 32", *height, len(hashBytes))
	}

	log.Printf("Found block %d with hash %x", *height, hashBytes[:8])

	// Confirm the block data itself exists (sanity check).
	blkKey := append([]byte("b/"), hashBytes...)
	blkData, err := db.Get(blkKey, nil)
	if err == leveldb.ErrNotFound || len(blkData) == 0 {
		log.Fatalf("FATAL: block data for height %d (hash %x) not found in b/ namespace", *height, hashBytes[:8])
	}
	if err != nil {
		log.Fatalf("FATAL: reading block data: %v", err)
	}
	log.Printf("Block data found: %d bytes", len(blkData))

	// Write new tip.
	var hb [8]byte
	binary.LittleEndian.PutUint64(hb[:], *height)

	batch := new(leveldb.Batch)
	batch.Put(metaKey("tip/hash"), hashBytes)
	batch.Put(metaKey("tip/height"), hb[:])

	if err := db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
		log.Fatalf("FATAL: writing tip: %v", err)
	}

	log.Printf("✓ Tip restored to height=%d hash=%x", *height, hashBytes[:8])
	log.Printf("You can now restart the node.")
}
