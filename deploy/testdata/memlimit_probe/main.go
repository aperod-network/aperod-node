// Command memlimit_probe is a subprocess helper used by
// TestGoRuntimeRespectsGomemlimit in deploy_test.
//
// It verifies that the GOMEMLIMIT environment variable — the same mechanism
// used by the installer drop-in — actually constrains Go heap growth under a
// workload that would exceed the limit if GOMEMLIMIT were absent.
//
// Strategy
// --------
// GOGC is disabled at startup (debug.SetGCPercent(-1)) so the ONLY GC trigger
// is GOMEMLIMIT.  A sliding-window workload then allocates 4 MiB chunks in a
// loop, keeping just the last WINDOW chunks live and letting older ones become
// garbage.  Without GOMEMLIMIT the GC never fires and RSS grows to
// ITERS×CHUNK = 120 MiB.  With GOMEMLIMIT the GC fires whenever total memory
// approaches the limit, keeping peak RSS near the limit.
//
// Exit codes
//   0 — peak RSS stayed within 2.5× GOMEMLIMIT (pass)
//   1 — peak RSS exceeded 2.5× GOMEMLIMIT (fail — limit not being honoured)
//   2 — GOMEMLIMIT env var missing or not a positive integer
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	chunkSize = 4 * 1024 * 1024 // 4 MiB per allocation burst
	window    = 2               // keep 2 most-recent chunks live (8 MiB live set)
	iters     = 30              // 30 × 4 MiB = 120 MiB total allocated
)

func main() {
	// The Go runtime reads GOMEMLIMIT from the environment during
	// schedinit() — before main() runs.  We just need to parse it here
	// for our own tolerance calculation.
	limitStr := os.Getenv("GOMEMLIMIT")
	limitBytes, err := strconv.ParseInt(limitStr, 10, 64)
	if err != nil || limitBytes <= 0 {
		fmt.Fprintf(os.Stderr,
			"memlimit_probe: GOMEMLIMIT not set or not a positive integer (got %q)\n",
			limitStr)
		os.Exit(2)
	}

	// Disable the percentage-based GC trigger entirely.
	// With GOGC=off the Go runtime will NOT run a GC cycle unless forced
	// by GOMEMLIMIT (or an explicit runtime.GC() call — which we omit).
	// This makes the test a clean demonstration of GOMEMLIMIT's effect.
	debug.SetGCPercent(-1)

	// Sliding-window workload.
	// Each iteration allocates one chunk and touches every byte (prevents
	// the compiler from optimising the allocation away).  Chunks older
	// than WINDOW fall off the slice and become GC-eligible garbage.
	live := make([][]byte, 0, window+1)
	var peakRSS int64

	for i := 0; i < iters; i++ {
		chunk := make([]byte, chunkSize)
		for j := range chunk {
			chunk[j] = byte(i ^ j) // prevent dead-code elimination
		}
		live = append(live, chunk)
		if len(live) > window {
			live = live[1:] // drop oldest; it becomes garbage
		}

		rss := readRSS()
		if rss > peakRSS {
			peakRSS = rss
		}
	}

	runtime.KeepAlive(live) // ensure window slice survives to end of loop

	// tolerance = 2.5 × GOMEMLIMIT.
	// With GOMEMLIMIT enabled, peak RSS should stay near the limit (≈32–48 MiB
	// for a 32 MiB limit).  Without GOMEMLIMIT+GOGC=off, RSS reaches
	// iters×chunkSize = 120 MiB, which far exceeds this tolerance.
	tolerance := limitBytes + limitBytes*3/2 // = 2.5 × limitBytes

	fmt.Printf("GOMEMLIMIT=%d B  peakRSS=%d MiB  tolerance=%d MiB  iters=%d\n",
		limitBytes, peakRSS/1024/1024, tolerance/1024/1024, iters)

	if peakRSS > tolerance {
		fmt.Fprintf(os.Stderr,
			"FAIL: peakRSS %d MiB exceeded 2.5× GOMEMLIMIT tolerance %d MiB\n"+
				"      (GOMEMLIMIT=%d B is not constraining heap growth)\n",
			peakRSS/1024/1024, tolerance/1024/1024, limitBytes)
		os.Exit(1)
	}

	fmt.Printf("PASS: peakRSS %d MiB ≤ 2.5× GOMEMLIMIT %d MiB\n",
		peakRSS/1024/1024, limitBytes/1024/1024)
}

// readRSS returns the Resident Set Size of the current process in bytes by
// reading VmRSS from /proc/self/status.  Returns 0 on any error so a
// transient read failure does not abort the probe.
func readRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line) // "VmRSS:" "<N>" "kB"
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 0
}
