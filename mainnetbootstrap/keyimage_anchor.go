package mainnetbootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	chaincrypto "github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/checkedio"
	"github.com/aperod/aperod/store"
)

const maxKeyImageAnchorBytes = 64 << 20
const maxKeyImageAnchorEntries = 250_000

type keyImageAnchorDocument struct {
	Height    uint64
	BlockHash string
	KeyImages []chaincrypto.KeyImage
}

// InspectKeyImageAnchor verifies a bounded operator state export against the
// canonical height index in the copied database and returns its exact digest.
// It does not make an independent historical-spend or committee claim.
func InspectKeyImageAnchor(snapshotDB, anchorPath string, checkpointHeight uint64) (digest, blockHash string, height uint64, err error) {
	info, err := os.Lstat(anchorPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("inspect key-image anchor: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxKeyImageAnchorBytes {
		return "", "", 0, errors.New("key-image anchor must be a regular non-symlink file <=64 MiB")
	}
	// #nosec G304 -- Offline operator-selected anchor; non-symlink, size, same-inode and canonical hash checks surround this read.
	f, err := os.Open(anchorPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("open key-image anchor: %w", err)
	}
	defer checkedio.CloseReadResult(f, &err)
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", "", 0, errors.New("key-image anchor changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyImageAnchorBytes+1))
	if err != nil || len(raw) > maxKeyImageAnchorBytes {
		return "", "", 0, errors.New("key-image anchor read failed or exceeds 64 MiB")
	}

	doc, err := decodeKeyImageAnchor(raw)
	if err != nil {
		return "", "", 0, err
	}
	if doc.Height == 0 || doc.Height >= checkpointHeight ||
		!isLowerHash(doc.BlockHash) {
		return "", "", 0, errors.New("key-image anchor height/hash is invalid or not earlier than checkpoint")
	}
	db, err := store.OpenReadOnly(snapshotDB)
	if err != nil {
		return "", "", 0, fmt.Errorf("open copied snapshot for key-image anchor: %w", err)
	}
	defer checkedio.CloseReadResult(db, &err)
	canonical, found, err := db.GetCanonicalHash(doc.Height)
	if err != nil || !found || hex.EncodeToString(canonical[:]) != doc.BlockHash {
		return "", "", 0, errors.New("key-image anchor block hash does not match the copied database")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), doc.BlockHash, doc.Height, nil
}

func decodeKeyImageAnchor(raw []byte) (keyImageAnchorDocument, error) {
	var doc keyImageAnchorDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return doc, errors.New("key-image anchor must be a JSON object")
	}
	seen := make(map[string]bool, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return doc, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return doc, errors.New("key-image anchor has an invalid or duplicate field")
		}
		seen[key] = true
		switch key {
		case "height":
			err = decoder.Decode(&doc.Height)
		case "block_hash":
			err = decoder.Decode(&doc.BlockHash)
		case "key_images":
			err = decodeKeyImageAnchorArray(decoder, &doc.KeyImages)
		default:
			return doc, fmt.Errorf("key-image anchor has unknown field %q", key)
		}
		if err != nil {
			return doc, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return doc, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return doc, errors.New("key-image anchor has trailing data")
	}
	if !seen["height"] || !seen["block_hash"] || !seen["key_images"] {
		return doc, errors.New("key-image anchor is missing a required field")
	}
	return doc, nil
}

func decodeKeyImageAnchorArray(decoder *json.Decoder, images *[]chaincrypto.KeyImage) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return errors.New("key_images must be an array")
	}
	seen := make(map[chaincrypto.KeyImage]struct{})
	for decoder.More() {
		if len(*images) >= maxKeyImageAnchorEntries {
			return errors.New("key-image anchor exceeds the 250000-entry limit")
		}
		var image chaincrypto.KeyImage
		if err := decoder.Decode(&image); err != nil {
			return err
		}
		canonical, err := chaincrypto.CanonicalKeyImage(image)
		if err != nil || canonical != image {
			return errors.New("key-image anchor contains a non-canonical image")
		}
		if _, duplicate := seen[image]; duplicate {
			return errors.New("key-image anchor contains a duplicate image")
		}
		seen[image] = struct{}{}
		*images = append(*images, image)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return errors.New("key_images array is malformed")
	}
	return nil
}
