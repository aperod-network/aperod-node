// Package deploy_test contains CI guards for the node deployment scripts.
//
// This file tests that the Makefile's atomic binary swap (build to
// aperod-node.new, then mv -f to aperod-node) does not corrupt a running
// process or produce an invalid binary even when make build-node is invoked
// concurrently with an already-executing aperod-node process.
//
// The Linux kernel keeps an open inode reference to the executable image
// of a running process.  A rename(2) call (which mv -f uses) atomically
// replaces the directory entry without touching that inode, so the old
// process continues executing from the old image while the new binary
// appears at the well-known path.  This test verifies both sides:
//
//  1. The "old process" side: a stub process launched from build/aperod-node
//     survives the concurrent make build-node without exiting unexpectedly
//     (crash, SIGSEGV, SIGBUS, etc.).
//
//  2. The "new binary" side: after make build-node completes, the file at
//     build/aperod-node has valid ELF magic bytes (0x7f 'E' 'L' 'F').
//
// Liveness detection
// ──────────────────
// The check uses a goroutine that calls stubProc.Wait() and reports to a
// channel.  Signal(0) is intentionally NOT used: on Linux a crashed child
// becomes a zombie until Wait() is called, and signalling a zombie with
// signal 0 succeeds — the very false-green the test must avoid.
//
// # Skip conditions
//
//   - Running on Windows or macOS (the atomic-swap guarantee is Linux-specific).
//   - make not found in PATH.
//   - The Makefile is not reachable from the test's working directory (deploy/).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestAtomicBinarySwap -v -timeout 180s
package deploy_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// elfMagic is the four-byte signature that every ELF binary starts with.
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// TestAtomicBinarySwap exercises the Makefile's build-node target while a
// process backed by the current build/aperod-node binary is running, then
// asserts:
//
//  1. The running process did not exit unexpectedly (crash, fatal signal).
//  2. The file at build/aperod-node after the build is a valid ELF binary.
func TestAtomicBinarySwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("atomic mv -f swap is a POSIX/Linux guarantee; skipping on Windows")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("mv on macOS is not guaranteed atomic across all FS types; skipping")
	}

	// Locate make.
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not found in PATH; skipping TestAtomicBinarySwap")
	}

	// The test lives in blockchain/deploy/; the Makefile is one level up.
	deployDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine deploy directory: %v", err)
	}
	blockchainDir := filepath.Dir(deployDir)
	makefilePath := filepath.Join(blockchainDir, "Makefile")
	if _, err := os.Stat(makefilePath); os.IsNotExist(err) {
		t.Skipf("Makefile not found at %s; skipping TestAtomicBinarySwap", makefilePath)
	}

	buildDir := filepath.Join(blockchainDir, "build")
	nodeBin := filepath.Join(buildDir, "aperod-node")

	// ── Step 1: build the long-running stub into build/aperod-node ────────────
	//
	// We compile testdata/stub/main.go (a tiny program that sleeps until
	// signalled) with CGO_ENABLED=0 so it is fully static and runnable from
	// any path without dynamic-linker issues.  Using a purpose-built stub
	// avoids the NixOS dynamic-linker path problem that arises when copying
	// system binaries to a new location.
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("cannot create build dir: %v", err)
	}

	t.Log("compiling stub binary into build/aperod-node …")
	stubBuildCmd := exec.Command(
		"go", "build",
		"-ldflags=-s -w",
		"-o", nodeBin,
		"./testdata/stub",
	)
	stubBuildCmd.Dir = deployDir
	stubBuildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := stubBuildCmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot compile stub: %v\n%s", err, string(out))
	}
	t.Logf("stub compiled → %s", nodeBin)

	// ── Step 2: start the stub process ───────────────────────────────────────
	stubProc := exec.Command(nodeBin)
	if err := stubProc.Start(); err != nil {
		t.Fatalf("cannot start stub process from %s: %v", nodeBin, err)
	}
	t.Logf("stub process started (PID %d)", stubProc.Process.Pid)

	// waitCh receives the result of Wait() exactly once.  We drive cleanup
	// through this channel so we never call Wait() twice (which would panic).
	waitCh := make(chan error, 1)
	go func() { waitCh <- stubProc.Wait() }()

	// cleanupStub kills the stub and drains waitCh.  Safe to call multiple
	// times — the second call is a no-op because the process is already dead.
	killed := false
	cleanupStub := func() {
		if !killed {
			killed = true
			_ = stubProc.Process.Kill()
			<-waitCh
		}
	}
	defer cleanupStub()

	// Give the stub a moment to fully initialise.
	time.Sleep(300 * time.Millisecond)

	// Sanity check: stub must be alive before the build starts.
	select {
	case exitErr := <-waitCh:
		t.Fatalf("stub process (PID %d) exited before the concurrent build started: %v",
			stubProc.Process.Pid, exitErr)
	default:
		t.Logf("stub process (PID %d) confirmed alive before build", stubProc.Process.Pid)
	}

	// ── Step 3: run make build-node concurrently ──────────────────────────────
	t.Log("running make build-node concurrently with the stub process …")
	buildCmd := exec.Command(makeBin, "build-node")
	buildCmd.Dir = blockchainDir
	var buildOut bytes.Buffer
	buildCmd.Stdout = &buildOut
	buildCmd.Stderr = &buildOut

	buildErr := buildCmd.Run()
	t.Logf("make build-node output:\n%s", buildOut.String())
	if buildErr != nil {
		t.Fatalf("make build-node failed: %v\n%s", buildErr, buildOut.String())
	}
	t.Log("make build-node completed successfully")

	// ── Step 4: assert stub process is still alive ────────────────────────────
	//
	// We check the waitCh channel (non-blocking).  If Wait() has returned, the
	// process exited — whether cleanly, via crash, or via fatal signal.
	//
	// Why NOT Signal(0): on Linux a crashed child stays as a zombie until
	// Wait() is called; signalling a zombie with signal 0 succeeds, producing
	// a false-green result.  Polling waitCh is the only reliable check.
	select {
	case exitErr := <-waitCh:
		// Process has already exited.  Re-send the value so cleanupStub can drain.
		waitCh <- exitErr
		killed = true // tell defer not to kill again
		t.Errorf("stub process (PID %d) exited during concurrent make build-node — "+
			"the atomic swap may have caused a crash or the process received an "+
			"unexpected fatal signal: %v",
			stubProc.Process.Pid, exitErr)
	default:
		t.Logf("stub process (PID %d) survived the concurrent build ✓", stubProc.Process.Pid)
	}

	// ── Step 5: kill the stub cleanly, then inspect the new binary ───────────
	cleanupStub()

	// ── Step 6: assert the new binary is a valid ELF ─────────────────────────
	assertELF(t, nodeBin)
}

