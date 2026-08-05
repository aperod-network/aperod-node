package api

// Snapshot and chain-database export endpoints for the node-join workflow.
//
// Routes (registered in registerRESTRoutes):
//
//	GET /api/v1/snapshot/export   — streams the latest UTXO snapshot (.json.gz) as-is.
//	GET /api/v1/chaindb/export    — streams the chain.db LevelDB directory as a .tar.gz.
//
// Both endpoints require the X-API-Key header when the node is configured with
// an API key (api.key in node.yaml).  Without a key the node is assumed to be in
// development mode and both endpoints are open.
//
// /api/v1/chaindb/export streams the LevelDB directory while the node is live.
// LevelDB SST files are immutable once written so the streamed archive is
// consistent for all historical data; the current write-ahead log and MANIFEST
// may be slightly ahead of the on-disk state but LevelDB recovers from that
// automatically on the next open.  The endpoint is intentionally not restricted
// to localhost because the joining node must be able to pull from a different
// host.

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

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
// Snapshot filename format: snapshot-v2-<height>.json.gz
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
		if !strings.HasSuffix(name, ".json.gz") {
			continue
		}
		if strings.HasSuffix(name, "-prev.json.gz") {
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
	snapPath := filepath.Join(dataDir, best.name)

	f, err := os.Open(snapPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "open snapshot: "+err.Error())
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stat snapshot: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, best.name))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Snapshot-Height", strconv.FormatUint(best.height, 10))
	w.Header().Set("X-Snapshot-Filename", best.name)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// restChainDBExport streams the chain.db LevelDB directory as a gzip-compressed
// tar archive suitable for extracting directly into the joining node's data_dir.
//
// Security: requires X-API-Key when the node is configured with one.
// This endpoint is intentionally not restricted to localhost so it can be
// called from the aperod-join.sh script running on a different machine.
//
// Safety notes:
//   - LevelDB SST files (*.ldb, *.sst) are immutable — safe to stream live.
//   - MANIFEST and CURRENT files describe the compaction state; they may be
//     briefly stale but LevelDB opens and self-heals on the next start.
//   - The *.log WAL file may be partially written; LevelDB replays from it
//     automatically and discards the uncommitted tail on the next open.
//
// The archive root is "chain.db/" so the receiver extracts with:
//
//	tar -xzf chaindb.tar.gz -C <data_dir>
func (s *Server) restChainDBExport(w http.ResponseWriter, r *http.Request) {
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

	chainDBPath := filepath.Join(dataDir, "chain.db")
	if _, err := os.Stat(chainDBPath); err != nil {
		writeJSONError(w, http.StatusNotFound, "chain.db not found: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="chaindb.tar.gz"`)
	w.WriteHeader(http.StatusOK)

	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)

	// Walk chain.db directory and add all files to the archive.
	// Files are stored under the path "chain.db/<relative>" so the receiver
	// can extract directly into data_dir.
	_ = filepath.Walk(chainDBPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries; do not abort the stream
		}
		rel, relErr := filepath.Rel(dataDir, path)
		if relErr != nil {
			return nil
		}

		hdr, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err // propagate tar write errors to abort early
		}

		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil // skip unreadable files; do not abort the archive
		}
		defer f.Close()
		_, _ = io.Copy(tw, f)
		return nil
	})

	_ = tw.Close()
	_ = gw.Close()
}
