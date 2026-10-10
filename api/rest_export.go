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
// Safety model for /api/v1/chaindb/export:
//
//	LevelDB SST files (*.ldb, *.sst) are immutable once written — safe to stream live.
//	WAL files (*.log) are actively written and are therefore excluded from the archive;
//	LevelDB treats a missing WAL as a clean shutdown and opens without it.
//	MANIFEST-* and CURRENT are small and stable enough to read consistently.
//
//	For each file the handler opens it before building the tar header so the size
//	in the header is derived from the open file descriptor (not Walk's pre-open
//	FileInfo), minimising the time-of-check/time-of-use window.  io.LimitReader
//	caps the copy at exactly hdr.Size bytes; any shortfall is padded with zeros so
//	the tar block boundary stays consistent.
//
//	Tar entry paths are sanitised: only relative paths that do not escape the
//	archive root are included.

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
	snapPath := filepath.Join(dataDir, best.name)

	f, err := os.Open(snapPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "open snapshot: "+err.Error())
		return
	}
	defer f.Close()

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
	_, _ = io.Copy(w, f)
}

// restChainDBExport streams the chain.db LevelDB directory as a gzip-compressed
// tar archive suitable for extracting directly into the joining node's data_dir.
//
// Security: requires X-API-Key when the node is configured with one.
// This endpoint is not restricted to localhost because the joining node pulls
// from a different host; callers should use an SSH tunnel or VPN in production
// to avoid transmitting the API key in cleartext over untrusted networks.
//
// The archive root is "chain.db/" so the receiver extracts with:
//
//	tar -xzf chaindb.tar.gz -C <data_dir>
//
// WAL files (*.log) are excluded; see package comment for rationale.
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

	_ = filepath.Walk(chainDBPath, func(path string, walkInfo os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Skip unreadable entries rather than aborting the whole archive;
			// the joining node's chain.db may still be usable without them.
			return nil
		}

		// Compute and sanitise the archive entry path.
		rel, relErr := filepath.Rel(dataDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.Clean(rel)
		// Reject any path that escapes the archive root (e.g. "../../etc").
		if strings.HasPrefix(rel, "..") {
			return nil
		}

		if walkInfo.IsDir() {
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     filepath.ToSlash(rel) + "/",
				Mode:     0755,
			}
			// Directory header errors abort the Walk so the tar is not silently
			// truncated mid-archive.
			return tw.WriteHeader(hdr)
		}

		// Exclude LevelDB WAL files (*.log).  They are actively written to
		// while the node runs; including a partially-written WAL would make
		// the tar header size disagree with the bytes copied, corrupting the
		// archive framing.  LevelDB opens cleanly without a WAL — it treats
		// the absence as a clean shutdown (all committed data is in SSTs).
		if strings.HasSuffix(path, ".log") {
			return nil
		}

		// Open the file before building the tar header so the size comes from
		// the open fd (via Stat), not from Walk's pre-open FileInfo.  This
		// minimises the TOCTOU window: Walk captures FileInfo before the file
		// is opened; if a compaction replaced the file in between, the size in
		// the header would disagree with bytes copied.
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil // skip unreadable files; do not abort the archive
		}
		defer f.Close()

		fdInfo, statErr := f.Stat()
		if statErr != nil {
			return nil
		}

		hdr, hdrErr := tar.FileInfoHeader(fdInfo, "")
		if hdrErr != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(rel)
		// hdr.Size comes from fdInfo.Size() — derived from the open fd.

		if err := tw.WriteHeader(hdr); err != nil {
			return err // propagate; abort Walk on tar write failure
		}

		// Copy exactly hdr.Size bytes.  LimitReader stops at hdr.Size even
		// if the file grew after Stat (impossible for immutable SST files but
		// a defensive guard for MANIFEST/CURRENT which are atomically replaced).
		written, copyErr := io.Copy(tw, io.LimitReader(f, hdr.Size))
		if written < hdr.Size {
			// Pad any shortfall with zeros to keep the tar block boundary
			// consistent.  This handles the (theoretical) case where a file
			// shrank between fd.Stat() and io.Copy.
			zeros := make([]byte, hdr.Size-written)
			if _, padErr := tw.Write(zeros); padErr != nil {
				return padErr
			}
		}
		if copyErr != nil && copyErr != io.EOF {
			// Individual file copy error: skip the remainder of this file
			// but continue with the next entry so the archive is as complete
			// as possible.
			return nil
		}
		return nil
	})

	// Close tar and gzip writers; errors here mean the receiver's archive is
	// incomplete.  We have already committed status 200 and cannot change it,
	// so the receiver detects the corruption via its own archive integrity check.
	_ = tw.Close()
	_ = gw.Close()
}