// assertELF opens path and verifies the ELF identity header is well-formed.
func assertELF(t *testing.T, path string) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("cannot open binary %s after build: %v", path, err)
	}
	defer f.Close()

	// Read e_ident (16 bytes) + e_type (2 bytes) = 18 bytes total.
	header := make([]byte, 18)
	n, err := f.Read(header)
	if err != nil || n < 18 {
		t.Fatalf("binary %s is too short to be a valid ELF (read %d bytes, err=%v)",
			path, n, err)
	}

	// Magic bytes.
	if !bytes.Equal(header[:4], elfMagic) {
		t.Errorf("binary %s does not start with ELF magic bytes\n"+
			"  got : %#x\n  want: %#x",
			path, header[:4], elfMagic)
		return
	}
	t.Logf("ELF magic bytes verified ✓ (%#x)", header[:4])

	// ELF class: 2 = 64-bit.
	elfClass := header[4]
	if elfClass != 2 {
		t.Errorf("binary %s: unexpected ELF class %d (want 2 for 64-bit)", path, elfClass)
	} else {
		t.Logf("ELF class = %d (64-bit) ✓", elfClass)
	}

	// ELF data encoding: 1=LE, 2=BE.
	elfData := header[5]
	if elfData != 1 && elfData != 2 {
		t.Errorf("binary %s: invalid ELF data encoding byte %d", path, elfData)
	}

	// e_type at offset 16.
	var eType uint16
	if elfData == 1 {
		eType = binary.LittleEndian.Uint16(header[16:18])
	} else {
		eType = binary.BigEndian.Uint16(header[16:18])
	}
	switch eType {
	case 2:
		t.Logf("e_type = ET_EXEC (executable) ✓")
	case 3:
		t.Logf("e_type = ET_DYN (PIE executable) ✓")
	default:
		t.Errorf("binary %s: unexpected e_type %d (want ET_EXEC=2 or ET_DYN=3); "+
			"binary may be truncated or corrupted after concurrent write", path, eType)
	}

	// Sane file size (> 1 KiB).
	info, err := f.Stat()
	if err == nil {
		if info.Size() < 1024 {
			t.Errorf("binary %s is suspiciously small (%d bytes); "+
				"may have been truncated during concurrent write", path, info.Size())
		} else {
			t.Logf("binary size = %d bytes ✓", info.Size())
		}
	}
}
