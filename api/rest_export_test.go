package api_test

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type exportDeadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
}

func (w *exportDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}

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

func TestChainDBExport_RequiresConfiguredStore(t *testing.T) {
	dir := t.TempDir()
	srv := buildExportServer(t, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
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

func writeExportFixture(t *testing.T, dataDir string) (*store.DB, crypto.Hash32) {
	t.Helper()
	db, err := store.Open(filepath.Join(dataDir, "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	tipHash := crypto.HashBytes([]byte("api logical export test genesis"))
	if err := db.PutRawBlock(tipHash, 0, []byte("synthetic genesis body")); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.PutTip(tipHash, 0); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db, tipHash
}

func extractCheckpointArchive(t *testing.T, r io.Reader, destination string) {
	t.Helper()
	gzr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if filepath.IsAbs(hdr.Name) || strings.Contains(hdr.Name, "..") {
			t.Fatalf("archive contains unsafe path %q", hdr.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(hdr.Name))
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			t.Fatalf("unsupported checkpoint archive member type %d at %q", hdr.Typeflag, hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(file, tr, hdr.Size)
		closeErr := file.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}

func TestChainDBExport_ProducesPointInTimeLogicalCheckpoint(t *testing.T) {
	dir := t.TempDir()
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil {
		t.Fatal(err)
	}
	freeBytes := disk.Bavail * uint64(disk.Bsize)
	if freeBytes <= 5*1024*1024*1024+64*1024*1024 {
		t.Skipf("logical checkpoint needs more than 5 GiB plus working headroom; currently %d bytes free", freeBytes)
	}
	db, tipHash := writeExportFixture(t, dir)
	defer db.Close()

	srv := buildExportServer(t, dir, "")
	srv.SetStore(db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chaindb/export", nil)
	rr := &exportDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	srv.ServeHTTP(rr, req)

	if time.Until(rr.writeDeadline) < 90*time.Minute {
		t.Fatalf("export write deadline was not extended sufficiently: %v", rr.writeDeadline)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if rr.Header().Get("X-ChainDB-Tip-Height") != "0" {
		t.Fatalf("tip height header = %q, want 0", rr.Header().Get("X-ChainDB-Tip-Height"))
	}
	if rr.Header().Get("X-ChainDB-Tip-Hash") != fmt.Sprintf("%x", tipHash[:]) {
		t.Fatalf("tip hash header = %q, want %x", rr.Header().Get("X-ChainDB-Tip-Hash"), tipHash)
	}
	extracted := t.TempDir()
	extractCheckpointArchive(t, rr.Body, extracted)
	gotHash, gotHeight, err := store.ReadTipOnly(filepath.Join(extracted, "chain.db"))
	if err != nil {
		t.Fatalf("read exported logical checkpoint: %v", err)
	}
	if gotHash != tipHash || gotHeight != 0 {
		t.Fatalf("reopened exported tip = %x/%d, want %x/0", gotHash, gotHeight, tipHash)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".backup-staging"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary checkpoint stage was not cleaned: %v", entries)
	}
}
