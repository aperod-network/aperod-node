package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// RingMaintenanceStateDirectory must be independently installed root-owned,
// non-writable by services, and searchable by readers. It is outside imported
// DB generations, so an old backup cannot restore a stale readiness marker over
// the maintenance fence. This is not installed as part of a node update.
const RingMaintenanceStateDirectory = "/var/lib/aperod-ring-maintenance"

func protectedRingMaintenanceRoot(root string, owner uint32) error {
	for path := root; ; path = filepath.Dir(path) {
		dir, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, ok := dir.Sys().(*syscall.Stat_t)
		if !ok || !dir.IsDir() || st.Uid != owner || dir.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("ring maintenance: protected non-symlink state directory required")
		}
		if root == RingMaintenanceStateDirectory && dir.Mode().Perm()&0001 == 0 {
			return fmt.Errorf("ring maintenance: state directory must remain searchable by readers")
		}
		// Production's fixed root has no service-owned ancestor. Private test
		// directories are capability fixtures, not configurable runtime roots.
		if root != RingMaintenanceStateDirectory || path == "/" {
			return nil
		}
	}
}

func ringMaintenanceIdentity(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if errors.Is(err, os.ErrNotExist) {
		// Preserve normal Open's ability to create nested directories.
		parent, suffix := filepath.Dir(absolute), filepath.Base(absolute)
		for {
			resolved, parentErr := filepath.EvalSymlinks(parent)
			if parentErr == nil {
				real, err = filepath.Join(resolved, suffix), nil
				break
			}
			if !errors.Is(parentErr, os.ErrNotExist) || parent == "/" {
				return "", parentErr
			}
			suffix = filepath.Join(filepath.Base(parent), suffix)
			parent = filepath.Dir(parent)
		}
	}
	if err != nil {
		return "", err
	}
	return real, nil
}

func ringMaintenanceStatePath(path, root string) (string, error) {
	identity, err := ringMaintenanceIdentity(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(root, hex.EncodeToString(digest[:])+".active"), nil
}

// RingMaintenanceControlPath returns fixed-root control identities for the
// reviewed coordinator and its independently approved systemd drop-ins.
func RingMaintenanceControlPath(path, kind string) (string, error) {
	state, err := ringMaintenanceStatePath(path, RingMaintenanceStateDirectory)
	if err != nil {
		return "", err
	}
	switch kind {
	case "active", "quiesced", "legacy-authorized", "journal.json", "receipt.json":
		return state[:len(state)-len("active")] + kind, nil
	default:
		return "", fmt.Errorf("ring maintenance: unsupported control kind")
	}
}

func checkRingMaintenanceFenceAt(path, root string) error {
	// Check lexical scope first. Redirecting an imported path to a different
	// symlink target must not hide the original externally fenced identity.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(absolute))
	if err := checkRingScopeFiles(filepath.Join(root, hex.EncodeToString(digest[:])+".active")); err != nil {
		return err
	}
	state, err := ringMaintenanceStatePath(path, root)
	if err != nil {
		return err
	}
	return checkRingScopeFiles(state)
}

func checkRingScopeFiles(state string) error {
	if err := checkRingStateFile(state); err != nil {
		return err
	}
	return checkRingStateFile(state[:len(state)-len("active")] + "quiesced")
}

func checkRingStateFile(state string) error {
	_, err := os.Lstat(state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ring maintenance: cannot check fence: %w", err)
	}
	return fmt.Errorf("ring maintenance: unfinished offline operation; writer/sampling access refused")
}

// CheckRingMaintenanceFence is a read-only startup guard. Any marker (including
// a corrupt file or symlink) blocks access. Permissions/errors fail closed.
func CheckRingMaintenanceFence(path string) error {
	if _, err := os.Lstat(RingMaintenanceStateDirectory); err == nil {
		if err := protectedRingMaintenanceRoot(RingMaintenanceStateDirectory, 0); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return checkRingMaintenanceFenceAt(path, RingMaintenanceStateDirectory)
}

// RingMaintenanceLease serializes approved maintenance controllers. Closing it
// never removes the fence. Only an audited rebuild can commit it away.
// Approval, service masking/quiescence and coherent full backup remain
// mandatory responsibilities of the independently approved coordinator.
type RingMaintenanceLease struct {
	path       string
	state      string
	lock       *os.File
	verified   bool
	generation string
}

func acquireRingMaintenanceAt(path, root string, owner uint32) (*RingMaintenanceLease, error) {
	identity, err := ringMaintenanceIdentity(path)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil || identity != absolute || identity == "/" {
		return nil, fmt.Errorf("ring maintenance: canonical non-symlink database path required")
	}
	info, err := os.Lstat(identity)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ring maintenance: existing database directory required")
	}
	if err := protectedRingMaintenanceRoot(root, owner); err != nil {
		return nil, err
	}
	state, err := ringMaintenanceStatePath(identity, root)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(state+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), state+".lock")
	fail := func(err error) (*RingMaintenanceLease, error) { file.Close(); return nil, err }
	li, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	ls, ok := li.Sys().(*syscall.Stat_t)
	if !ok || !li.Mode().IsRegular() || ls.Uid != owner || li.Mode().Perm()&0077 != 0 || ls.Nlink != 1 {
		return fail(fmt.Errorf("ring maintenance: unsafe lease file"))
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("ring maintenance: another controller owns lease: %w", err))
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail(err)
	}
	return &RingMaintenanceLease{path: identity, state: state, lock: file, generation: hex.EncodeToString(nonce[:])}, nil
}

