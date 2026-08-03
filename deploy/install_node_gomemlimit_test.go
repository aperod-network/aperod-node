// Package deploy_test contains CI guards for the GOMEMLIMIT installer drop-in.
//
// Two tests are provided:
//
//  1. TestInstallNodeGomemlimit — shells out to test-install-node-gomemlimit.sh,
//     which extracts and runs the *real* drop-in creation and GOMEMLIMIT
//     computation blocks from install-node.sh against controlled temp-dir and
//     /proc/meminfo seams, then asserts the generated drop-in file is correct.
//
//  2. TestGoRuntimeRespectsGomemlimit — launches the memlimit_probe subprocess
//     (blockchain/deploy/testdata/memlimit_probe) with GOMEMLIMIT set via
//     environment variable — the same path used by the installer drop-in at
//     production runtime.  The probe disables GOGC and runs a sliding-window
//     allocation workload that would exhaust ~120 MiB without GOMEMLIMIT;
//     with GOMEMLIMIT the GC fires on the limit alone and peak RSS stays near
//     the configured cap.  The test asserts the subprocess exits 0 (peak RSS
//     within 2.5× GOMEMLIMIT).
//
// Run from the blockchain root:
//
//	go test ./deploy/...
package deploy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInstallNodeGomemlimit runs test-install-node-gomemlimit.sh as a
// subprocess.  The script extracts and executes the real drop-in creation
// block from install-node.sh so any future refactor that breaks drop-in
// generation will fail here.
//
// Skipped when bash is not available (e.g. Windows CI runners).
func TestInstallNodeGomemlimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test-install-node-gomemlimit.sh requires bash; skipping on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH; skipping TestInstallNodeGomemlimit")
	}

	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "test-install-node-gomemlimit.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatalf("test-install-node-gomemlimit.sh not found at %s", scriptPath)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("test-install-node-gomemlimit.sh reported failures: %v", err)
	}
}

// TestGoRuntimeRespectsGomemlimit confirms that the GOMEMLIMIT environment
// variable — the mechanism used by the installer drop-in — actually constrains
// heap growth under a realistic workload.
//
// The test:
//  1. Builds the memlimit_probe helper binary from
//     ./testdata/memlimit_probe/main.go.
//  2. Launches the probe as a subprocess with GOMEMLIMIT=33554432 (32 MiB).
//     The probe disables GOGC so the Go runtime's ONLY GC trigger is
//     GOMEMLIMIT, then allocates 30×4 MiB = 120 MiB in a sliding window
//     (only 8 MiB live at any time, 112 MiB becomes garbage).
//  3. Without GOMEMLIMIT the GC never fires and RSS reaches ~120 MiB,
//     far above the 2.5× tolerance (80 MiB); the probe exits non-zero.
//  4. With GOMEMLIMIT=32 MiB the GC fires whenever total memory approaches
//     the limit and peak RSS stays near ~32–45 MiB; the probe exits 0.
//
// Skipped on non-Linux (probe uses /proc/self/status) and when the go
// toolchain is unavailable.
func TestGoRuntimeRespectsGomemlimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("memlimit_probe uses /proc/self/status; skipping on " + runtime.GOOS)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not found in PATH; skipping TestGoRuntimeRespectsGomemlimit")
	}

	// ── 1. Build the probe binary into a temp directory ────────────────────
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("cannot determine script directory: %v", err)
	}
	probeSrc := filepath.Join(scriptDir, "testdata", "memlimit_probe")
	if _, err := os.Stat(probeSrc); os.IsNotExist(err) {
		t.Fatalf("memlimit_probe source not found at %s", probeSrc)
	}

	tmpDir := t.TempDir()
	probeBin := filepath.Join(tmpDir, "memlimit_probe")

	buildCmd := exec.Command("go", "build", "-o", probeBin, probeSrc)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("failed to build memlimit_probe: %v", err)
	}

	// ── 2. Run the probe with GOMEMLIMIT set via environment ────────────────
	// 32 MiB = 33554432 bytes.  The workload allocates 30×4 MiB = 120 MiB
	// total, so without GOMEMLIMIT the probe would exhaust ~120 MiB and fail.
	const limitBytes = 32 * 1024 * 1024 // 32 MiB
	limitStr := fmt.Sprintf("%d", limitBytes)

	probeCmd := exec.Command(probeBin)
	probeCmd.Env = append(os.Environ(), "GOMEMLIMIT="+limitStr)
	probeCmd.Stdout = os.Stdout
	probeCmd.Stderr = os.Stderr

	t.Logf("launching memlimit_probe with GOMEMLIMIT=%s B (%d MiB)", limitStr, limitBytes/1024/1024)

	if err := probeCmd.Run(); err != nil {
		t.Errorf("memlimit_probe exited with error: %v\n"+
			"  This means peak RSS exceeded 2.5× GOMEMLIMIT (%d MiB),\n"+
			"  indicating the runtime is not honouring the GOMEMLIMIT\n"+
			"  environment variable set by the installer drop-in.",
			err, limitBytes*5/2/1024/1024)
	}
}
