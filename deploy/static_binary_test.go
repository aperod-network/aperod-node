// Package deploy_test contains CI guards for the node deployment scripts.
//
// This file verifies that build/aperod-node is a fully statically linked
// binary — containing no PT_INTERP ELF program header — so it can start on
// any Linux distribution regardless of the host GLIBC version.
//
// Background: when aperod-node was compiled without CGO_ENABLED=0 it
// linked against GLIBC_2.32 and GLIBC_2.34 (Ubuntu 22.04 toolchain).
// Debian 11 and Ubuntu 20.04 ship GLIBC 2.31 and refused to start the
// binary with "GLIBC_2.34 not found", causing 640+ crash-loop restarts on
// rucode before a static binary was manually compiled.
//
// The Makefile build-node target now passes CGO_ENABLED=0.  This test
// confirms that invariant holds by:
//
//  1. Running `make build-node` from the blockchain root.
//  2. Parsing the resulting ELF to confirm no PT_INTERP segment is present
//     (presence of PT_INTERP would mean the binary depends on a dynamic linker).
//  3. Running `ldd build/aperod-node` (when available) and asserting the
//     output contains "not a dynamic executable".
//
// # Skip conditions
//
//   - Running on Windows or macOS (Linux-specific ELF check).
//   - `make` not found in PATH.
//   - The Makefile is not reachable from the test's working directory (deploy/).
//
// # Running manually
//
//	# From the blockchain root:
//	go test ./deploy/... -run TestNodeBinaryIsStatic -v -timeout 300s
package deploy_test

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNodeBinaryIsStatic builds aperod-node via `make build-node` and then
// asserts the resulting binary is fully statically linked (no PT_INTERP ELF
// program header, no dynamic-linker dependency).
func TestNodeBinaryIsStatic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ELF static-link check is Linux-specific; skipping on Windows")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("ELF static-link check does not apply to macOS Mach-O binaries; skipping")
	}

	// Locate make.
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not found in PATH; skipping TestNodeBinaryIsStatic")
	}

	// The test lives in blockchain/deploy/; the Makefile is one level up.
	deployDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine deploy directory: %v", err)
	}
	blockchainDir := filepath.Dir(deployDir)
	makefilePath := filepath.Join(blockchainDir, "Makefile")
	if _, err := os.Stat(makefilePath); os.IsNotExist(err) {
		t.Skipf("Makefile not found at %s; skipping TestNodeBinaryIsStatic", makefilePath)
	}

	nodeBin := filepath.Join(blockchainDir, "build", "aperod-node")

	// ── Step 1: build aperod-node via the Makefile ────────────────────────────
	t.Log("running make build-node …")
	buildCmd := exec.Command(makeBin, "build-node")
	buildCmd.Dir = blockchainDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("make build-node failed: %v\n%s", err, string(out))
	}
	t.Logf("make build-node succeeded → %s", nodeBin)

	if _, err := os.Stat(nodeBin); os.IsNotExist(err) {
		t.Fatalf("build/aperod-node not found after make build-node")
	}

	// ── Step 2: parse ELF program headers ────────────────────────────────────
	// A dynamically linked binary contains a PT_INTERP segment that names the
	// dynamic linker (e.g. /lib64/ld-linux-x86-64.so.2).  A fully static
	// binary has no PT_INTERP segment at all.
	assertNoInterp(t, nodeBin)

	// ── Step 3: confirm with ldd (best-effort) ────────────────────────────────
	// ldd is not present in every environment (e.g. musl-libc Alpine images),
	// so failure to locate ldd is not itself a test failure.  When available,
	// ldd must report "not a dynamic executable" for a static binary.
	assertLddStatic(t, nodeBin)
}

// TestCliBinaryIsStatic builds aperod via `make build-cli` and then
// asserts the resulting binary is fully statically linked (no PT_INTERP ELF
// program header, no dynamic-linker dependency).
func TestCliBinaryIsStatic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ELF static-link check is Linux-specific; skipping on Windows")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("ELF static-link check does not apply to macOS Mach-O binaries; skipping")
	}

	// Locate make.
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not found in PATH; skipping TestCliBinaryIsStatic")
	}

	// The test lives in blockchain/deploy/; the Makefile is one level up.
	deployDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine deploy directory: %v", err)
	}
	blockchainDir := filepath.Dir(deployDir)
	makefilePath := filepath.Join(blockchainDir, "Makefile")
	if _, err := os.Stat(makefilePath); os.IsNotExist(err) {
		t.Skipf("Makefile not found at %s; skipping TestCliBinaryIsStatic", makefilePath)
	}

	cliBin := filepath.Join(blockchainDir, "build", "aperod")

	// ── Step 1: build aperod via the Makefile ────────────────────────────────
	t.Log("running make build-cli …")
	buildCmd := exec.Command(makeBin, "build-cli")
	buildCmd.Dir = blockchainDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("make build-cli failed: %v\n%s", err, string(out))
	}
	t.Logf("make build-cli succeeded → %s", cliBin)

	if _, err := os.Stat(cliBin); os.IsNotExist(err) {
		t.Fatalf("build/aperod not found after make build-cli")
	}

	// ── Step 2: parse ELF program headers ────────────────────────────────────
	assertNoInterp(t, cliBin)

	// ── Step 3: confirm with ldd (best-effort) ────────────────────────────────
	assertLddStatic(t, cliBin)
}

// assertNoInterp opens path as an ELF file and fails the test if any
// PT_INTERP (dynamic-linker path) program header is found.
func assertNoInterp(t *testing.T, path string) {
	t.Helper()

	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("cannot open %s as ELF: %v", path, err)
	}
	defer f.Close()

	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			// Read the interpreter path for a helpful error message.
			interp := make([]byte, prog.Filesz)
			if _, err := prog.ReadAt(interp, 0); err == nil {
				// Trim the NUL terminator if present.
				interp = []byte(strings.TrimRight(string(interp), "\x00"))
			}
			t.Errorf("%s is NOT statically linked: PT_INTERP segment found\n"+
				"  interpreter : %s\n"+
				"  This means the binary requires a dynamic linker and will fail\n"+
				"  on Debian 11 / Ubuntu 20.04 with \"GLIBC_X.YY not found\".\n"+
				"  Fix: ensure CGO_ENABLED=0 is set in the corresponding Makefile build target.",
				path, string(interp))
			return
		}
	}

	t.Log("ELF PT_INTERP segment absent — binary is fully static ✓")
}

// assertLddStatic runs ldd on path and checks that the output contains
// "not a dynamic executable".  When ldd is not found the check is skipped
// with a log message (ldd absence is not a test failure).
func assertLddStatic(t *testing.T, path string) {
	t.Helper()

	lddBin, err := exec.LookPath("ldd")
	if err != nil {
		t.Log("ldd not found in PATH — skipping ldd cross-check (ELF check above is authoritative)")
		return
	}

	// ldd exits non-zero for static binaries on some distros; capture output
	// regardless of exit code.
	out, _ := exec.Command(lddBin, path).CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	t.Logf("ldd output: %s", outStr)

	if !strings.Contains(outStr, "not a dynamic executable") {
		t.Errorf("ldd did not report \"not a dynamic executable\" for %s\n"+
			"  ldd output: %s\n"+
			"  Ensure CGO_ENABLED=0 is set in the Makefile build-node target.",
			path, outStr)
		return
	}

	t.Log("ldd confirmed: \"not a dynamic executable\" ✓")
}
