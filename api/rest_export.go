package api

// Snapshot and chain-database export endpoints for the node-join workflow.
//
// Routes (registered in registerRESTRoutes):
//
//	GET /api/v1/snapshot/export   — streams the latest UTXO snapshot (.json.gz) as-is.
//	GET /api/v1/chaindb/export    — streams a closed logical chain.db checkpoint as .tar.gz.
//
// Both endpoints require the X-API-Key header when the node is configured with
// an API key (api.key in node.yaml).  Without a key the node is assumed to be in
// development mode and both endpoints are open.
//
// The chain database endpoint exports a closed logical LevelDB checkpoint, not
// a directory walk of the active database. The checkpoint is staged privately
// and removed after the response completes or the client disconnects.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aperod/aperod/internal/checkedio"
)

const chainDBExportWriteTimeout = 2 * time.Hour

// requireAPIKeyOrOpen returns 403 when the node is configured with an API key
// and the caller omits or mismatches it.  When no key is configured the node is
// in dev mode and the check is skipped.
func (s *Server) requireAPIKeyOrOpen(w http.ResponseWriter, r *http.Request) bool {
	if s.apiKey == "" {
		return true // dev mode — no auth
	}
	if r.Header.Get("X-API-Key") != s.apiKey {
		writeJSONError(w, http.StatusForbidden, "invalid or missing X-API-Key")
		return false
	}
	return true
}

// restSnapshotExport serves the latest UTXO snapshot file as a binary download.
// The client (aperod-join.sh) writes this file directly to the new node's
// data_dir; the node reads it on startup via the fast-path logic in snapshot.go.
//
// Snapshot filename format: snapshot-v2-<height>.json.gz.
func (s *Server) restSnapshotExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.requireAPIKeyOrOpen(w, r) {
		return
	}

	dataDir := s.dataDir
	if dataDir == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "export not available: data_dir not configured")
		return
	}

	// Find the highest-height v2 snapshot primary file in the data directory.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot read data dir: "+err.Error())
		return
	}

	const prefix = "snapshot-v2-"
	type candidate struct {
		height uint64
		name   string
	}
	var candidates []candidate
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, "-prev.json.gz") {
			continue
		}
		if !strings.HasSuffix(name, ".json.gz") {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		rest = strings.TrimSuffix(rest, ".json.gz")
		h, parseErr := strconv.ParseUint(rest, 10, 64)
		if parseErr != nil {
			continue
		}
		candidates = append(candidates, candidate{h, name})
	}

	if len(candidates) == 0 {
		writeJSONError(w, http.StatusNotFound, "no snapshot available yet; wait for the node to produce one")
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].height > candidates[j].height
	})
	best := candidates[0]
	f, err := openSnapshotForExport(dataDir, best.name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "open snapshot: "+err.Error())
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			s.log.Debug("close exported snapshot reader failed", "err", err)
		}
	}()

	// Stat via the open fd to get an accurate, race-free size.
	info, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stat snapshot: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, best.name))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Snapshot-Height", strconv.FormatUint(best.height, 10))
	// X-Snapshot-Filename is constrained to "snapshot-v2-<digits>.json.gz" by the
	// selection logic above; no path traversal is possible.
	w.Header().Set("X-Snapshot-Filename", best.name)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Debug("snapshot export stream interrupted", "err", err)
	}
}

// restChainDBExport creates an isolated logical checkpoint through LevelDB's
// point-in-time snapshot API, then streams only that closed checkpoint. It must
// never archive the live chain.db directory by walking files: that can omit
// unflushed writes, WAL state, or files replaced during compaction.
func (s *Server) restChainDBExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.requireAPIKeyOrOpen(w, r) {
		return
	}
	if s.dataDir == "" || s.blockStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "export not available: live chain store is not configured")
		return
	}
	// A logical snapshot can take longer than the API server's general
	// WriteTimeout before the first response byte is sent. Extend this request's
	// deadline before doing that work and streaming the resulting archive.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(chainDBExportWriteTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		writeJSONError(w, http.StatusInternalServerError, "cannot extend checkpoint export deadline")
		return
	}

	// Bound duplicate exports. ExportLogicalBackup also enforces a live disk
	// reserve before and during the logical copy.
	s.chainDBExportMu.Lock()
	defer s.chainDBExportMu.Unlock()

	stagingRoot := filepath.Join(s.dataDir, ".backup-staging")
	if err := os.MkdirAll(stagingRoot, 0700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot prepare private export staging")
		return
	}
	if info, err := os.Lstat(stagingRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		writeJSONError(w, http.StatusInternalServerError, "export staging path is not a safe directory")
		return
	}
	// #nosec G302 -- Lstat above proves this is a directory. Owner traversal
	// is required; group/other permissions remain zero.
	if err := os.Chmod(stagingRoot, 0700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot secure export staging directory")
		return
	}
	stage, err := os.MkdirTemp(stagingRoot, "join-export-")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot create private export stage")
		return
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			s.log.Error("cannot remove private chain export staging", "err", err)
		}
	}()

	chainDBPath := filepath.Join(stage, "chain.db")
	tipHash, tipHeight, err := s.blockStore.ExportLogicalBackup(r.Context(), chainDBPath)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusRequestTimeout
		}
		writeJSONError(w, status, "logical chain checkpoint could not be completed")
		return
	}

	exportRoot, err := os.OpenRoot(stage)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot open isolated checkpoint root")
		return
	}
	defer checkedio.LogClose(s.log, exportRoot, "logical checkpoint export root")

	gw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot initialize checkpoint archive")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="chaindb-checkpoint.tar.gz"`)
	w.Header().Set("X-ChainDB-Tip-Height", strconv.FormatUint(tipHeight, 10))
	w.Header().Set("X-ChainDB-Tip-Hash", fmt.Sprintf("%x", tipHash[:]))
	w.WriteHeader(http.StatusOK)

	tw := tar.NewWriter(gw)

	walkErr := filepath.Walk(filepath.Join(stage, "chain.db"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		rel = filepath.Clean(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) ||
			filepath.IsAbs(rel) {
			return fmt.Errorf("unsafe logical checkpoint archive path %q", rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("logical checkpoint contains unexpected symlink %q", rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
			hdr.Mode = 0755
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("logical checkpoint contains unsupported file %q", rel)
		}
		hdr.Mode = 0644
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := checkedio.OpenRootedRegular(exportRoot, rel)
		if err != nil {
			return err
		}
		opened, statErr := f.Stat()
		if statErr != nil {
			return errors.Join(statErr, f.Close())
		}
		if !os.SameFile(info, opened) {
			return errors.Join(fmt.Errorf("logical checkpoint changed while archiving %q", rel), f.Close())
		}
		_, copyErr := io.CopyN(tw, f, info.Size())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = gw.Close()
		return
	}
	_ = tw.Close()
	_ = gw.Close()
}
