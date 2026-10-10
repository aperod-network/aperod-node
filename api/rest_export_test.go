package api_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
)

// buildExportServer returns a server wired with a data dir and an optional API key.
func buildExportServer(t *testing.T, dataDir string, apiKey string) *api.Server {
	t.Helper()
	chain := core.NewChain()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	utxos := core.NewUTXOSet()
	srv := api.NewServer(":0", chain, mp, utxos, testLogger())
	if dataDir != "" {
		srv.SetDataDir(dataDir)
	}
	if apiKey != "" {
		srv.SetAPIKey(apiKey)
	}
	return srv
}

// ─── /api/v1/snapshot/export ─────────────────────────────────────────────────

func TestSnapshotExport_NoDataDir(t *testing.T) {
	srv := buildExportServer(t, "", "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestSnapshotExport_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestSnapshotExport_NoSnapshots(t *testing.T) {
	dir := t.TempDir()
	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotExport_RequiresAPIKey(t *testing.T) {
	dir := t.TempDir()
	srv := buildExportServer(t, dir, "secret")

	// Missing key → 403
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("no key: status = %d, want 403", rr.Code)
	}

	// Wrong key → 403
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/export", nil)
	req2.Header.Set("X-API-Key", "wrong")
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("wrong key: status = %d, want 403", rr2.Code)
	}
}

func TestSnapshotExport_ServesLatestSnapshot(t *testing.T) {
	dir := t.TempDir()

	// Write two snapshot files; the handler must serve the higher-height one.
	oldData := []byte("old-snapshot-data")
	newData := []byte("new-snapshot-data-longer")
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2-100.json.gz"), oldData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2-200.json.gz"), newData, 0644); err != nil {
		t.Fatal(err)
	}
	// A -prev file should not be selected.
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2-300-prev.json.gz"), []byte("prev"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// Verify the correct snapshot (height 200) was served.
	if got := rr.Header().Get("X-Snapshot-Height"); got != "200" {
		t.Errorf("X-Snapshot-Height = %q, want 200", got)
	}
	if got := rr.Header().Get("X-Snapshot-Filename"); got != "snapshot-v2-200.json.gz" {
		t.Errorf("X-Snapshot-Filename = %q, want snapshot-v2-200.json.gz", got)
	}
	if got := rr.Body.Bytes(); string(got) != string(newData) {
		t.Errorf("body = %q, want %q", got, newData)
	}
}

func TestSnapshotExport_KeyAccepted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapshot-v2-1.json.gz"), []byte("snap"), 0644); err != nil {
		t.Fatal(err)
	}
	srv := buildExportServer(t, dir, "mykey")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/export", nil)
	req.Header.Set("X-API-Key", "mykey")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("valid key: status = %d, want 200", rr.Code)
	}
}

// ─── /api/v1/chaindb/export ──────────────────────────────────────────────────

func TestChainDBExport_NoDataDir(t *testing.T) {
	srv := buildExportServer(t, "", "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestChainDBExport_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestChainDBExport_NoChainDB(t *testing.T) {
	dir := t.TempDir()
	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestChainDBExport_RequiresAPIKey(t *testing.T) {
	dir := t.TempDir()
	// Create a minimal chain.db directory so the endpoint reaches the auth check.
	if err := os.MkdirAll(filepath.Join(dir, "chain.db"), 0755); err != nil {
		t.Fatal(err)
	}
	srv := buildExportServer(t, dir, "secret")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("no key: status = %d, want 403", rr.Code)
	}
}

// writeFakeChainDB creates a fake chain.db directory with immutable SST files
// and a WAL file (*.log) for testing.  Returns the list of SST filenames.
func writeFakeChainDB(t *testing.T, dataDir string) []string {
	t.Helper()
	dbDir := filepath.Join(dataDir, "chain.db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	sstFiles := []string{"000001.ldb", "000002.ldb", "MANIFEST-000003", "CURRENT"}
	for _, name := range sstFiles {
		if err := os.WriteFile(filepath.Join(dbDir, name), []byte("data:"+name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// WAL file — must be excluded from the archive.
	if err := os.WriteFile(filepath.Join(dbDir, "000004.log"), []byte("wal-data"), 0644); err != nil {
		t.Fatal(err)
	}
	return sstFiles
}

// readTarEntries reads a gzip+tar archive from r and returns the map of
// entry name → content.
func readTarEntries(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gzr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	entries := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		data, _ := io.ReadAll(tr)
		entries[hdr.Name] = string(data)
	}
	return entries
}

func TestChainDBExport_ProducesValidTar(t *testing.T) {
	dir := t.TempDir()
	sstFiles := writeFakeChainDB(t, dir)

	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("Content-Type = %q, want application/x-tar", ct)
	}

	entries := readTarEntries(t, rr.Body)

	// All SST files must be present.
	for _, name := range sstFiles {
		key := "chain.db/" + name
		if _, ok := entries[key]; !ok {
			t.Errorf("missing entry %q in archive", key)
		}
	}
}

func TestChainDBExport_ExcludesWALFiles(t *testing.T) {
	dir := t.TempDir()
	writeFakeChainDB(t, dir)

	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	entries := readTarEntries(t, rr.Body)
	for name := range entries {
		if strings.HasSuffix(name, ".log") {
			t.Errorf("WAL file %q must not appear in the archive", name)
		}
	}
}

func TestChainDBExport_FileContentIsCorrect(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "chain.db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := []byte("leveldb-sst-content")
	if err := os.WriteFile(filepath.Join(dbDir, "000001.ldb"), content, 0644); err != nil {
		t.Fatal(err)
	}

	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	entries := readTarEntries(t, rr.Body)
	got, ok := entries["chain.db/000001.ldb"]
	if !ok {
		t.Fatal("chain.db/000001.ldb not found in archive")
	}
	if got != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestChainDBExport_ValidKeyAccepted(t *testing.T) {
	dir := t.TempDir()
	writeFakeChainDB(t, dir)
	srv := buildExportServer(t, dir, "thekey")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	req.Header.Set("X-API-Key", "thekey")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("valid key: status = %d, want 200", rr.Code)
	}
}

// TestChainDBExport_PathSanitisation verifies that symlinks or paths containing
// ".." do not appear in the archive.  We can't easily create an escaping entry
// via the Walk (the sanitisation happens inside the Walk), but we can verify
// that no entry in a real export starts with "..".
func TestChainDBExport_PathSanitisation(t *testing.T) {
	dir := t.TempDir()
	writeFakeChainDB(t, dir)

	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	entries := readTarEntries(t, rr.Body)
	for name := range entries {
		if strings.HasPrefix(name, "..") || strings.Contains(name, "/../") {
			t.Errorf("archive contains escaping path: %q", name)
		}
	}
}
