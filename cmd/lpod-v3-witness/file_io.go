package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func read0600RegularFile(path string) (result []byte, readCloseErr error) {
	if path == "" {
		return nil, fmt.Errorf("validator key path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("key path must be a regular, non-symlink file")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("key file permissions must be exactly 0600")
	}
	if info.Size() != 32 && info.Size() != 64 {
		return nil, fmt.Errorf("key file must contain exactly 32 or 64 raw bytes")
	}
	// #nosec G304 -- Operator-selected offline input; O_NOFOLLOW and subsequent metadata checks reject unsafe key-file forms.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var raw []byte
	defer func() {
		readCloseErr = errors.Join(readCloseErr, file.Close())
		if readCloseErr != nil {
			crypto.ZeroBytes(raw)
			result = nil
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened key file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("key file changed during validation")
	}
	raw, err = io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 && len(raw) != 64 {
		return nil, fmt.Errorf("key file must contain exactly 32 or 64 raw bytes")
	}
	return raw, nil
}

func writeExclusive0600(path string, migration *store.LPoDMigration) (err error) {
	// #nosec G304 -- Operator-selected witness output, exclusively created with owner-only permissions.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create output exclusively: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			_ = os.Remove(path)
			err = fmt.Errorf("close output witness: %w", closeErr)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("set witness output mode 0600: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(migration); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write witness: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("sync witness: %w", err)
	}
	return nil
}
