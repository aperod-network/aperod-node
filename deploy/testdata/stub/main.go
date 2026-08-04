// Stub is a minimal long-running process used by TestAtomicBinarySwap.
// It sleeps until it receives a signal or the timeout (120 s) expires.
// It reports the received signal as a non-zero exit code so the test can
// distinguish a clean kill (SIGKILL → exit -1 on Linux) from a fatal crash
// signal (SIGSEGV → exit ≥ 2).
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGSEGV, syscall.SIGBUS, syscall.SIGILL, syscall.SIGFPE, syscall.SIGABRT)

	select {
	case sig := <-sigs:
		fmt.Fprintf(os.Stderr, "stub: fatal signal received: %v\n", sig)
		os.Exit(2)
	case <-time.After(120 * time.Second):
		// Timed out — parent forgot to clean up.
		os.Exit(0)
	}
}
