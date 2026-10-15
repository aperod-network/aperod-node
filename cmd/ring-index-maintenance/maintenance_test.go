package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPolicy() maintenancePolicy {
	h := strings.Repeat("a", 64)
	unit := func(name string) unitPolicy {
		return unitPolicy{Name: name, Files: map[string]string{"/etc/systemd/system/" + name: h}}
	}
	return maintenancePolicy{Schema: 1, ApprovalID: "synthetic-only", ToolSHA256: h, Operation: "repair", DataDir: "/opt/synthetic/data", DBPath: "/opt/synthetic/data/chain.db", BackupRoot: "/opt/synthetic/backup", ConfigPath: "/etc/synthetic/node.yaml", ConfigSHA256: h, NodeBinary: "/usr/local/bin/synthetic-node", IndexAwareBinaries: []string{h}, Clients: []unitPolicy{unit("client.service")}, Writers: []unitPolicy{unit("writer.service")}, Legacy: unit("offline.service"), LegacyBinary: "/usr/local/bin/synthetic-legacy", LegacySHA256: h}
}

func TestPolicyStrictIndependentApprovals(t *testing.T) {
	p := testPolicy()
	body, _ := json.Marshal(p)
	if _, err := strictPolicy(strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*maintenancePolicy){
		"no-tool-approval":   func(p *maintenancePolicy) { p.ToolSHA256 = "" },
		"unknown-binary":     func(p *maintenancePolicy) { p.IndexAwareBinaries = nil },
		"backup-overlap":     func(p *maintenancePolicy) { p.BackupRoot = p.DataDir + "/backups" },
		"scope-outside-data": func(p *maintenancePolicy) { p.DBPath = "/other/chain.db" },
		"unknown-operation":  func(p *maintenancePolicy) { p.Operation = "execute" },
		"duplicate-unit":     func(p *maintenancePolicy) { p.Legacy = p.Writers[0] },
		"wildcard-unit":      func(p *maintenancePolicy) { p.Clients[0].Name = "*.service" },
		"absent-unit-pin":    func(p *maintenancePolicy) { p.Legacy.Files = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := testPolicy()
			change(&p)
			b, _ := json.Marshal(p)
			if _, err := strictPolicy(strings.NewReader(string(b))); err == nil {
				t.Fatal("unsafe policy accepted")
			}
		})
	}
	for _, text := range []string{string(body) + `{}`, strings.Replace(string(body), `"schema":1`, `"schema":1,"override":true`, 1)} {
		if _, err := strictPolicy(strings.NewReader(text)); err == nil {
			t.Fatal("trailing/unknown policy accepted")
		}
	}
}

func syntheticBackup(t *testing.T) (string, string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	backup := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(filepath.Join(source, "chain.db"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "chain.db", "synthetic.sst"), []byte("synthetic-only immutable content"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(backup, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(f)
	encoder.Encode(map[string]any{"schema": 1, "db_path": source + "/chain.db", "tip": tipReceipt{Hash: strings.Repeat("a", 64), Height: 7}, "approval_id": "synthetic-only"})
	if err := copyTree(source, filepath.Join(backup, "data"), encoder); err != nil {
		t.Fatal(err)
	}
	encoder.Encode(map[string]any{"complete": true})
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return source, backup
}

func TestCoherentCopyChecksEveryFileAndCompleteness(t *testing.T) {
	_, backup := syntheticBackup(t)
	if err := verifyBackup(backup); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(backup, "data", "chain.db", "synthetic.sst")
	if err := os.WriteFile(path, []byte("synthetic-only immutable contenX"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(backup); err == nil {
		t.Fatal("corrupted copy accepted")
	}
	_, backup = syntheticBackup(t)
	manifest := filepath.Join(backup, "manifest.jsonl")
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), `{"complete":true}`, "", 1))
	os.WriteFile(manifest, body, 0600)
	if err := verifyBackup(backup); err == nil {
		t.Fatal("partial backup accepted")
	}
}

func TestCopyRefusesLinksAndSpecialFiles(t *testing.T) {
	source := t.TempDir()
	backup := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := copyTree(source, backup, json.NewEncoder(f)); err == nil {
		t.Fatal("source symlink accepted")
	}
}

func TestCgroupQuiescenceIncludesDescendants(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "cgroup.procs"), nil, 0600)
	os.Mkdir(filepath.Join(root, "child"), 0700)
	os.WriteFile(filepath.Join(root, "child", "cgroup.procs"), []byte("123\n"), 0600)
	if err := emptyCgroup(root); err == nil {
		t.Fatal("descendant writer ignored")
	}
	os.WriteFile(filepath.Join(root, "child", "cgroup.procs"), nil, 0600)
	if err := emptyCgroup(root); err != nil {
		t.Fatal(err)
	}
}

func TestResumeDoesNotStartInactiveWriterOrLegacy(t *testing.T) {
	p := testPolicy()
	p.Writers = append(p.Writers, unitPolicy{Name: "backup.timer"})
	got := previouslyActive(p.Writers, []string{"writer.service", "offline.service"})
	if len(got) != 1 || got[0].Name != "writer.service" {
		t.Fatal("inactive/legacy unit selected")
	}
}

func TestControlFileSurvivesReopenAndExclusiveRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := writeControl(path, map[string]string{"phase": "prepared"}, true); err != nil {
		t.Fatal(err)
	}
	if err := writeControl(path, map[string]string{"phase": "legacy-attempted"}, true); err == nil {
		t.Fatal("exclusive retry overwrote journal")
	}
	if err := writeControl(path, map[string]string{"phase": "legacy-attempted"}, false); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "legacy-attempted") {
		t.Fatal("durable journal unreadable")
	}
	if err := removeControl(path); err != nil {
		t.Fatal(err)
	}
}
