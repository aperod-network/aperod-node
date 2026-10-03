// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const (
	walletSnapshotTTL             = 10 * time.Minute
	walletSnapshotGlobalLimit     = 16
	walletSnapshotAddressLimit    = 2
	walletSnapshotCheckpointLimit = 32 << 20
)

var errWalletSnapshotExpired = errors.New("wallet snapshot expired or not found")

type walletSnapshotEntry struct {
	id               string
	address          crypto.Address
	read             *store.WalletReadSnapshot
	checkpoint       *store.LPoDCheckpoint
	hash             crypto.Hash32
	height           uint64
	cursorKey        [32]byte
	bytes            int
	expiresAt        time.Time
	inflight         int
	closed           bool
	pendingOnce      sync.Once
	pendingKeyImages []string
	pendingErr       error
	outputTargetSet  bool
	outputTarget     uint64
	outputTargetHash crypto.Hash32
	witnessSet       bool
	witnessHeight    uint64
	witnessHash      crypto.Hash32
}

type walletSnapshotManager struct {
	mu              sync.Mutex
	createMu        sync.Mutex
	entries         map[string]*walletSnapshotEntry
	active          int
	activeByAddress map[crypto.Address]int
	checkpointBytes int
}

func newWalletSnapshotManager() *walletSnapshotManager {
	return &walletSnapshotManager{
		entries:         make(map[string]*walletSnapshotEntry),
		activeByAddress: make(map[crypto.Address]int),
	}
}

// expireLocked lazily retires expired snapshots. In-flight readers retain
// their LevelDB view until their final lease is returned.
func (m *walletSnapshotManager) expireLocked(now time.Time) {
	for id, entry := range m.entries {
		if !now.Before(entry.expiresAt) {
			delete(m.entries, id)
			entry.closed = true
			m.releaseIfIdleLocked(entry)
		}
	}
}

func (m *walletSnapshotManager) expire() {
	m.mu.Lock()
	m.expireLocked(time.Now())
	m.mu.Unlock()
}

func (m *walletSnapshotManager) closeAll() {
	m.mu.Lock()
	for id, entry := range m.entries {
		delete(m.entries, id)
		entry.closed = true
		m.releaseIfIdleLocked(entry)
	}
	m.mu.Unlock()
}

func (m *walletSnapshotManager) releaseIfIdleLocked(entry *walletSnapshotEntry) {
	if !entry.closed || entry.inflight != 0 || entry.read == nil {
		return
	}
	entry.read.Release()
	entry.read = nil
	entry.checkpoint = nil
	m.active--
	m.activeByAddress[entry.address]--
	if m.activeByAddress[entry.address] == 0 {
		delete(m.activeByAddress, entry.address)
	}
	m.checkpointBytes -= entry.bytes
	if m.checkpointBytes < 0 {
		m.checkpointBytes = 0
	}
	entry.bytes = 0
}

type walletSnapshotLease struct {
	manager *walletSnapshotManager
	entry   *walletSnapshotEntry
	once    sync.Once
}

func (l *walletSnapshotLease) Release() {
	if l == nil || l.manager == nil || l.entry == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		m.mu.Lock()
		l.entry.inflight--
		if l.entry.inflight < 0 {
			l.entry.inflight = 0
		}
		m.releaseIfIdleLocked(l.entry)
		m.mu.Unlock()
	})
}

func (m *walletSnapshotManager) acquire(id string, address crypto.Address) (*walletSnapshotLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(time.Now())
	entry := m.entries[id]
	if entry == nil || entry.closed {
		return nil, errWalletSnapshotExpired
	}
	if entry.address != address {
		return nil, errors.New("snapshot address does not match")
	}
	entry.inflight++
	return &walletSnapshotLease{manager: m, entry: entry}, nil
}

func (m *walletSnapshotManager) close(id string, address crypto.Address) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(time.Now())
	entry := m.entries[id]
	if entry == nil || entry.closed {
		return errWalletSnapshotExpired
	}
	if entry.address != address {
		return errors.New("snapshot address does not match")
	}
	delete(m.entries, id)
	entry.closed = true
	m.releaseIfIdleLocked(entry)
	return nil
}

func (m *walletSnapshotManager) bindOutputTarget(entry *walletSnapshotEntry, height uint64, hash crypto.Hash32) (uint64, crypto.Hash32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.outputTargetSet {
		if entry.outputTarget != height || entry.outputTargetHash != hash {
			return 0, crypto.Hash32{}, errors.New("bootstrap target does not match snapshot")
		}
		return entry.outputTarget, entry.outputTargetHash, nil
	}
	entry.outputTargetSet = true
	entry.outputTarget = height
	entry.outputTargetHash = hash
	return height, hash, nil
}

