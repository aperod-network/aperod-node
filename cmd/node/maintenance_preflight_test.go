package main

import (
	"bytes"
"crypto/sha256"
"encoding/binary"
	"fmt"
"io/fs"
	"os"
	"path/filepath"
"reflect"
	"strings"
	"testing"

"github.com/aperod/aperod/crypto"
"github.com/aperod/aperod/store"
)

func TestMaintenancePreflightWarnsAndRefusesStaleTip(t *testing.T) {
	dataDir := t.TempDir()
	snapshotHeight := uint64(1_135_000)
	snapshot := filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, snapshotHeight))
	if err := os.WriteFile(snapshot, []byte("test"), 0600); err != nil {
		t.Fatalf("write snapshot marker: %v", err)
	}

	var out bytes.Buffer
	err := maintenancePreflight(&out, "/etc/aperod/node.yaml", dataDir, 105_932)
	if err == nil {
		t.Fatal("expected stale database tip to refuse maintenance")
	}
	if !strings.Contains(err.Error(), "MAINTENANCE REFUSED") ||
		!strings.Contains(err.Error(), "appears stale or internally inconsistent") {
		t.Fatalf("unexpected refusal: %v", err)
	}

	text := out.String()
	for _, want := range []string{
		"MAINTENANCE PREFLIGHT",
		"resolved_config: /etc/aperod/node.yaml",
		"absolute_data_dir: " + dataDir,
		"tip_height: 105932",
		"latest_snapshot_height: 1135000",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preflight output missing %q:\n%s", want, text)
		}
	}
}

func TestMaintenancePreflightAllowsMatchingTip(t *testing.T) {
	dataDir := t.TempDir()
	var out bytes.Buffer
	if err := maintenancePreflight(&out, "config/testnet.yaml", dataDir, 42); err != nil {
		t.Fatalf("maintenancePreflight: %v", err)
	}
	if !strings.Contains(out.String(), "tip_height: 42") {
		t.Fatalf("preflight output missing tip:\n%s", out.String())
	}
}

func createMaintenanceTestDB(t *testing.T, dataDir string, tipHeight uint64) {
t.Helper()
db, err := store.Open(filepath.Join(dataDir, "chain.db"))
if err != nil {
t.Fatalf("open fixture db: %v", err)
}
sum := sha256.Sum256([]byte("maintenance-test-tip"))
var hash crypto.Hash32
copy(hash[:], sum[:])
block := &store.StoredBlock{Height: tipHeight, Hash: hash}
if err := db.PutBlock(hash, block); err != nil {
db.Close()
t.Fatalf("write fixture block: %v", err)
}
if err := db.PutTip(hash, tipHeight); err != nil {
db.Close()
t.Fatalf("write fixture tip: %v", err)
}
if err := db.Close(); err != nil {
t.Fatalf("close fixture db: %v", err)
}
}

func TestRepairDBRefusesInvalidTipMetadataBeforeRecovery(t *testing.T) {
testCases := []struct {
name  string
setup func(t *testing.T, db *store.DB)
}{
{
name: "empty database",
setup: func(t *testing.T, db *store.DB) {},
},
{
name: "missing tip hash",
setup: func(t *testing.T, db *store.DB) {
var height [8]byte
binary.LittleEndian.PutUint64(height[:], 42)
if err := db.PutMeta("tip/height", height[:]); err != nil {
t.Fatalf("write tip height: %v", err)
}
},
},
{
name: "missing tip height",
setup: func(t *testing.T, db *store.DB) {
hash := sha256.Sum256([]byte("half-tip"))
if err := db.PutMeta("tip/hash", hash[:]); err != nil {
t.Fatalf("write tip hash: %v", err)
}
},
},
{
name: "malformed tip metadata",
setup: func(t *testing.T, db *store.DB) {
if err := db.PutMeta("tip/hash", []byte{1, 2, 3}); err != nil {
t.Fatalf("write malformed tip hash: %v", err)
}
if err := db.PutMeta("tip/height", []byte{4, 5}); err != nil {
t.Fatalf("write malformed tip height: %v", err)
}
},
},
}

for _, tc := range testCases {
t.Run(tc.name, func(t *testing.T) {
dataDir := t.TempDir()
db, err := store.Open(filepath.Join(dataDir, "chain.db"))
if err != nil {
t.Fatalf("open fixture db: %v", err)
}
tc.setup(t, db)
if err := db.Close(); err != nil {
t.Fatalf("close fixture db: %v", err)
}
before := maintenanceDBState(t, dataDir)

cfgPath := filepath.Join(t.TempDir(), "node.yaml")
cfgText := fmt.Sprintf("network: testnet\ndata_dir: %q\n", dataDir)
if err := os.WriteFile(cfgPath, []byte(cfgText), 0600); err != nil {
t.Fatalf("write config: %v", err)
}
t.Setenv("APEROD_DATA_DIR", "")
oldArgs := os.Args
os.Args = []string{"aperod-node", "--repair-db", "--config", cfgPath}
t.Cleanup(func() { os.Args = oldArgs })

err = run()
if err == nil || !strings.Contains(err.Error(), "MAINTENANCE REFUSED") {
t.Fatalf("expected invalid-tip refusal, got %v", err)
}
if after := maintenanceDBState(t, dataDir); !reflect.DeepEqual(after, before) {
t.Fatalf("repair-db changed invalid chain.db before refusing:\nbefore=%v\nafter=%v", before, after)
}
})
}
}