// RingMaintenanceWorkerReceipt is accepted only by the trusted root coordinator
// after its exact approved unprivileged worker exited successfully. The nonce
// prevents cross-controller receipt reuse; the inherited root-owned FD is the
// worker's authority. A nonce/JSON supplied by a wallet is never authority.
type RingMaintenanceWorkerReceipt struct {
	Schema     int                   `json:"schema"`
	DBPath     string                `json:"db_path"`
	Generation string                `json:"generation"`
	Inventory  RingSamplingInventory `json:"inventory"`
}

func (l *RingMaintenanceLease) WorkerGeneration() string {
	if l == nil {
		return ""
	}
	return l.generation
}

func (l *RingMaintenanceLease) WorkerFile() (*os.File, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	fd, err := syscall.Dup(int(l.lock.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "ring-maintenance-lease"), nil
}

func borrowRingMaintenanceAt(path, root, generation string, file *os.File, owner uint32) (*RingMaintenanceLease, error) {
	if file == nil {
		return nil, fmt.Errorf("ring maintenance: inherited authority FD required")
	}
	if err := protectedRingMaintenanceRoot(root, owner); err != nil {
		return nil, err
	}
	identity, err := ringMaintenanceIdentity(path)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil || identity != absolute {
		return nil, fmt.Errorf("ring maintenance: noncanonical worker scope")
	}
	state, err := ringMaintenanceStatePath(identity, root)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil {
		return nil, err
	}
	expected, err := os.Lstat(state + ".lock")
	if err != nil {
		return nil, err
	}
	st, ok := actual.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != owner || !actual.Mode().IsRegular() || actual.Mode().Perm()&0077 != 0 || st.Nlink != 1 || !os.SameFile(actual, expected) {
		return nil, fmt.Errorf("ring maintenance: forged/mismatched worker authority")
	}
	if _, err := hex.DecodeString(generation); err != nil || len(generation) != 32 {
		return nil, fmt.Errorf("ring maintenance: invalid worker generation")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	return &RingMaintenanceLease{path: identity, state: state, lock: file, generation: generation}, nil
}

// BorrowRingMaintenanceLease only accepts the inherited capability for the
// fixed protected state directory. It does not accept a runtime state-root
// override. Workers run as the data owner, never as root.
func BorrowRingMaintenanceLease(path, generation string, file *os.File) (*RingMaintenanceLease, error) {
	return borrowRingMaintenanceAt(path, RingMaintenanceStateDirectory, generation, file, 0)
}

// OpenReadOnly allows an approved unprivileged worker to hold the native
// LevelDB shared LOCK while its root coordinator copies the quiesced full data
// directory. It neither repairs nor initializes a missing database.
func (l *RingMaintenanceLease) OpenReadOnly() (*DB, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(l.path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ring maintenance: database directory required")
	}
	for _, name := range []string{"LOCK", "CURRENT"} {
		file, err := os.Lstat(filepath.Join(l.path, name))
		if err != nil {
			return nil, err
		}
		if !file.Mode().IsRegular() {
			return nil, fmt.Errorf("ring maintenance: existing regular database control files required")
		}
	}
	raw, err := leveldb.OpenFile(l.path, &opt.Options{ReadOnly: true, ErrorIfMissing: true})
	if err != nil {
		return nil, err
	}
	return &DB{db: raw, path: l.path}, nil
}

func (l *RingMaintenanceLease) commitVerifiedWorkerAt(receipt RingMaintenanceWorkerReceipt, coordinatorUID int) error {
	if os.Geteuid() != coordinatorUID {
		return fmt.Errorf("ring maintenance: root coordinator required")
	}
	if err := l.check(); err != nil {
		return err
	}
	i := receipt.Inventory
	digest, err := hex.DecodeString(i.CanonicalSHA256)
	if receipt.Schema != 1 || receipt.DBPath != l.path || receipt.Generation != l.generation || err != nil || len(digest) != 32 || i.Eligible > i.Canonical || i.Locked != i.Canonical-i.Eligible {
		return fmt.Errorf("ring maintenance: mismatched/incomplete verified worker receipt")
	}
	l.verified = true
	return l.Commit()
}

// CommitVerifiedWorker is a trusted coordinator boundary, not a receipt
// verification service. Caller must have just waited for its separately pinned
// worker's success and strictly decoded that worker's sole stdout receipt.
func (l *RingMaintenanceLease) CommitVerifiedWorker(receipt RingMaintenanceWorkerReceipt) error {
	return l.commitVerifiedWorkerAt(receipt, 0)
}

// AcquireRingMaintenance requires an independently prepared trusted directory.
// Production callers must authenticate their separate tool approval first.
func AcquireRingMaintenance(path string) (*RingMaintenanceLease, error) {
	return acquireRingMaintenanceAt(path, RingMaintenanceStateDirectory, 0)
}

func (l *RingMaintenanceLease) check() error {
	if l == nil || l.lock == nil {
		return fmt.Errorf("ring maintenance: no active lease")
	}
	identity, err := ringMaintenanceIdentity(l.path)
	if err != nil || identity != l.path {
		return fmt.Errorf("ring maintenance: database path identity changed: %w", err)
	}
	return nil
}

func syncRingStateDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Fence publishes a durable external barrier BEFORE permitting a legacy writer,
// restore/import or repair. An already-present marker is a recovery case; it is
// not silently overwritten or treated as a completed operation.
func (l *RingMaintenanceLease) Fence() error {
	if err := l.check(); err != nil {
		return err
	}
	fd, err := syscall.Open(l.state, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("ring maintenance: fence already present or cannot create: %w", err)
	}
	f := os.NewFile(uintptr(fd), l.state)
	_, writeErr := f.WriteString(l.path + "\n")
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr, syncRingStateDirectory(l.state))
}

func (l *RingMaintenanceLease) existingFencedDB() (*DB, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(l.state)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("ring maintenance: invalid durable fence")
	}
	// OpenFile(ErrorIfMissing) still creates its filesystem directory before
	// detecting an absent CURRENT. Never initialize a removed import target.
	fi, err = os.Lstat(l.path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("ring maintenance: database directory required")
	}
	db, err := leveldb.OpenFile(l.path, &opt.Options{ErrorIfMissing: true, BlockCacheCapacity: 32 * opt.MiB, WriteBuffer: 16 * opt.MiB, CompactionTableSize: 4 * opt.MiB})
	if err != nil {
		return nil, err
	}
	return &DB{db: db, path: l.path}, nil
}