func (m *walletSnapshotManager) outputTargetFor(entry *walletSnapshotEntry) (uint64, crypto.Hash32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return entry.outputTarget, entry.outputTargetHash, entry.outputTargetSet
}

func (m *walletSnapshotManager) bindWitness(entry *walletSnapshotEntry, height uint64, hash crypto.Hash32) (uint64, crypto.Hash32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.witnessSet {
		if entry.witnessHeight != height || entry.witnessHash != hash {
			return 0, crypto.Hash32{}, errors.New("witness does not match snapshot")
		}
		return entry.witnessHeight, entry.witnessHash, nil
	}
	entry.witnessSet = true
	entry.witnessHeight = height
	entry.witnessHash = hash
	return height, hash, nil
}

func (m *walletSnapshotManager) witnessFor(entry *walletSnapshotEntry) (uint64, crypto.Hash32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return entry.witnessHeight, entry.witnessHash, entry.witnessSet
}

func (m *walletSnapshotManager) capture(ctx context.Context, s *Server, address crypto.Address) (*walletSnapshotLease, error) {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wallet snapshot capture canceled: %w", err)
	}
	m.mu.Lock()
	m.expireLocked(time.Now())
	if m.active >= walletSnapshotGlobalLimit {
		m.mu.Unlock()
		return nil, errors.New("wallet snapshot capacity reached")
	}
	if m.activeByAddress[address] >= walletSnapshotAddressLimit {
		m.mu.Unlock()
		return nil, errors.New("wallet snapshot address capacity reached")
	}
	availableBytes := walletSnapshotCheckpointLimit - m.checkpointBytes
	m.mu.Unlock()
	if availableBytes <= 0 {
		return nil, errors.New("wallet snapshot checkpoint memory limit reached")
	}

	s.walletSnapshotCaptureMu.RLock()
	capture := s.walletSnapshotCapture
	s.walletSnapshotCaptureMu.RUnlock()
	if capture == nil {
		return nil, errors.New("wallet snapshot capture is unavailable")
	}
	read, err := capture()
	if err != nil {
		if read != nil {
			read.Release()
		}
		return nil, fmt.Errorf("capture wallet snapshot: %w", err)
	}
	if read == nil {
		return nil, errors.New("capture wallet snapshot returned nil")
	}
	fail := func(err error) (*walletSnapshotLease, error) {
		read.Release()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("wallet snapshot capture canceled: %w", err))
	}

	hash, height, err := read.GetTip()
	if err != nil {
		return fail(fmt.Errorf("read pinned canonical tip: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("wallet snapshot capture canceled: %w", err))
	}
	indexed, found, err := read.GetCanonicalHash(height)
	if err != nil || !found || indexed != hash {
		return fail(errors.New("pinned canonical height index does not match tip"))
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("wallet snapshot capture canceled: %w", err))
	}
	if s.lpodMigration == nil || s.lpodFinalized == nil || !s.lpodFinalized(height, hash) {
		return fail(errors.New("hash-specific finalized checkpoint required"))
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("wallet snapshot capture canceled: %w", err))
	}
	checkpoint, err := read.LoadLPoDCheckpointAtBounded(hash, availableBytes)
	if ctx.Err() != nil {
		return fail(fmt.Errorf("wallet snapshot capture canceled: %w", ctx.Err()))
	}
	if err != nil || checkpoint == nil || checkpoint.Allocation == nil {
		if err == nil {
			err = errors.New("pinned LPoD checkpoint is missing")
		}
		return fail(fmt.Errorf("load bounded LPoD checkpoint: %w", err))
	}
	allocation := checkpoint.Allocation
	if allocation.Genesis != s.lpodMigration.Genesis ||
		allocation.ReconciliationRoot != s.lpodMigration.ReconciliationRoot ||
		allocation.FundingHeight != s.lpodMigration.Height ||
		checkpoint.State.LastHeight != height {
		return fail(errors.New("pinned LPoD checkpoint does not match configured migration and height"))
	}
	if err := checkpoint.State.Validate(); err != nil {
		return fail(fmt.Errorf("invalid pinned LPoD checkpoint state: %w", err))
	}
	ready, err := read.LPoDWalletIndexReady(allocation.FundingBlock)
	if err != nil || !ready {
		if err == nil {
			err = errors.New("native wallet index is not ready at pinned snapshot")
		}
		return fail(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fail(fmt.Errorf("measure pinned checkpoint: %w", err))
	}
	checkpointSize := len(encoded)
	if checkpointSize > availableBytes || checkpointSize > walletSnapshotCheckpointLimit {
		return fail(errors.New("wallet snapshot checkpoint memory limit reached"))
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("wallet snapshot capture canceled: %w", err))
	}

	var randomID [32]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return fail(fmt.Errorf("generate wallet snapshot id: %w", err))
	}
	var cursorKey [32]byte
	if _, err := rand.Read(cursorKey[:]); err != nil {
		return fail(fmt.Errorf("generate wallet snapshot cursor key: %w", err))
	}
	id := hex.EncodeToString(randomID[:])
	entry := &walletSnapshotEntry{
		id: id, address: address, read: read, checkpoint: checkpoint,
		hash: hash, height: height, cursorKey: cursorKey, bytes: checkpointSize,
		expiresAt: time.Now().Add(walletSnapshotTTL), inflight: 1,
	}

	m.mu.Lock()
	m.expireLocked(time.Now())
	if _, exists := m.entries[id]; exists {
		m.mu.Unlock()
		return fail(errors.New("wallet snapshot id collision"))
	}
	m.entries[id] = entry
	m.active++
	m.activeByAddress[address]++
	m.checkpointBytes += checkpointSize
	m.mu.Unlock()
	return &walletSnapshotLease{manager: m, entry: entry}, nil
}