func maintenanceDBState(t *testing.T, dataDir string) map[string][32]byte {
t.Helper()
state := make(map[string][32]byte)
root := filepath.Join(dataDir, "chain.db")
err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
if walkErr != nil {
return walkErr
}
if entry.IsDir() {
return nil
}
content, err := os.ReadFile(path)
if err != nil {
return err
}
relative, err := filepath.Rel(root, path)
if err != nil {
return err
}
state[relative] = sha256.Sum256(content)
return nil
})
if err != nil {
t.Fatalf("capture chain.db state: %v", err)
}
return state
}

func TestCompactDBRefusesStaleTargetBeforeCompaction(t *testing.T) {
dataDir := t.TempDir()
createMaintenanceTestDB(t, dataDir, 100)
snapshot := filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, 20_000))
if err := os.WriteFile(snapshot, []byte("test"), 0600); err != nil {
t.Fatalf("write snapshot marker: %v", err)
}
before := maintenanceDBState(t, dataDir)

oldArgs := os.Args
os.Args = []string{"aperod-node", "--compact-db", "--data-dir", dataDir}
t.Cleanup(func() { os.Args = oldArgs })

err := runCompactDB()
if err == nil || !strings.Contains(err.Error(), "MAINTENANCE REFUSED") {
t.Fatalf("expected stale-target refusal, got %v", err)
}
if after := maintenanceDBState(t, dataDir); !reflect.DeepEqual(after, before) {
t.Fatalf("compact-db changed chain.db before refusing:\nbefore=%v\nafter=%v", before, after)
}
}

func TestRepairHeightIndexRefusesStaleTargetBeforeRepair(t *testing.T) {
dataDir := t.TempDir()
createMaintenanceTestDB(t, dataDir, 100)
snapshot := filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, 20_000))
if err := os.WriteFile(snapshot, []byte("test"), 0600); err != nil {
t.Fatalf("write snapshot marker: %v", err)
}
before := maintenanceDBState(t, dataDir)

oldArgs := os.Args
os.Args = []string{"aperod-node", "--repair-height-index", "--data-dir", dataDir}
t.Cleanup(func() { os.Args = oldArgs })

err := runRepairHeightIndex()
if err == nil || !strings.Contains(err.Error(), "MAINTENANCE REFUSED") {
t.Fatalf("expected stale-target refusal, got %v", err)
}
if after := maintenanceDBState(t, dataDir); !reflect.DeepEqual(after, before) {
t.Fatalf("repair-height-index changed chain.db before refusing:\nbefore=%v\nafter=%v", before, after)
}
}

func TestRepairDBRefusesStaleTargetBeforeRecovery(t *testing.T) {
dataDir := t.TempDir()
createMaintenanceTestDB(t, dataDir, 100)
snapshot := filepath.Join(dataDir, fmt.Sprintf("snapshot-v%d-%d.json.gz", snapVersion, 20_000))
if err := os.WriteFile(snapshot, []byte("test"), 0600); err != nil {
t.Fatalf("write snapshot marker: %v", err)
}
before := maintenanceDBState(t, dataDir)

cfgPath := filepath.Join(t.TempDir(), "node.yaml")
cfgText := fmt.Sprintf("network: testnet\ndata_dir: %q\n", dataDir)
if err := os.WriteFile(cfgPath, []byte(cfgText), 0600); err != nil {
t.Fatalf("write config: %v", err)
}
t.Setenv("APEROD_DATA_DIR", "")
oldArgs := os.Args
os.Args = []string{"aperod-node", "--repair-db", "--config", cfgPath}
t.Cleanup(func() { os.Args = oldArgs })

err := run()
if err == nil || !strings.Contains(err.Error(), "MAINTENANCE REFUSED") {
t.Fatalf("expected stale-target refusal, got %v", err)
}
if after := maintenanceDBState(t, dataDir); !reflect.DeepEqual(after, before) {
t.Fatalf("repair-db changed chain.db before refusing:\nbefore=%v\nafter=%v", before, after)
}
}
