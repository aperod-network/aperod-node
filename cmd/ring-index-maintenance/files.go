package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func syncDirectory(path string) error {
	// #nosec G304 -- path is a staging directory generated beneath the approved backup root.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

func writeControl(path string, value any, exclusive bool) (retErr error) {
	if exclusive {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("existing operation/control must be recovered, not overwritten")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".control-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary control file: %w", err))
		}
	}()
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func removeControl(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

type backupEntry struct {
	Tree   string `json:"tree"`
	Kind   string `json:"kind,omitempty"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
}

// Rooted source access cannot escape the approved data tree via raced symlinks.
// Services/writers and the shared native DB lock must already be quiescent.
// Destination is a new root-private directory, never a live/overlapping tree.
func copyTree(source, destination string, manifest *json.Encoder) (retErr error) {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close source root: %w", err))
		}
	}()
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	var visit func(string) error
	visit = func(relative string) (retErr error) {
		dir, err := root.Open(relative)
		if err != nil {
			return err
		}
		defer func() {
			if err := dir.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close source directory: %w", err))
			}
		}()
		info, err := dir.Stat()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("directory ownership unavailable")
		}
		if err := manifest.Encode(backupEntry{Tree: "data", Kind: "directory", Path: relative, Mode: uint32(info.Mode().Perm()), UID: stat.Uid, GID: stat.Gid}); err != nil {
			return err
		}
		for {
			entries, err := dir.ReadDir(256)
			if err != nil && err != io.EOF {
				return err
			}
			for _, entry := range entries {
				name := filepath.Join(relative, entry.Name())
				info, err := root.Lstat(name)
				if err != nil {
					return err
				}
				target := filepath.Join(destination, name)
				if info.IsDir() {
					if err := os.Mkdir(target, 0700); err != nil {
						return err
					}
					if err := visit(name); err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("backup refuses links/devices/special files")
				}
				in, err := root.Open(name)
				if err != nil {
					return err
				}
				opened, err := in.Stat()
				if err != nil {
					return errors.Join(err, in.Close())
				}
				if !os.SameFile(info, opened) {
					return errors.Join(fmt.Errorf("source file identity changed"), in.Close())
				}
				// #nosec G304 -- target is beneath a new backup root and its relative name comes from the opened source directory.
				out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					return errors.Join(err, in.Close())
				}
				hash := sha256.New()
				n, copyErr := io.Copy(io.MultiWriter(out, hash), in)
				after, statErr := in.Stat()
				inputCloseErr := in.Close()
				syncErr := out.Sync()
				closeErr := out.Close()
				if err := errors.Join(copyErr, statErr, inputCloseErr, syncErr, closeErr); err != nil {
					return err
				}
				if n != info.Size() || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
					return fmt.Errorf("source changed during coherent copy")
				}
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					return fmt.Errorf("backup ownership unavailable")
				}
				if err := manifest.Encode(backupEntry{Tree: "data", Path: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: n, Mode: uint32(info.Mode().Perm()), UID: stat.Uid, GID: stat.Gid}); err != nil {
					return err
				}
			}
			if err == io.EOF {
				break
			}
		}
		return syncDirectory(filepath.Join(destination, relative))
	}
	if err := visit("."); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

// Verify every copied file by re-reading the private destination, not only its
// manifest or tip. This is byte-copy integrity, not a historical-chain proof.
func verifyBackup(backup string) (retErr error) {
	root, err := os.OpenRoot(backup)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close backup root: %w", err))
		}
	}()
	f, err := root.Open("manifest.jsonl")
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close backup manifest: %w", err))
		}
	}()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 16384)
	header, complete := false, false
	for scanner.Scan() {
		var row struct {
			backupEntry
			Schema     int        `json:"schema"`
			DBPath     string     `json:"db_path"`
			Tip        tipReceipt `json:"tip"`
			ApprovalID string     `json:"approval_id"`
			Complete   bool       `json:"complete"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return err
		}
		if complete {
			return fmt.Errorf("backup data after completion")
		}
		if !header {
			if row.Schema != 1 || row.DBPath == "" || !digestRE.MatchString(row.Tip.Hash) {
				return fmt.Errorf("backup header incomplete")
			}
			header = true
			continue
		}
		if row.Complete {
			complete = true
			continue
		}
		if row.Tree != "data" && row.Tree != "meta" {
			return fmt.Errorf("backup tree invalid")
		}
		if filepath.IsAbs(row.Path) || filepath.Clean(row.Path) != row.Path || row.Path == ".." || strings.HasPrefix(row.Path, "../") {
			return fmt.Errorf("backup entry invalid")
		}
		path := row.Path
		if row.Tree == "data" {
			path = filepath.Join("data", row.Path)
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if row.Kind == "directory" {
			if !info.IsDir() {
				return fmt.Errorf("backup directory differs")
			}
			continue
		}
		if row.Kind != "" || !digestRE.MatchString(row.SHA256) || row.Path == "." {
			return fmt.Errorf("backup entry type/hash invalid")
		}
		if !info.Mode().IsRegular() || info.Size() != row.Bytes {
			return fmt.Errorf("backup file type/size differs")
		}
		in, err := root.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, in)
		closeErr := in.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != row.SHA256 {
			return fmt.Errorf("backup file checksum differs")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !header || !complete {
		return fmt.Errorf("backup has no durable completion")
	}
	return nil
}
