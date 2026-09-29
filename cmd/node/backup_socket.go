package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aperod/aperod/crypto"
)

const backupSocketName = ".aperod-backup.sock"

type logicalBackupExporter interface {
	ExportLogicalBackup(context.Context, string) (crypto.Hash32, uint64, error)
}

type backupSocket struct {
	server    *http.Server
	listener  net.Listener
	path      string
	closeOnce sync.Once
	request   chan struct{}
	exporter  logicalBackupExporter
	dataDir   string
	mu        sync.Mutex
	cond      *sync.Cond
	closed    bool
	active    int
	ctx       context.Context
	cancel    context.CancelFunc
}

type checkpointManifest struct {
	Success   bool   `json:"success"`
	TipHash   string `json:"tip_hash"`
	TipHeight uint64 `json:"tip_height"`
}

type checkpointResponse struct {
	Path      string `json:"path"`
	TipHeight uint64 `json:"tip_height"`
	TipHash   string `json:"tip_hash"`
}

// startBackupSocket exposes only a local, single-operation checkpoint trigger.
// dataDir must be the same directory used to open chain.db.
func startBackupSocket(dataDir string, exporter logicalBackupExporter) (*backupSocket, error) {
	if exporter == nil {
		return nil, errors.New("backup socket: exporter is required")
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("backup socket: resolve data directory: %w", err)
	}
	socketPath := filepath.Join(absDataDir, backupSocketName)
	if info, err := os.Lstat(socketPath); err == nil {
		if err := removeStaleBackupSocket(socketPath, info); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("backup socket: inspect socket path: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("backup socket: listen: %w", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("backup socket: set socket permissions: %w", err)
	}

	socketCtx, cancel := context.WithCancel(context.Background())
	s := &backupSocket{
		listener: listener,
		path:     socketPath,
		request:  make(chan struct{}, 1),
		exporter: exporter,
		dataDir:  absDataDir,
		ctx:      socketCtx,
		cancel:   cancel,
	}
	s.cond = sync.NewCond(&s.mu)
	mux := http.NewServeMux()
	mux.HandleFunc("/checkpoint", s.handleCheckpoint)
	s.server = &http.Server{Handler: mux}
	go func() {
		_ = s.server.Serve(listener)
	}()
	return s, nil
}

// removeStaleBackupSocket only unlinks a same-user Unix socket after a
// connection-refused/timeout result. The inode check prevents replacing a
// different path if another process changes it while liveness is checked.
func removeStaleBackupSocket(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("backup socket: refusing to replace non-socket path %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("backup socket: refusing to replace socket not owned by this user: %s", path)
	}
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("backup socket: another node is listening at %s", path)
	}
	var netErr net.Error
	if !errors.Is(err, syscall.ECONNREFUSED) && !(errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("backup socket: could not verify existing socket %s: %w", path, err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("backup socket: re-inspect stale socket: %w", err)
	}
	currentStat, ok := current.Sys().(*syscall.Stat_t)
	if current.Mode()&os.ModeSocket == 0 || !ok ||
		currentStat.Uid != uint32(os.Geteuid()) ||
		!os.SameFile(info, current) {
		return fmt.Errorf("backup socket: existing path changed during stale check: %s", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup socket: remove stale socket: %w", err)
	}
	return nil
}

func (s *backupSocket) beginRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active++
	return true
}

func (s *backupSocket) endRequest() {
	s.mu.Lock()
	s.active--
	if s.active == 0 {
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

func (s *backupSocket) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/checkpoint" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// This endpoint has no request parameters; reject payloads instead of
	// accepting caller-controlled paths or other incidental input.
	if r.ContentLength != 0 {
		http.Error(w, "request body must be empty", http.StatusBadRequest)
		return
	}
	if !s.beginRequest() {
		http.Error(w, "backup socket is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.endRequest()
	select {
	case s.request <- struct{}{}:
		defer func() { <-s.request }()
	default:
		http.Error(w, "checkpoint already in progress", http.StatusServiceUnavailable)
		return
	}

	stagingDir := filepath.Join(s.dataDir, ".backup-staging")
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		http.Error(w, "could not prepare backup staging directory", http.StatusInternalServerError)
		return
	}
	if err := os.Chmod(stagingDir, 0700); err != nil {
		http.Error(w, "could not secure backup staging directory", http.StatusInternalServerError)
		return
	}
	stage, err := os.MkdirTemp(stagingDir, "checkpoint-")
	if err != nil {
		http.Error(w, "could not create backup stage", http.StatusInternalServerError)
		return
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()

	chainDB := filepath.Join(stage, "chain.db")
	ctx, cancel := context.WithCancel(r.Context())
	stopCancel := context.AfterFunc(s.serverContext(), cancel)
	defer func() {
		stopCancel()
		cancel()
	}()
	tipHash, tipHeight, err := s.exporter.ExportLogicalBackup(ctx, chainDB)
	if err != nil {
		http.Error(w, "logical backup failed", http.StatusInternalServerError)
		return
	}
	hashHex := fmt.Sprintf("%x", tipHash[:])

	manifestPath := filepath.Join(stage, "manifest.json")
	manifest, err := os.OpenFile(manifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		http.Error(w, "could not write backup manifest", http.StatusInternalServerError)
		return
	}
	encodeErr := json.NewEncoder(manifest).Encode(checkpointManifest{
		Success:   true,
		TipHash:   hashHex,
		TipHeight: tipHeight,
	})
	closeErr := manifest.Close()
	if encodeErr != nil || closeErr != nil {
		http.Error(w, "could not complete backup manifest", http.StatusInternalServerError)
		return
	}
	if err := ctx.Err(); err != nil {
		http.Error(w, "checkpoint request canceled", http.StatusRequestTimeout)
		return
	}

	response := checkpointResponse{Path: stage, TipHeight: tipHeight, TipHash: hashHex}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		return
	}
	// The stage is complete and must remain unchanged once published.
	published = true
}

func (s *backupSocket) serverContext() context.Context {
	// The server lifetime is represented by the cancel function's context.
	// Keep it separately via the channel returned by context.AfterFunc below.
	return s.ctx
}

func (s *backupSocket) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		s.mu.Unlock()
		closeErr = s.server.Close()
		_ = s.listener.Close()
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) && closeErr == nil {
			closeErr = err
		}
		s.mu.Lock()
		for s.active > 0 {
			s.cond.Wait()
		}
		s.mu.Unlock()
	})
	return closeErr
}