func writeWalletSnapshotError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}

func writeSnapshotLeaseError(w http.ResponseWriter, err error) {
	if errors.Is(err, errWalletSnapshotExpired) {
		writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_EXPIRED", err.Error())
		return
	}
	if strings.Contains(err.Error(), "address does not match") {
		writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_ADDRESS_MISMATCH", err.Error())
		return
	}
	writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", err.Error())
}

func validSnapshotAddress(text string) (crypto.Address, error) {
	address := crypto.Address(text)
	if _, _, _, err := crypto.DecodeAddress(address); err != nil {
		return "", err
	}
	return address, nil
}

func snapshotCursor(id, cursor string) string {
	if cursor == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(id + ":" + cursor))
}

func unwrapSnapshotCursor(id, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errors.New("invalid snapshot cursor")
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != id || parts[1] == "" {
		return "", errors.New("snapshot cursor belongs to a different session")
	}
	return parts[1], nil
}

func writeCreatedWalletSnapshot(w http.ResponseWriter, r *http.Request, body interface{}) bool {
	encoded, err := json.Marshal(body)
	if err != nil || r.Context().Err() != nil {
		return false
	}
	encoded = append(encoded, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	n, err := w.Write(encoded)
	return err == nil && n == len(encoded)
}

func (s *Server) restLPoDWalletOutputsSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	address, err := validSnapshotAddress(q.Get("address"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid address")
		return
	}
	id := q.Get("snapshot_id")
	cursor := q.Get("cursor")
	resumeCursor := q.Get("resume_cursor")
	creating := q.Get("snapshot") == "1"
	var lease *walletSnapshotLease
	if creating {
		if id != "" || cursor != "" {
			writeJSONError(w, http.StatusBadRequest, "snapshot creation cannot include snapshot_id or cursor")
			return
		}
		if s.walletSnapshots == nil {
			writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", "wallet snapshot manager unavailable")
			return
		}
		lease, err = s.walletSnapshots.capture(r.Context(), s, address)
	} else if q.Has("snapshot_id") {
		if cursor != "" && resumeCursor != "" {
			writeJSONError(w, http.StatusBadRequest, "cursor and resume_cursor are mutually exclusive")
			return
		}
		cursor, err = unwrapSnapshotCursor(id, cursor)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", err.Error())
			return
		}
		if id == "" || s.walletSnapshots == nil {
			err = errWalletSnapshotExpired
		} else {
			lease, err = s.walletSnapshots.acquire(id, address)
		}
	} else {
		writeJSONError(w, http.StatusBadRequest, "snapshot_id or snapshot=1 required")
		return
	}
	if err != nil {
		writeSnapshotLeaseError(w, err)
		return
	}
	if id == "" {
		id = lease.entry.id
	}
	delivered := !creating
	defer func() {
		if creating && !delivered {
			_ = s.walletSnapshots.close(lease.entry.id, address)
		}
		lease.Release()
	}()
	entry := lease.entry
	throughHeight, throughHash, targetBound := s.walletSnapshots.outputTargetFor(entry)
	if !targetBound {
		throughHeight, throughHash = entry.height, entry.hash
	}
	if q.Has("through_height") != q.Has("through_hash") {
		writeJSONError(w, http.StatusBadRequest, "through_height and through_hash must be supplied together")
		return
	}
	if q.Has("through_height") {
		requestedThroughHeight, parseErr := strconv.ParseUint(q.Get("through_height"), 10, 64)
		if parseErr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid through_height")
			return
		}
		requestedThroughHash, parseErr := parseHash32(q.Get("through_hash"))
		if parseErr != nil {
			writeJSONError(w, http.StatusBadRequest, "through_hash must be 64 hexadecimal characters")
			return
		}
		if targetBound && (requestedThroughHeight != throughHeight || requestedThroughHash != throughHash) {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "through target does not match the snapshot bootstrap")
			return
		}
		throughHeight, throughHash = requestedThroughHeight, requestedThroughHash
		if throughHeight > entry.height {
			writeWalletSnapshotError(w, http.StatusConflict, "REORG", "through_height is above the captured checkpoint")
			return
		}
		canonical, found, readErr := entry.read.GetCanonicalHash(throughHeight)
		if readErr != nil || !found {
			reconciliationError(w, throughHeight, throughHeight, "through_height canonical header is unavailable")
			return
		}
		if canonical != throughHash {
			reorgError(w, throughHeight, "through_hash is not canonical in the captured snapshot")
			return
		}
		throughHeight, throughHash, err = s.walletSnapshots.bindOutputTarget(entry, throughHeight, throughHash)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", err.Error())
			return
		}
	} else {
		throughHeight, throughHash, err = s.walletSnapshots.bindOutputTarget(entry, throughHeight, throughHash)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", err.Error())
			return
		}
	}
	afterHeight, hasAfterHeight := uint64(0), q.Has("after_height")
	if hasAfterHeight != q.Has("after_hash") {
		writeJSONError(w, http.StatusBadRequest, "after_height and after_hash must be supplied together")
		return
	}
	if hasAfterHeight {
		afterHeight, err = strconv.ParseUint(q.Get("after_height"), 10, 64)
		if err != nil || afterHeight > throughHeight {
			writeJSONError(w, http.StatusBadRequest, "invalid after_height")
			return
		}
		afterHash, parseErr := parseHash32(q.Get("after_hash"))
		if parseErr != nil {
			writeJSONError(w, http.StatusBadRequest, "after_hash must be 64 hexadecimal characters")
			return
		}
		canonical, found, readErr := entry.read.GetCanonicalHash(afterHeight)
		if readErr != nil || !found {
			reconciliationError(w, afterHeight, afterHeight, "after_height canonical header is unavailable")
			return
		}
		if canonical != afterHash {
			reorgError(w, afterHeight, "after_hash is not canonical in the captured snapshot")
			return
		}
	}
	if r.Context().Err() != nil {
		return
	}
	if resumeCursor != "" {
		if cursor != "" {
			writeJSONError(w, http.StatusBadRequest, "cursor and resume_cursor are mutually exclusive")
			return
		}
		cursor = resumeCursor
	}
	rows, next, lastExamined, err := entry.read.LPoDWalletOutputsAfterHeight(
		address, afterHeight, hasAfterHeight, cursor, throughHeight, 128,
	)
	if err != nil {
		var missing *store.WalletReconciliationError
		if errors.As(err, &missing) {
			reconciliationError(w, missing.Height, missing.Height, missing.Error())
			return
		}
		var cursorReorg *store.WalletCursorReorgError
		if errors.As(err, &cursorReorg) {
			reorgError(w, cursorReorg.Height, cursorReorg.Error())
			return
		}
		writeWalletSnapshotError(w, http.StatusBadRequest, "SNAPSHOT_QUERY_FAILED", err.Error())
		return
	}
	outputs := make([]map[string]interface{}, 0, len(rows))
	for _, u := range rows {
		output := map[string]interface{}{
			"tx_hash": fmt.Sprintf("%x", u.TxHash[:]), "out_idx": u.OutputIndex, "block_height": u.BlockHeight,
			"one_time_pub": fmt.Sprintf("%x", u.OneTimePub[:]), "tx_pub_key": fmt.Sprintf("%x", u.TxPubKey[:]),
			"amount_commit": fmt.Sprintf("%x", u.AmountCommit[:]), "enc_amount": fmt.Sprintf("%x", u.EncAmount[:]),
		}
		var zeroPoint crypto.Point32
		if u.TxPubKey == zeroPoint {
			if u.AmountNAPRO > 0 {
				output["amount_napr"] = fmt.Sprintf("%d", u.AmountNAPRO)
			} else {
				output["opening_status"] = "OUTPUT_OPENING_UNAVAILABLE"
			}
		}
		outputs = append(outputs, output)
	}
	response := map[string]interface{}{
		"state": "active", "snapshot_id": id, "checkpoint_height": lease.entry.height,
		"checkpoint_hash": fmt.Sprintf("%x", lease.entry.hash[:]),
		"through_height":  throughHeight, "through_hash": fmt.Sprintf("%x", throughHash[:]),
		"outputs": outputs, "next_cursor": snapshotCursor(id, next),
		"resume_cursor": lastExamined,
	}
	if creating {
		delivered = writeCreatedWalletSnapshot(w, r, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) restWalletSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	var request struct {
		Address    string `json:"address"`
		SnapshotID string `json:"snapshot_id"`
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "snapshot release body too large")
			return
		}
		if len(body) != 0 {
			if err := json.Unmarshal(body, &request); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid snapshot release body")
				return
			}
		}
	}
	q := r.URL.Query()
	if request.Address == "" {
		request.Address = q.Get("address")
	}
	if request.SnapshotID == "" {
		request.SnapshotID = q.Get("snapshot_id")
	}
	address, err := validSnapshotAddress(request.Address)
	if err != nil || request.SnapshotID == "" {
		writeJSONError(w, http.StatusBadRequest, "address and snapshot_id are required")
		return
	}
	if s.walletSnapshots == nil {
		err = errWalletSnapshotExpired
	} else {
		err = s.walletSnapshots.close(request.SnapshotID, address)
	}
	if err != nil {
		writeSnapshotLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"released": true})
}

