package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

type backupExporterFunc func(context.Context, string) (crypto.Hash32, uint64, error)

func (f backupExporterFunc) ExportLogicalBackup(ctx context.Context, dst string) (crypto.Hash32, uint64, error) {
	return f(ctx, dst)
}

func backupHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}},
		Timeout: 5 * time.Second,
	}
}

func TestBackupSocketModeMethodsAndClose(t *testing.T) {
	dataDir := t.TempDir()
	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(context.Context, string) (crypto.Hash32, uint64, error) {
		return crypto.Hash32{}, 0, errors.New("unused")
	}))
	if err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Stat(srv.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := socketInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("socket mode = %#o, want 0600", got)
	}
	client := backupHTTPClient(srv.path)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		resp, err := client.Do(mustRequest(t, method, "http://unix/checkpoint", nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s /checkpoint status = %d, want %d", method, resp.StatusCode, http.StatusMethodNotAllowed)
		}
	}
	resp, err := client.Post("http://unix/not-checkpoint", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown endpoint status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(srv.path); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after Close: %v", err)
	}
}

func TestBackupSocketRecoversSameUserStaleSocket(t *testing.T) {
	dataDir := t.TempDir()
	socketPath := filepath.Join(dataDir, backupSocketName)
	oldListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	stalePath := socketPath + ".stale"
	if err := os.Rename(socketPath, stalePath); err != nil {
		t.Fatal(err)
	}
	if err := oldListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stalePath, socketPath); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("test setup did not leave a stale socket: info=%v err=%v", info, err)
	}

	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(context.Context, string) (crypto.Hash32, uint64, error) {
		return crypto.Hash32{}, 0, errors.New("unused")
	}))
	if err != nil {
		t.Fatalf("start with stale socket: %v", err)
	}
	defer srv.Close()
	if srv.path != socketPath {
		t.Fatalf("socket path = %q, want %q", srv.path, socketPath)
	}
}

func TestBackupSocketRefusesLiveSocket(t *testing.T) {
	dataDir := t.TempDir()
	exporter := backupExporterFunc(func(context.Context, string) (crypto.Hash32, uint64, error) {
		return crypto.Hash32{}, 0, errors.New("unused")
	})
	srv, err := startBackupSocket(dataDir, exporter)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := startBackupSocket(dataDir, exporter); err == nil {
		t.Fatal("starting a second server replaced a live socket")
	}
	resp, err := backupHTTPClient(srv.path).Get("http://unix/checkpoint")
	if err != nil {
		t.Fatalf("original live socket stopped working: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("original socket response = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestBackupSocketCheckpointSuccess(t *testing.T) {
	dataDir := t.TempDir()
	var expectedHash crypto.Hash32
	for i := range expectedHash {
		expectedHash[i] = byte(i + 1)
	}
	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(ctx context.Context, dst string) (crypto.Hash32, uint64, error) {
		if err := ctx.Err(); err != nil {
			return crypto.Hash32{}, 0, err
		}
		if err := os.Mkdir(dst, 0700); err != nil {
			return crypto.Hash32{}, 0, err
		}
		return expectedHash, 42, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	resp, err := backupHTTPClient(srv.path).Post("http://unix/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("checkpoint status = %d: %s", resp.StatusCode, body)
	}
	var result checkpointResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	wantHash := hex.EncodeToString(expectedHash[:])
	if result.TipHeight != 42 || result.TipHash != wantHash {
		t.Fatalf("response tip = %d/%q, want 42/%q", result.TipHeight, result.TipHash, wantHash)
	}
	stage, err := filepath.Abs(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	stagingDir, err := filepath.Abs(filepath.Join(dataDir, ".backup-staging"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(stage) != stagingDir {
		t.Fatalf("stage path %q not directly inside %q", stage, stagingDir)
	}
	if _, err := os.Stat(filepath.Join(stage, "chain.db")); err != nil {
		t.Fatalf("checkpoint database missing: %v", err)
	}
	rawManifest, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpointManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Success || manifest.TipHash != wantHash || manifest.TipHeight != 42 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if strings.Contains(string(rawManifest), "secret") {
		t.Fatal("manifest includes secret data")
	}
}

func TestBackupSocketFailureCleansStage(t *testing.T) {
	dataDir := t.TempDir()
	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(_ context.Context, dst string) (crypto.Hash32, uint64, error) {
		if err := os.Mkdir(dst, 0700); err != nil {
			return crypto.Hash32{}, 0, err
		}
		_ = os.WriteFile(filepath.Join(dst, "partial"), []byte("partial"), 0600)
		return crypto.Hash32{}, 0, errors.New("export failed")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	resp, err := backupHTTPClient(srv.path).Post("http://unix/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failure status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, ".backup-staging"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial backup stage was not cleaned: %v", entries)
	}
}

func TestBackupSocketCancellationCleansStage(t *testing.T) {
	dataDir := t.TempDir()
	entered := make(chan struct{})
	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(ctx context.Context, dst string) (crypto.Hash32, uint64, error) {
		if err := os.Mkdir(dst, 0700); err != nil {
			return crypto.Hash32{}, 0, err
		}
		_ = os.WriteFile(filepath.Join(dst, "partial"), []byte("partial"), 0600)
		close(entered)
		<-ctx.Done()
		return crypto.Hash32{}, 0, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/checkpoint", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = backupHTTPClient(srv.path).Do(req)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("checkpoint did not enter exporter")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled client request did not return")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, readErr := os.ReadDir(filepath.Join(dataDir, ".backup-staging"))
		if readErr == nil && len(entries) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, ".backup-staging"))
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("canceled partial stage was not cleaned: %v", entries)
}

func TestBackupSocketExcludesConcurrentCheckpoint(t *testing.T) {
	dataDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(ctx context.Context, dst string) (crypto.Hash32, uint64, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return crypto.Hash32{}, 0, ctx.Err()
		}
		if err := os.Mkdir(dst, 0700); err != nil {
			return crypto.Hash32{}, 0, err
		}
		return crypto.Hash32{1}, 7, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := backupHTTPClient(srv.path)
	firstDone := make(chan error, 1)
	go func() {
		resp, err := client.Post("http://unix/checkpoint", "", nil)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = errors.New("first checkpoint did not succeed")
			}
		}
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first checkpoint did not enter exporter")
	}
	second, err := client.Post("http://unix/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("concurrent request status = %d, want %d", second.StatusCode, http.StatusServiceUnavailable)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestBackupSocketCloseCancelsAndWaitsForExport(t *testing.T) {
	dataDir := t.TempDir()
	entered := make(chan struct{})
	exportStopped := make(chan struct{})
	srv, err := startBackupSocket(dataDir, backupExporterFunc(func(ctx context.Context, _ string) (crypto.Hash32, uint64, error) {
		close(entered)
		<-ctx.Done()
		close(exportStopped)
		return crypto.Hash32{}, 0, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan struct{})
	go func() {
		resp, _ := backupHTTPClient(srv.path).Post("http://unix/checkpoint", "", nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("export did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- srv.Close() }()
	select {
	case <-exportStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("socket shutdown did not cancel the export")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("socket Close returned before waiting for the export")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request handler did not finish after export cancellation")
	}
}

func mustRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
