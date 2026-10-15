package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/aperod/aperod/store"
	"golang.org/x/sys/unix"
)

type tipReceipt struct {
	Hash   string `json:"hash"`
	Height uint64 `json:"height"`
}

func tipMetadata(db *store.DB) (tipReceipt, error) {
	hash, err := db.GetMeta("tip/hash")
	if err != nil {
		return tipReceipt{}, err
	}
	height, err := db.GetMeta("tip/height")
	if err != nil {
		return tipReceipt{}, err
	}
	if len(hash) != 32 || len(height) != 8 {
		return tipReceipt{}, fmt.Errorf("complete canonical tip metadata required")
	}
	zero := true
	for _, b := range hash {
		if b != 0 {
			zero = false
		}
	}
	if zero {
		return tipReceipt{}, fmt.Errorf("zero tip hash refused")
	}
	return tipReceipt{hex.EncodeToString(hash), binary.LittleEndian.Uint64(height)}, nil
}

func worker(mode, path, generation string) error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("database worker must not run as root")
	}
	// Do not expose inherited capabilities/worker memory to unrelated
	// processes of the data-owner UID via ptrace or /proc/<pid>/fd.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	p, err := loadApprovedPolicyMode(false)
	if err != nil {
		return err
	}
	if path != p.DBPath {
		return fmt.Errorf("worker scope differs from independent approval")
	}
	lease, err := store.BorrowRingMaintenanceLease(path, generation, os.NewFile(3, "inherited-lease"))
	if err != nil {
		return err
	}
	defer lease.Close()
	switch mode {
	case "backup-lock":
		db, err := lease.OpenReadOnly()
		if err != nil {
			return err
		}
		defer db.Close()
		tip, err := tipMetadata(db)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(os.Stdout).Encode(tip); err != nil {
			return err
		}
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 16)).ReadString('\n')
		if err != nil || line != "release\n" {
			return fmt.Errorf("backup lock controller disconnected")
		}
		return nil
	case "invalidate":
		return lease.Invalidate()
	case "rebuild":
		inventory, err := lease.RebuildAndVerify()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(store.RingMaintenanceWorkerReceipt{Schema: 1, DBPath: path, Generation: generation, Inventory: inventory})
	default:
		return fmt.Errorf("unsupported worker operation")
	}
}

func workerCommand(p maintenancePolicy, l *store.RingMaintenanceLease, mode string) (*exec.Cmd, *os.File, context.CancelFunc, error) {
	// Recheck the installed executable immediately before exec, not merely its
	// name; approval of a source tree does not authorize these root tool bytes.
	digest, err := fileDigest(toolPath)
	if err != nil || digest != p.ToolSHA256 {
		return nil, nil, nil, fmt.Errorf("worker tool changed")
	}
	account, err := user.Lookup("aperod")
	if err != nil {
		return nil, nil, nil, err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 {
		return nil, nil, nil, fmt.Errorf("non-root data owner required")
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, nil, nil, err
	}
	fd, err := l.WorkerFile()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	cmd := exec.CommandContext(ctx, toolPath, "--worker", mode, "--db", p.DBPath, "--generation", l.WorkerGeneration())
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "GOMEMLIMIT=256MiB"}
	cmd.ExtraFiles = []*os.File{fd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, Cloneflags: syscall.CLONE_NEWNET, Pdeathsig: syscall.SIGKILL}
	return cmd, fd, cancel, nil
}

func invalidateWorker(p maintenancePolicy, l *store.RingMaintenanceLease) error {
	cmd, fd, cancel, err := workerCommand(p, l, "invalidate")
	if err != nil {
		return err
	}
	defer fd.Close()
	defer cancel()
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func rebuildWorker(p maintenancePolicy, l *store.RingMaintenanceLease) (store.RingMaintenanceWorkerReceipt, error) {
	var receipt store.RingMaintenanceWorkerReceipt
	cmd, fd, cancel, err := workerCommand(p, l, "rebuild")
	if err != nil {
		return receipt, err
	}
	defer fd.Close()
	defer cancel()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return receipt, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return receipt, err
	}
	decoder := json.NewDecoder(io.LimitReader(out, 8193))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&receipt)
	var extra any
	if decodeErr == nil {
		if err := decoder.Decode(&extra); err != io.EOF {
			decodeErr = fmt.Errorf("unexpected worker stdout")
		}
	}
	if decodeErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if decodeErr != nil {
		return receipt, decodeErr
	}
	return receipt, waitErr
}