func (s *Server) acquireWalletSnapshot(w http.ResponseWriter, id string, address crypto.Address) *walletSnapshotLease {
	if s.walletSnapshots == nil {
		writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_EXPIRED", errWalletSnapshotExpired.Error())
		return nil
	}
	lease, err := s.walletSnapshots.acquire(id, address)
	if err != nil {
		writeSnapshotLeaseError(w, err)
		return nil
	}
	return lease
}

func (s *Server) restWalletKeyImagesSnapshot(
	w http.ResponseWriter,
	images []string,
	refs []struct {
		Hash  string `json:"tx_hash"`
		Index uint32 `json:"out_idx"`
	},
	id, addressText string,
) {
	address, err := validSnapshotAddress(addressText)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "valid address required with snapshot_id")
		return
	}
	lease := s.acquireWalletSnapshot(w, id, address)
	if lease == nil {
		return
	}
	defer lease.Release()

	pending := make(map[string]bool)
	if s.mempool != nil {
		for _, hash := range s.mempool.Hashes() {
			tx, ok := s.mempool.Get(hash)
			if !ok {
				continue
			}
			for _, input := range tx.Inputs {
				ki, err := crypto.CanonicalKeyImage(input.KeyImage)
				if err == nil {
					pending[hex.EncodeToString(ki[:])] = true
				}
			}
		}
	}

	statuses := make(map[string]bool)
	canonicalSpent := make(map[string]bool)
	pendingLocked := make(map[string]bool)
	for i, text := range images {
		raw, err := hex.DecodeString(text)
		if err != nil || len(raw) != 32 {
			writeJSONError(w, http.StatusBadRequest, "invalid key image")
			return
		}
		var ki crypto.KeyImage
		copy(ki[:], raw)
		ki, err = crypto.CanonicalKeyImage(ki)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid key image")
			return
		}
		spent, err := lease.entry.read.IsKeyImageSpent(ki)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_QUERY_FAILED", "pinned key image index unavailable")
			return
		}
		ref := refs[i]
		raw, err = hex.DecodeString(ref.Hash)
		if err != nil || len(raw) != 32 {
			writeJSONError(w, http.StatusBadRequest, "invalid source hash")
			return
		}
		var txHash crypto.Hash32
		copy(txHash[:], raw)
		utxo, err := lease.entry.read.GetUTXO(txHash, ref.Index)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_QUERY_FAILED", "pinned output index unavailable")
			return
		}
		directSpent, err := lease.entry.read.IsUTXOSpentChecked(txHash, ref.Index)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_QUERY_FAILED", "pinned source index unavailable")
			return
		}
		if utxo == nil && !spent && !directSpent {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_SOURCE_MISSING", "source is not indexed at the pinned checkpoint")
			return
		}
		canonicalSpent[text] = spent || utxo == nil || directSpent
		pendingLocked[text] = pending[hex.EncodeToString(ki[:])]
		statuses[text] = canonicalSpent[text] || pendingLocked[text]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"spent": statuses, "canonical_spent": canonicalSpent, "pending_locked": pendingLocked,
		"snapshot_id":       lease.entry.id,
		"checkpoint_height": lease.entry.height,
		"checkpoint_hash":   fmt.Sprintf("%x", lease.entry.hash[:]),
	})
}
