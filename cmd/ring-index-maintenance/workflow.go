package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aperod/aperod/store"
)

type operationJournal struct {
	Schema         int      `json:"schema"`
	ApprovalID     string   `json:"approval_id"`
	PolicySHA256   string   `json:"policy_sha256"`
	DBPath         string   `json:"db_path"`
	Backup         string   `json:"backup"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	Phase          string   `json:"phase"`
	Resume         []string `json:"resume"`
}

func control(p maintenancePolicy, kind string) string {
	path, err := store.RingMaintenanceControlPath(p.DBPath, kind)
	if err != nil {
		panic(err)
	} // validated canonical scope, not request input
	return path
}

func readJournal(p maintenancePolicy) (operationJournal, error) {
	j, err := readJournalHeader(p)
	if err != nil {
		return j, err
	}
	if filepath.Dir(j.Backup) != p.BackupRoot || !digestRE.MatchString(j.ManifestSHA256) {
		return j, fmt.Errorf("unverified coherent backup")
	}
	if err := protectedPath(j.Backup, 0); err != nil {
		return j, err
	}
	got, err := fileDigest(filepath.Join(j.Backup, "manifest.jsonl"))
	if err != nil {
		return j, err
	}
	if got != j.ManifestSHA256 {
		return j, fmt.Errorf("coherent backup manifest changed")
	}
	if err := verifyBackup(j.Backup); err != nil {
		return j, err
	}
	return j, nil
}

func readJournalHeader(p maintenancePolicy) (operationJournal, error) {
	var j operationJournal
	path := control(p, "journal.json")
	if err := protectedPath(path, 0); err != nil {
		return j, err
	}
	f, err := os.Open(path)
	if err != nil {
		return j, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&j); err != nil {
		return j, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return j, fmt.Errorf("journal trailing content")
	}
	digest, err := fileDigest(policyPath)
	if err != nil {
		return j, err
	}
	if j.Schema != 1 || j.ApprovalID != p.ApprovalID || j.DBPath != p.DBPath || j.PolicySHA256 != digest {
		return j, fmt.Errorf("operation approval/scope changed; independent recovery review required")
	}
	return j, nil
}

func coherentBackup(p maintenancePolicy, l *store.RingMaintenanceLease, register func(string) error) (string, string, error) {
	if err := protectedPath(p.BackupRoot, 0); err != nil {
		return "", "", err
	}
	cmd, fd, cancel, err := workerCommand(p, l, "backup-lock")
	if err != nil {
		return "", "", err
	}
	defer fd.Close()
	defer cancel()
	input, err := cmd.StdinPipe()
	if err != nil {
		return "", "", err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	done := false
	defer func() {
		if !done {
			cancel()
			input.Close()
			cmd.Wait()
		}
	}()
	var original tipReceipt
	decoder := json.NewDecoder(io.LimitReader(output, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&original); err != nil {
		return "", "", err
	}
	backup, err := os.MkdirTemp(p.BackupRoot, "ring-maintenance-")
	if err != nil {
		return "", "", err
	}
	if err := register(backup); err != nil {
		return backup, "", err
	}
	manifest, err := os.OpenFile(filepath.Join(backup, "manifest.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return backup, "", err
	}
	closed := false
	defer func() {
		if !closed {
			manifest.Close()
		}
	}()
	encoder := json.NewEncoder(manifest)
	if err := encoder.Encode(map[string]any{"schema": 1, "db_path": p.DBPath, "tip": original, "approval_id": p.ApprovalID}); err != nil {
		return backup, "", err
	}
	if err := copyTree(p.DataDir, filepath.Join(backup, "data"), encoder); err != nil {
		return backup, "", err
	}
	// Preserve exact matching config/binary, but never install/restore either
	// automatically. A restored old writer needs this procedure again.
	for _, pair := range [][2]string{{p.ConfigPath, "node.yaml"}, {p.NodeBinary, "node.bin"}} {
		in, err := os.Open(pair[0])
		if err != nil {
			return backup, "", err
		}
		out, err := os.OpenFile(filepath.Join(backup, pair[1]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return backup, "", err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(out, h), in)
		in.Close()
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil {
			return backup, "", copyErr
		}
		if syncErr != nil {
			return backup, "", syncErr
		}
		if closeErr != nil {
			return backup, "", closeErr
		}
		if err := encoder.Encode(backupEntry{Tree: "meta", Path: pair[1], SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n, Mode: 0600}); err != nil {
			return backup, "", err
		}
	}
	copyDB := filepath.Join(backup, "data", mustRelative(p.DataDir, p.DBPath))
	db, err := store.OpenReadOnly(copyDB)
	if err != nil {
		return backup, "", err
	}
	copied, tipErr := tipMetadata(db)
	closeErr := db.Close()
	if tipErr != nil {
		return backup, "", tipErr
	}
	if closeErr != nil {
		return backup, "", closeErr
	}
	if copied != original {
		return backup, "", fmt.Errorf("copy tip differs from quiesced source")
	}
	if err := encoder.Encode(map[string]any{"complete": true, "tip": copied}); err != nil {
		return backup, "", err
	}
	if err := manifest.Sync(); err != nil {
		return backup, "", err
	}
	if err := manifest.Close(); err != nil {
		return backup, "", err
	}
	closed = true
	if err := syncDirectory(backup); err != nil {
		return backup, "", err
	}
	if err := syncDirectory(p.BackupRoot); err != nil {
		return backup, "", err
	}
	if err := verifyBackup(backup); err != nil {
		return backup, "", err
	}
	if _, err := io.WriteString(input, "release\n"); err != nil {
		return backup, "", err
	}
	input.Close()
	if err := cmd.Wait(); err != nil {
		return backup, "", err
	}
	done = true
	digest, err := fileDigest(filepath.Join(backup, "manifest.jsonl"))
	return backup, digest, err
}

func mustRelative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		panic(err)
	}
	return rel
}

func prepare(p maintenancePolicy, l *store.RingMaintenanceLease, s serviceController) error {
	var j operationJournal
	var partial string
	if _, err := os.Lstat(control(p, "journal.json")); os.IsNotExist(err) {
		if err := s.witnessDB(p); err != nil {
			return err
		}
		resume, err := s.activeBefore(p)
		if err != nil {
			return err
		}
		digest, err := fileDigest(policyPath)
		if err != nil {
			return err
		}
		j = operationJournal{Schema: 1, ApprovalID: p.ApprovalID, PolicySHA256: digest, DBPath: p.DBPath, Phase: "quiesced", Resume: resume}
		if err := writeControl(control(p, "journal.json"), j, true); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		j, err = readJournalHeader(p)
		if err != nil {
			return err
		}
		if j.Phase != "quiesced" && j.Phase != "copying" {
			return fmt.Errorf("prepared/attempted operation cannot be replayed")
		}
		partial = j.Backup
	}
	if _, err := os.Lstat(control(p, "active")); !os.IsNotExist(err) {
		return fmt.Errorf("unexpected prior active fence requires reviewed recovery")
	}
	if _, err := os.Lstat(control(p, "quiesced")); os.IsNotExist(err) {
		if err := writeControl(control(p, "quiesced"), map[string]string{"approval_id": p.ApprovalID}, true); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := s.stop(allUnits(p)); err != nil {
		return err
	}
	backup, manifest, err := coherentBackup(p, l, func(path string) error {
		j.Phase = "copying"
		j.Backup = path
		return writeControl(control(p, "journal.json"), j, false)
	})
	if err != nil {
		return err
	}
	j.Backup = backup
	j.ManifestSHA256 = manifest
	j.Phase = "backup-complete"
	if err := writeControl(control(p, "journal.json"), j, false); err != nil {
		return err
	}
	// Only this operation's abandoned pre-write copy is disposable, and only
	// after a newer full, re-read verified copy exists. Never prune completed
	// rollback evidence, by age or because disk space is tight.
	if partial != "" && partial != backup {
		if filepath.Dir(partial) != p.BackupRoot {
			return fmt.Errorf("partial backup scope invalid")
		}
		if err := protectedPath(partial, 0); err != nil {
			return err
		}
		if err := os.RemoveAll(partial); err != nil {
			return err
		}
		if err := syncDirectory(p.BackupRoot); err != nil {
			return err
		}
	}
	if err := l.Fence(); err != nil {
		return err
	}
	if err := invalidateWorker(p, l); err != nil {
		return err
	}
	j.Phase = "prepared"
	return writeControl(control(p, "journal.json"), j, false)
}

func enterLegacy(p maintenancePolicy, l *store.RingMaintenanceLease, s serviceController) error {
	j, err := readJournal(p)
	if err != nil {
		return err
	}
	if j.Phase != "prepared" {
		return fmt.Errorf("legacy operation already attempted/not durably prepared")
	}
	if err := s.stop(allUnits(p)); err != nil {
		return err
	}
	if err := invalidateWorker(p, l); err != nil {
		return err
	}
	// Fence the attempt BEFORE starting; a crash must never silently replay
	// an import/repair/write operation with possible monetary side effects.
	j.Phase = "legacy-attempted"
	if err := writeControl(control(p, "journal.json"), j, false); err != nil {
		return err
	}
	if err := writeControl(control(p, "legacy-authorized"), map[string]string{"approval_id": p.ApprovalID}, true); err != nil {
		return err
	}
	return s.start([]unitPolicy{p.Legacy})
}

func finalize(p maintenancePolicy, l *store.RingMaintenanceLease, s serviceController) error {
	j, err := readJournal(p)
	if err != nil {
		return err
	}
	if j.Phase == "completed" {
		return nil
	}
	if j.Phase != "prepared" && j.Phase != "legacy-attempted" && j.Phase != "backup-complete" && j.Phase != "audited" {
		return fmt.Errorf("unknown/incomplete recovery phase")
	}
	if _, err := os.Lstat(control(p, "quiesced")); os.IsNotExist(err) {
		if err := writeControl(control(p, "quiesced"), map[string]string{"approval_id": p.ApprovalID}, true); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := removeControl(control(p, "legacy-authorized")); err != nil {
		return err
	}
	if err := s.stop(allUnits(p)); err != nil {
		return err
	}
	if err := approvedIndexAware(p, p.NodeBinary); err != nil {
		return err
	}
	// backup-complete may mean the controller crashed just before fence
	// publication; no legacy action was authorized in that state.
	if err := l.Fence(); err != nil && !os.IsExist(err) {
		return err
	}
	receipt, err := rebuildWorker(p, l)
	if err != nil {
		return err
	}
	if err := writeControl(control(p, "receipt.json"), map[string]any{"verified": receipt, "backup": j.Backup, "time": time.Now().UTC(), "policy_sha256": j.PolicySHA256}, false); err != nil {
		return err
	}
	j.Phase = "audited"
	if err := writeControl(control(p, "journal.json"), j, false); err != nil {
		return err
	}
	if err := l.CommitVerifiedWorker(receipt); err != nil {
		return err
	}
	// External quiescence remains until the verified commit was fsynced.
	if err := removeControl(control(p, "quiesced")); err != nil {
		return err
	}
	if err := s.start(previouslyActive(p.Writers, j.Resume)); err != nil {
		return err
	}
	if err := s.start(previouslyActive(p.Clients, j.Resume)); err != nil {
		return err
	}
	j.Phase = "completed"
	return writeControl(control(p, "journal.json"), j, false)
}
