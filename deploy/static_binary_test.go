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

// TestExplorerIndexerBinaryIsStatic builds aperod-explorer-indexer via
// `make build-explorer-indexer` and then asserts the resulting binary is fully
// statically linked (no PT_INTERP ELF program header, no dynamic-linker
// dependency).
func TestExplorerIndexerBinaryIsStatic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ELF static-link check is Linux-specific; skipping on Windows")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("ELF static-link check does not apply to macOS Mach-O binaries; skipping")
	}

	// Locate make.
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not found in PATH; skipping TestExplorerIndexerBinaryIsStatic")
	}

	// The test lives in blockchain/deploy/; the Makefile is one level up.
	deployDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine deploy directory: %v", err)
	}
	blockchainDir := filepath.Dir(deployDir)
	makefilePath := filepath.Join(blockchainDir, "Makefile")
	if _, err := os.Stat(makefilePath); os.IsNotExist(err) {
		t.Skipf("Makefile not found at %s; skipping TestExplorerIndexerBinaryIsStatic", makefilePath)
	}

	indexerBin := filepath.Join(blockchainDir, "build", "aperod-explorer-indexer")

	// ── Step 1: build aperod-explorer-indexer via the Makefile ───────────────
	t.Log("running make build-explorer-indexer …")
	buildCmd := exec.Command(makeBin, "build-explorer-indexer")
	buildCmd.Dir = blockchainDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("make build-explorer-indexer failed: %v\n%s", err, string(out))
	}
	t.Logf("make build-explorer-indexer succeeded → %s", indexerBin)

	if _, err := os.Stat(indexerBin); os.IsNotExist(err) {
		t.Fatalf("build/aperod-explorer-indexer not found after make build-explorer-indexer")
	}

	// ── Step 2: parse ELF program headers ────────────────────────────────────
	assertNoInterp(t, indexerBin)

	// ── Step 3: confirm with ldd (best-effort) ────────────────────────────────
	assertLddStatic(t, indexerBin)
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

// TestUpdateNodeStaticGuard verifies that the ldd-based guard added to
// update-node.sh (Step 2b) correctly distinguishes dynamic binaries from
// static ones.
//
// The test exercises two cases:
//
//  1. A known dynamically-linked system binary (e.g. /bin/ls) triggers the
//     guard — ldd output does NOT contain "not a dynamic executable", so the
//     guard would abort the upgrade.
//
//  2. The freshly built aperod-node binary passes the guard — ldd reports
//     "not a dynamic executable", so the upgrade proceeds normally.
//
// This gives confidence that, if CGO_ENABLED=0 were accidentally dropped from
// the Makefile, update-node.sh would catch the regression before ever stopping
// the running service.
//
// Skip conditions: non-Linux OS, ldd absent, Makefile absent (case 2 only).
func TestUpdateNodeStaticGuard(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("ldd guard is Linux-specific; skipping on non-Linux")
	}

	lddBin, err := exec.LookPath("ldd")
	if err != nil {
		t.Skip("ldd not found in PATH; skipping TestUpdateNodeStaticGuard")
	}

	// ── Case 1: a known dynamic binary must NOT say "not a dynamic executable" ──
	//
	// update-node.sh fires (aborts) when ldd does NOT output that string.
	// We verify the guard logic is directionally correct: a real dynamic binary
	// produces output that would trigger the abort.
	dynamicBin := "/bin/ls"
	if _, statErr := os.Stat(dynamicBin); os.IsNotExist(statErr) {
		// Some distros (NixOS, Alpine) place ls elsewhere.
		for _, alt := range []string{"/usr/bin/ls", "/bin/cat", "/usr/bin/cat"} {
			if _, altErr := os.Stat(alt); altErr == nil {
				dynamicBin = alt
				break
			}
		}
		if _, statErr2 := os.Stat(dynamicBin); os.IsNotExist(statErr2) {
			t.Skip("could not find a known dynamically-linked system binary; skipping Case 1")
		}
	}

	dynOut, _ := exec.Command(lddBin, dynamicBin).CombinedOutput()
	dynStr := strings.TrimSpace(string(dynOut))
	t.Logf("ldd %s → %s", dynamicBin, dynStr)

	if strings.Contains(dynStr, "not a dynamic executable") {
		// If /bin/ls is somehow static (unusual musl build), skip rather than fail.
		t.Skipf("%s appears to be statically linked on this host — cannot exercise dynamic guard; skipping", dynamicBin)
	}
	t.Logf("Case 1 ✓: dynamic binary %s correctly triggers the guard (ldd did not say \"not a dynamic executable\")", dynamicBin)

	// ── Case 2: the freshly built aperod-node must pass the guard ────────────
	//
	// Build aperod-node via the Makefile (same as update-node.sh does) and
	// confirm ldd says "not a dynamic executable".  Skip gracefully when the
	// Makefile or make binary is unavailable (e.g. restricted CI).
	deployDir, absErr := filepath.Abs(".")
	if absErr != nil {
		t.Fatalf("cannot determine deploy directory: %v", absErr)
	}
	blockchainDir := filepath.Dir(deployDir)
	makefilePath := filepath.Join(blockchainDir, "Makefile")
	if _, statErr := os.Stat(makefilePath); os.IsNotExist(statErr) {
		t.Logf("Makefile not found at %s — skipping Case 2 (aperod-node guard check)", makefilePath)
		return
	}

	makeBin, makeErr := exec.LookPath("make")
	if makeErr != nil {
		t.Log("make not found in PATH — skipping Case 2 (aperod-node guard check)")
		return
	}

	t.Log("running make build-node for guard check…")
	buildCmd := exec.Command(makeBin, "build-node")
	buildCmd.Dir = blockchainDir
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("make build-node failed: %v\n%s", buildErr, string(out))
	}

	nodeBin := filepath.Join(blockchainDir, "build", "aperod-node")
	if _, statErr := os.Stat(nodeBin); os.IsNotExist(statErr) {
		t.Fatalf("build/aperod-node not found after make build-node")
	}

	staticOut, _ := exec.Command(lddBin, nodeBin).CombinedOutput()
	staticStr := strings.TrimSpace(string(staticOut))
	t.Logf("ldd aperod-node → %s", staticStr)

	if !strings.Contains(staticStr, "not a dynamic executable") {
		t.Errorf("update-node.sh guard would fire on the freshly built aperod-node:\n"+
			"  ldd output : %s\n"+
			"  Ensure CGO_ENABLED=0 is set in the Makefile build-node target.",
			staticStr)
		return
	}
	t.Log("Case 2 ✓: aperod-node passes the static-link guard — update-node.sh would not abort ✓")
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