// Invalidate fsync-deletes readiness while preserving canonical/spent records.
// The exclusive LevelDB lock also rejects a writer that was not quiesced.
func (l *RingMaintenanceLease) Invalidate() error {
	if err := l.check(); err != nil {
		return err
	}
	l.verified = false
	db, err := l.existingFencedDB()
	if err != nil {
		return err
	}
	return errors.Join(db.InvalidateRingSamplingIndex(), db.Close())
}

// RebuildAndVerify distrusts even a ready marker imported from an old backup.
// No marker or receipt supplied by the importer skips this full streaming audit.
func (l *RingMaintenanceLease) RebuildAndVerify() (RingSamplingInventory, error) {
	var out RingSamplingInventory
	if err := l.check(); err != nil {
		return out, err
	}
	l.verified = false
	db, err := l.existingFencedDB()
	if err != nil {
		return out, err
	}
	err = db.InvalidateRingSamplingIndex()
	if err == nil {
		err = db.verifyLegacyRingBackfillPreservesCanonical()
	}
	if err == nil {
		err = db.EnsureRingMemberIndex()
	}
	if err == nil {
		out, err = db.VerifyRingSamplingIndex()
	}
	if err != nil {
		err = errors.Join(err, db.InvalidateRingSamplingIndex())
	}
	err = errors.Join(err, db.Close())
	if err == nil {
		l.verified = true
	}
	return out, err
}

// Commit is legal only after verified rebuild and clean DB close. The caller
// must keep ALL client/writer services masked through this commit; masking is
// not delegated to a database API. A failed commit leaves services quiesced.
func (l *RingMaintenanceLease) Commit() error {
	if err := l.check(); err != nil {
		return err
	}
	if !l.verified {
		return fmt.Errorf("ring maintenance: verified rebuild required")
	}
	if err := os.Remove(l.state); err != nil {
		return err
	}
	if err := syncRingStateDirectory(l.state); err != nil {
		l.verified = false
		return errors.Join(err, l.Fence())
	}
	l.verified = false
	return nil
}

func (l *RingMaintenanceLease) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	err := l.lock.Close()
	l.lock = nil
	l.verified = false
	return err
}
