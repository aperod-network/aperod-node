package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	chaincrypto "github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestAllowedDatabasePath(t *testing.T) {
	for _, id := range []string{
		"checkpoint-1189583511",
		"checkpoint-1235802059",
		"checkpoint-147415538",
	} {
		path := stagingRoot + "/" + id + "/chain.db"
		if !allowedDatabasePath(path) {
			t.Errorf("approved checkpoint path rejected: %s", path)
		}
	}
	for _, path := range []string{
		"/opt/aperod/data/testnet/chain.db",
		stagingRoot + "/checkpoint-unlisted/chain.db",
		stagingRoot + "/checkpoint-1189583511/manifest.json",
		stagingRoot + "/checkpoint-1189583511/chain.db/child",
		"/tmp/checkpoint-1189583511/chain.db",
	} {
		if allowedDatabasePath(path) {
			t.Errorf("unapproved path accepted: %s", path)
		}
	}
}

func TestSyntheticPinnedArchiveRestore(t *testing.T) {
	sandbox := t.TempDir()
	sandbox = filepath.Clean(sandbox)
	if sandbox == stagingRoot || strings.HasPrefix(sandbox, stagingRoot+string(os.PathSeparator)) {
		t.Fatalf("test fixture unexpectedly uses the production staging root: %s", sandbox)
	}

	const checkpointID = "checkpoint-1189583511"
	const fixtureIdentity = "Aperod isolated restore rehearsal"
	height := uint64(42)
	hash := chaincrypto.Hash32{}
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	hashHex := fmt.Sprintf("%x", hash[:])

	sourceRoot := filepath.Join(sandbox, "source")
	sourceCheckpoint := filepath.Join(sourceRoot, checkpointID)
	sourceDB := filepath.Join(sourceCheckpoint, "chain.db")
	if err := os.MkdirAll(sourceCheckpoint, 0700); err != nil {
		t.Fatal(err)
	}
	createSyntheticCheckpointDB(t, sourceDB, height, hash)
	manifest, err := json.Marshal(map[string]any{
		"success": true, "tip_height": height, "tip_hash": hashHex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCheckpoint, "manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	sourceFiles := syntheticFileInventory(t, sourceCheckpoint)

	sourceReport, err := inspectCheckpoint(sourceDB, sourceRoot, uint32(os.Getuid()))
	if err != nil {
		t.Fatalf("verify synthetic source checkpoint: %v", err)
	}
	if sourceReport.Height != height || sourceReport.Hash != hashHex || !sourceReport.HeightIndexMatches {
		t.Fatalf("unexpected source tip report: %+v", sourceReport)
	}
	if _, err := inspectCheckpoint(sourceDB, sourceRoot, uint32(os.Getuid())+1); err == nil {
		t.Fatal("checkpoint control files with the wrong owner were accepted")
	}

	archiveBytes := tarSyntheticCheckpoint(t, sourceCheckpoint)
	archiveKeyHome := filepath.Join(sandbox, "archive-key-home")
	if err := os.Mkdir(archiveKeyHome, 0700); err != nil {
		t.Fatal(err)
	}
	runFixtureGPG(t, archiveKeyHome, nil,
		"--pinentry-mode", "loopback", "--passphrase", "",
		"--quick-generate-key", fixtureIdentity, "rsa2048", "encr", "0")
	fingerprint := fixtureGPGFingerprint(t, archiveKeyHome)
	ciphertextPath := filepath.Join(sandbox, "synthetic-checkpoint.tar.gpg")
	runFixtureGPG(t, archiveKeyHome, bytes.NewReader(archiveBytes),
		"--trust-model", "always", "--compress-algo", "none",
		"--output", ciphertextPath, "--recipient", fingerprint, "--encrypt")
	ciphertext, err := os.ReadFile(ciphertextPath)
	if err != nil {
		t.Fatal(err)
	}
	secretKey := runFixtureGPG(t, archiveKeyHome, nil, "--export-secret-keys", fingerprint)
	restoreKeyHome := filepath.Join(sandbox, "offhost-restore-key-home")
	if err := os.Mkdir(restoreKeyHome, 0700); err != nil {
		t.Fatal(err)
	}
	runFixtureGPG(t, restoreKeyHome, bytes.NewReader(secretKey), "--import")
	if err := os.RemoveAll(archiveKeyHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archiveKeyHome); !os.IsNotExist(err) {
		t.Fatalf("archive-side key home still exists after off-host key-copy setup: %v", err)
	}
	ciphertextHash := sha256.Sum256(ciphertext)
	fileName := "retained-checkpoints/rehearsal/" + hashHex + ".tar.gpg"
	const pinnedVersionID = "synthetic-b2-version-002"
	versions := map[string]fixtureB2Version{
		"synthetic-b2-version-001": {
			fileID: "synthetic-b2-version-001", fileName: fileName, data: []byte("older version"),
		},
		pinnedVersionID: {
			fileID: pinnedVersionID, fileName: fileName, data: ciphertext,
		},
		"synthetic-b2-version-003": {
			fileID: "synthetic-b2-version-003", fileName: fileName, data: []byte("newer version"),
		},
	}
	pinnedBytes, err := selectFixtureB2Version(versions, fixtureB2Receipt{
		fileID: pinnedVersionID, fileName: fileName,
		ciphertextSHA256: hex.EncodeToString(ciphertextHash[:]),
	})
	if err != nil {
		t.Fatalf("select exact synthetic B2 version: %v", err)
	}

	restoredTar := runFixtureGPG(t, restoreKeyHome, bytes.NewReader(pinnedBytes), "--decrypt")
	restoreParent := filepath.Join(sandbox, "fresh-restore")
	if err := os.MkdirAll(restoreParent, 0700); err != nil {
		t.Fatal(err)
	}
	restoreRoot := filepath.Join(restoreParent, checkpointID)
	if err := extractSyntheticTar(restoredTar, restoreRoot); err != nil {
		t.Fatalf("extract synthetic archive: %v", err)
	}

	restoredManifest, err := os.ReadFile(filepath.Join(restoreRoot, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restoredMetadata struct {
		Success   bool   `json:"success"`
		TipHeight uint64 `json:"tip_height"`
		TipHash   string `json:"tip_hash"`
	}
	if err := json.Unmarshal(restoredManifest, &restoredMetadata); err != nil {
		t.Fatal(err)
	}
	if !restoredMetadata.Success || restoredMetadata.TipHeight != height || restoredMetadata.TipHash != hashHex {
		t.Fatalf("restored manifest does not match the synthetic tip: %+v", restoredMetadata)
	}
	restoredFiles := syntheticFileInventory(t, restoreRoot)
	if !reflect.DeepEqual(restoredFiles, sourceFiles) {
		t.Fatalf("restored member inventory differs from source:\nsource:   %v\nrestored: %v",
			sourceFiles, restoredFiles)
	}

	restoredDB := filepath.Join(restoreRoot, "chain.db")
	report, err := inspectCheckpoint(restoredDB, filepath.Dir(restoreRoot), uint32(os.Getuid()))
	if err != nil {
		t.Fatalf("closed-staging tip verification failed after isolated restore: %v", err)
	}
	if report.Height != restoredMetadata.TipHeight || report.Hash != restoredMetadata.TipHash ||
		!report.HeightIndexMatches {
		t.Fatalf("restored database tip differs from its manifest: %+v", report)
	}
	if afterVerify := syntheticFileInventory(t, restoreRoot); !reflect.DeepEqual(afterVerify, restoredFiles) {
		t.Fatalf("read-only tip verification changed restored files:\nbefore: %v\nafter:  %v",
			restoredFiles, afterVerify)
	}
	if sourceAfter := syntheticFileInventory(t, sourceCheckpoint); !reflect.DeepEqual(sourceAfter, sourceFiles) {
		t.Fatalf("synthetic source changed during restore rehearsal:\nbefore: %v\nafter:  %v",
			sourceFiles, sourceAfter)
	}
}

type fixtureB2Version struct {
	fileID   string
	fileName string
	data     []byte
}

type fixtureB2Receipt struct {
	fileID           string
	fileName         string
	ciphertextSHA256 string
}

func selectFixtureB2Version(versions map[string]fixtureB2Version, receipt fixtureB2Receipt) ([]byte, error) {
	version, ok := versions[receipt.fileID]
	if !ok || version.fileID != receipt.fileID || version.fileName != receipt.fileName {
		return nil, fmt.Errorf("exact pinned object version identity does not match")
	}
	digest := sha256.Sum256(version.data)
	if hex.EncodeToString(digest[:]) != receipt.ciphertextSHA256 {
		return nil, fmt.Errorf("exact pinned object version ciphertext digest does not match")
	}
	return version.data, nil
}

func createSyntheticCheckpointDB(t *testing.T, path string, height uint64, hash chaincrypto.Hash32) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutBlock(hash, &store.StoredBlock{Height: height, Hash: hash}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var heightBytes [8]byte
	binary.LittleEndian.PutUint64(heightBytes[:], height)
	if err := db.PutMetaSync("tip/hash", hash[:]); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.PutMetaSync("tip/height", heightBytes[:]); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func syntheticFileInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular fixture file: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = fmt.Sprintf("%s:%04o",
			hex.EncodeToString(digest[:]), info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func tarSyntheticCheckpoint(t *testing.T, source string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected synthetic archive member type: %s", path)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name, err = filepath.Rel(source, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(header.Name)
		if info.IsDir() {
			header.Name += "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func runFixtureGPG(t *testing.T, home string, input io.Reader, args ...string) []byte {
	t.Helper()
	commandArgs := []string{
		"--homedir", home, "--batch", "--yes", "--no-tty",
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command("gpg", commandArgs...)
	if input != nil {
		cmd.Stdin = input
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture-only GPG operation failed: %v: %s", err, stderr.String())
	}
	return stdout.Bytes()
}

func fixtureGPGFingerprint(t *testing.T, home string) string {
	t.Helper()
	output := runFixtureGPG(t, home, nil, "--with-colons", "--list-secret-keys")
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			return fields[9]
		}
	}
	t.Fatal("temporary GPG home has no fixture secret-key fingerprint")
	return ""
}

type syntheticTarMember struct {
	name     string
	typeflag byte
	linkname string
	data     []byte
}

type parsedSyntheticTarMember struct {
	name string
	dir  bool
	mode os.FileMode
	data []byte
}

func extractSyntheticTar(archiveBytes []byte, destination string) error {
	parent := filepath.Dir(destination)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("restore parent: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("restore parent must be a real directory")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != filepath.Clean(parent) {
		return fmt.Errorf("restore parent must not traverse symlinks")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("restore destination already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect restore destination: %w", err)
	}

	reader := tar.NewReader(bytes.NewReader(archiveBytes))
	seen := make(map[string]parsedSyntheticTarMember)
	members := make([]parsedSyntheticTarMember, 0)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive header: %w", err)
		}
		isDir := header.Typeflag == tar.TypeDir
		cleanName, err := cleanSyntheticTarName(header.Name, isDir)
		if err != nil {
			return fmt.Errorf("unsafe archive member %q: %w", header.Name, err)
		}
		if _, exists := seen[cleanName]; exists {
			return fmt.Errorf("duplicate archive member %q", header.Name)
		}
		member := parsedSyntheticTarMember{
			name: cleanName, dir: isDir, mode: os.FileMode(header.Mode) & 0777,
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return fmt.Errorf("directory archive member %q has a payload", header.Name)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > int64(len(archiveBytes)) {
				return fmt.Errorf("archive member %q has invalid size %d", header.Name, header.Size)
			}
			member.data, err = io.ReadAll(reader)
			if err != nil {
				return fmt.Errorf("read archive member %q: %w", header.Name, err)
			}
			if int64(len(member.data)) != header.Size {
				return fmt.Errorf("archive member %q has %d bytes, expected %d",
					header.Name, len(member.data), header.Size)
			}
		default:
			return fmt.Errorf("unsupported archive member type %d for %q", header.Typeflag, header.Name)
		}
		seen[cleanName] = member
		members = append(members, member)
	}
	for _, required := range []string{
		"manifest.json", filepath.Join("chain.db", "CURRENT"), filepath.Join("chain.db", "LOCK"),
	} {
		member, exists := seen[required]
		if !exists || member.dir {
			return fmt.Errorf("synthetic restore omitted required regular file %q", required)
		}
	}
	for _, member := range members {
		if member.dir {
			continue
		}
		prefix := member.name + string(os.PathSeparator)
		for otherName := range seen {
			if strings.HasPrefix(otherName, prefix) {
				return fmt.Errorf("archive file %q conflicts with child member %q", member.name, otherName)
			}
		}
		for parentName := filepath.Dir(member.name); parentName != "."; parentName = filepath.Dir(parentName) {
			if parent, exists := seen[parentName]; exists && !parent.dir {
				return fmt.Errorf("archive member %q is beneath non-directory %q", member.name, parentName)
			}
		}
	}

	stage, err := os.MkdirTemp(parent, ".checkpoint-restore-")
	if err != nil {
		return fmt.Errorf("create restore staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	directories := make([]parsedSyntheticTarMember, 0)
	for _, member := range members {
		target := filepath.Join(stage, member.name)
		if member.dir {
			if err := os.MkdirAll(target, 0700); err != nil {
				return fmt.Errorf("create archive directory %q: %w", member.name, err)
			}
			directories = append(directories, member)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return fmt.Errorf("create parent for archive member %q: %w", member.name, err)
		}
		if err := os.WriteFile(target, member.data, 0600); err != nil {
			return fmt.Errorf("write archive member %q: %w", member.name, err)
		}
		if err := os.Chmod(target, member.mode); err != nil {
			return fmt.Errorf("set mode for archive member %q: %w", member.name, err)
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(directories[i].name, string(os.PathSeparator)) >
			strings.Count(directories[j].name, string(os.PathSeparator))
	})
	for _, directory := range directories {
		if err := os.Chmod(filepath.Join(stage, directory.name), directory.mode); err != nil {
			return fmt.Errorf("set mode for archive directory %q: %w", directory.name, err)
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		return fmt.Errorf("publish restored checkpoint directory: %w", err)
	}
	return nil
}

func cleanSyntheticTarName(name string, isDir bool) (string, error) {
	if name == "" || strings.Contains(name, `\`) {
		return "", fmt.Errorf("empty names and backslash separators are not supported")
	}
	if isDir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || strings.HasPrefix(name, "/") || filepath.IsAbs(name) ||
		filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("non-canonical path component")
		}
	}
	cleanName := filepath.Clean(filepath.FromSlash(name))
	if cleanName == "." || cleanName == ".." || filepath.IsAbs(cleanName) ||
		strings.HasPrefix(cleanName, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes extraction root")
	}
	return cleanName, nil
}

func syntheticTarBytes(t *testing.T, members ...syntheticTarMember) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, member := range members {
		mode := int64(0600)
		if member.typeflag == tar.TypeDir {
			mode = 0700
		}
		header := &tar.Header{
			Name: member.name, Typeflag: member.typeflag, Linkname: member.linkname,
			Mode: mode, Size: int64(len(member.data)),
		}
		if member.typeflag == tar.TypeDir || member.typeflag == tar.TypeSymlink ||
			member.typeflag == tar.TypeLink {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write synthetic tar header %q: %v", member.name, err)
		}
		if member.typeflag == tar.TypeReg || member.typeflag == tar.TypeRegA {
			if _, err := writer.Write(member.data); err != nil {
				t.Fatalf("write synthetic tar member %q: %v", member.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close synthetic tar: %v", err)
	}
	return output.Bytes()
}

func requiredSyntheticTarMembers() []syntheticTarMember {
	return []syntheticTarMember{
		{name: "manifest.json", typeflag: tar.TypeReg, data: []byte(`{"success":true}`)},
		{name: "chain.db/", typeflag: tar.TypeDir},
		{name: "chain.db/CURRENT", typeflag: tar.TypeReg, data: []byte("MANIFEST-000001\n")},
		{name: "chain.db/LOCK", typeflag: tar.TypeReg},
	}
}

func TestExtractSyntheticTarRejectsUnsafeMembersWithoutWrites(t *testing.T) {
	tests := []struct {
		name      string
		malformed func(root string) ([]syntheticTarMember, string)
	}{
		{
			name: "absolute path",
			malformed: func(root string) ([]syntheticTarMember, string) {
				target := filepath.Join(root, "absolute-escape")
				return []syntheticTarMember{
					{name: target, typeflag: tar.TypeReg, data: []byte("outside")},
				}, target
			},
		},
		{
			name: "parent traversal",
			malformed: func(root string) ([]syntheticTarMember, string) {
				return []syntheticTarMember{
					{name: "../traversal-escape", typeflag: tar.TypeReg, data: []byte("outside")},
				}, filepath.Join(root, "traversal-escape")
			},
		},
		{
			name: "duplicate names",
			malformed: func(string) ([]syntheticTarMember, string) {
				return []syntheticTarMember{
					{name: "duplicate.txt", typeflag: tar.TypeReg, data: []byte("first")},
					{name: "duplicate.txt", typeflag: tar.TypeReg, data: []byte("second")},
				}, ""
			},
		},
		{
			name: "symbolic link",
			malformed: func(root string) ([]syntheticTarMember, string) {
				return []syntheticTarMember{
					{name: "link", typeflag: tar.TypeSymlink, linkname: "../symlink-escape"},
				}, filepath.Join(root, "symlink-escape")
			},
		},
		{
			name: "hard link",
			malformed: func(root string) ([]syntheticTarMember, string) {
				return []syntheticTarMember{
					{name: "link", typeflag: tar.TypeLink, linkname: "../hardlink-escape"},
				}, filepath.Join(root, "hardlink-escape")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			destination := filepath.Join(root, "restore")
			malformed, outside := test.malformed(root)
			if outside != "" {
				if _, err := os.Lstat(outside); !os.IsNotExist(err) {
					t.Fatalf("outside target unexpectedly exists before test: %v", err)
				}
			}

			members := requiredSyntheticTarMembers()
			members = append(members,
				syntheticTarMember{name: "partial-before-invalid.txt", typeflag: tar.TypeReg, data: []byte("must not be written")},
			)
			members = append(members, malformed...)

			err := extractSyntheticTar(syntheticTarBytes(t, members...), destination)
			if err == nil {
				t.Fatal("malformed archive was accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("rejected archive created or partially restored destination: %v", err)
			}
			if outside != "" {
				if _, err := os.Lstat(outside); !os.IsNotExist(err) {
					t.Fatalf("rejected archive wrote outside restore root: %v", err)
				}
			}
			remaining, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(remaining) != 0 {
				t.Fatalf("rejected archive left staging files outside destination: %v", remaining)
			}
		})
	}
}

func TestAllowedDatabasePathUnderTemporaryRoot(t *testing.T) {
	root := t.TempDir()
	checkpoint := filepath.Join(root, "checkpoint-1189583511", "chain.db")
	if !allowedDatabasePathUnder(checkpoint, root) {
		t.Fatalf("approved fixture path rejected: %s", checkpoint)
	}
	if allowedDatabasePathUnder(checkpoint, stagingRoot) {
		t.Fatalf("fixture path unexpectedly accepted under production root")
	}
}

func TestSelectFixtureB2VersionRequiresExactIDNameAndDigest(t *testing.T) {
	data := []byte("fixture ciphertext")
	hash := sha256.Sum256(data)
	receipt := fixtureB2Receipt{
		fileID: "fixture-version-7", fileName: "retained-checkpoints/fixture.tar.gpg",
		ciphertextSHA256: hex.EncodeToString(hash[:]),
	}
	versions := map[string]fixtureB2Version{
		receipt.fileID: {fileID: receipt.fileID, fileName: receipt.fileName, data: data},
		"fixture-version-8": {
			fileID: "fixture-version-8", fileName: receipt.fileName, data: []byte("other version"),
		},
	}
	selected, err := selectFixtureB2Version(versions, receipt)
	if err != nil || !bytes.Equal(selected, data) {
		t.Fatalf("exact pinned fixture version rejected: %v", err)
	}
	for name, altered := range map[string]fixtureB2Receipt{
		"version id": {fileID: "fixture-version-8", fileName: receipt.fileName,
			ciphertextSHA256: receipt.ciphertextSHA256},
		"object name": {fileID: receipt.fileID, fileName: "unexpected.tar.gpg",
			ciphertextSHA256: receipt.ciphertextSHA256},
		"ciphertext digest": {fileID: receipt.fileID, fileName: receipt.fileName,
			ciphertextSHA256: strings.Repeat("0", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := selectFixtureB2Version(versions, altered); err == nil {
				t.Fatalf("accepted mismatched %s", name)
			}
		})
	}
}
