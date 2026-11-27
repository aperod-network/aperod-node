package main

// height_index_repair_binary_test.go — end-to-end coverage for the startup
// height-index self-heal using the REAL compiled aperod-node binary.
//
// startup_integrity_test.go drives checkStartupIntegrity in-process with a
// capture logger.  That proves the helper's log level, but not that the
// production binary wires the helper into run(), routes the record through the
// real JSON logger at INFO, and then keeps starting.  This test closes that gap:
//
//  1. Build aperod-node from this package into a temp directory.
//  2. Start it as a single-validator devnet node with an isolated data dir,
//     loopback-only P2P, no bootnodes and the API disabled.  Wait until it has
//     finalized a few blocks, then stop it with SIGTERM.
//  3. With the node stopped, overwrite h/<tipHeight> in chain.db with an
//     all-zero hash (the state left by a live-LevelDB rsync or an OOM-kill).
//  4. Restart the binary, capturing stdout and stderr.  Assert that the repair
//     message is logged at INFO, that no ERROR record and no startup-integrity
//     WARN is emitted, and that startup continues: "node is running" is logged
//     and a block above the corrupted tip is finalized.
//  5. Stop the node and confirm h/<tipHeight> now points at the tip hash.
//
// Nothing here contacts a remote host: bootnodes is empty, the single
// allowed_peers fingerprint is a placeholder, and the API listener is off.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

const heightIndexRepairMsg = "startup integrity: height index was missing/zeroed — repaired from tip pointer"

// nodeLogLine is one parsed JSON record from the node's slog output.
type nodeLogLine struct {
	Raw    string
	Stream string
	Level  string
	Msg    string
	Fields map[string]any
}

// runningNode wraps a started aperod-node process and the lines it has printed.
type runningNode struct {
	t     *testing.T
	cmd   *exec.Cmd
	mu    sync.Mutex
	lines []nodeLogLine
	notif chan struct{}
	done  chan struct{}
	err   error
}

func buildAperodNodeBinary(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source file")
	}
	pkgDir := filepath.Dir(thisFile)
	bin := filepath.Join(t.TempDir(), "aperod-node")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = pkgDir
	var out bytes.Buffer
	build.Stdout = &out
	build.Stderr = &out
	if err := build.Run(); err != nil {
		t.Fatalf("go build aperod-node: %v\n%s", err, out.String())
	}
	return bin
}

func writeIsolatedDevnetConfig(t *testing.T, dir string) (cfgPath, dataDir string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	genesis, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "genesis-testnet.yaml"))
	if err != nil {
		t.Fatalf("resolve genesis path: %v", err)
	}
	if _, err := os.Stat(genesis); err != nil {
		t.Fatalf("genesis file: %v", err)
	}
	dataDir = filepath.Join(dir, "data")
	cfg := fmt.Sprintf(`network: devnet
data_dir: %q
log_level: info
p2p:
  listen_addr: "/ip4/127.0.0.1/tcp/0"
  bootnodes: []
  allowed_peers: ["1111111111111111111111111111111111111111111111111111111111111111"]
  max_peers: 1
  min_peers: 0
consensus:
  block_time: "1s"
  oracle_url: ""
api:
  enabled: false
  listen_addr: ""
genesis:
  file: %q
pruning:
  mode: archive
snapshot:
  periodic_snapshot_interval: 0
`, dataDir, genesis)
	cfgPath = filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, dataDir
}

// startAperodNode launches the binary and streams both stdout and stderr into
// parsed log lines.  A cleanup hook guarantees the child is killed.
func startAperodNode(t *testing.T, bin, cfgPath string) *runningNode {
	t.Helper()
	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Dir = filepath.Dir(cfgPath)
	// Keep the child away from operator-scoped behaviour switches.
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SCHED_RESTART") || strings.HasPrefix(kv, "APEROD_") || strings.HasPrefix(kv, "GOMEMLIMIT=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	n := &runningNode{t: t, cmd: cmd, notif: make(chan struct{}, 1), done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start aperod-node: %v", err)
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go n.consume(&readers, "stdout", stdout)
	go n.consume(&readers, "stderr", stderr)
	go func() {
		readers.Wait()
		n.err = cmd.Wait()
		close(n.done)
	}()
	t.Cleanup(func() {
		select {
		case <-n.done:
		default:
			_ = cmd.Process.Kill()
			<-n.done
		}
	})
	return n
}

func (n *runningNode) consume(wg *sync.WaitGroup, stream string, r io.Reader) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := nodeLogLine{Raw: sc.Text(), Stream: stream}
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			line.Fields = rec
			line.Level, _ = rec["level"].(string)
			line.Msg, _ = rec["msg"].(string)
		}
		n.mu.Lock()
		n.lines = append(n.lines, line)
		n.mu.Unlock()
		select {
		case n.notif <- struct{}{}:
		default:
		}
	}
}

func (n *runningNode) snapshot() []nodeLogLine {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]nodeLogLine(nil), n.lines...)
}

func (n *runningNode) dump() string {
	var b strings.Builder
	for _, l := range n.snapshot() {
		fmt.Fprintf(&b, "[%s] %s\n", l.Stream, l.Raw)
	}
	return b.String()
}

// waitFor blocks until pred matches a captured line, the process exits, or
// the timeout elapses.
func (n *runningNode) waitFor(desc string, timeout time.Duration, pred func(nodeLogLine) bool) nodeLogLine {
	n.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		for _, l := range n.snapshot() {
			if pred(l) {
				return l
			}
		}
		select {
		case <-n.notif:
		case <-n.done:
			for _, l := range n.snapshot() {
				if pred(l) {
					return l
				}
			}
			n.t.Fatalf("aperod-node exited (%v) before %s\noutput:\n%s", n.err, desc, n.dump())
		case <-deadline.C:
			n.t.Fatalf("timed out after %s waiting for %s\noutput:\n%s", timeout, desc, n.dump())
		}
	}
}

// stop sends SIGTERM and waits for a bounded, clean exit.
func (n *runningNode) stop(timeout time.Duration) {
	n.t.Helper()
	if err := n.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		n.t.Fatalf("SIGTERM aperod-node: %v", err)
	}
	select {
	case <-n.done:
		if n.err != nil {
			n.t.Fatalf("aperod-node exited with error after SIGTERM: %v\noutput:\n%s", n.err, n.dump())
		}
	case <-time.After(timeout):
		_ = n.cmd.Process.Kill()
		<-n.done
		n.t.Fatalf("aperod-node did not exit within %s of SIGTERM\noutput:\n%s", timeout, n.dump())
	}
}

func finalizedAbove(min uint64) func(nodeLogLine) bool {
	return func(l nodeLogLine) bool {
		if l.Msg != "block finalized" {
			return false
		}
		h, ok := l.Fields["height"].(float64)
		return ok && uint64(h) > min
	}
}

func rawHeightKey(h uint64) []byte {
	key := make([]byte, len("h/")+8)
	copy(key, "h/")
	binary.BigEndian.PutUint64(key[len("h/"):], h)
	return key
}

func TestAperodNodeBinary_ZeroedHeightIndex_RepairsAtINFOAndContinues(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real aperod-node binary; skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX signals")
	}

	bin := buildAperodNodeBinary(t)
	// The node binds a unix socket inside data_dir; t.TempDir() paths can
	// exceed the 108-byte sun_path limit, so use a short temp directory.
	workDir, err := os.MkdirTemp("", "apn-hidx-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	cfgPath, dataDir := writeIsolatedDevnetConfig(t, workDir)
	dbPath := filepath.Join(dataDir, "chain.db")

	// ── Run 1: create a short canonical chain, then stop cleanly. ───────────
	first := startAperodNode(t, bin, cfgPath)
	first.waitFor("genesis creation", 60*time.Second, func(l nodeLogLine) bool { return l.Msg == "genesis block created" })
	first.waitFor("block finalized above height 2", 60*time.Second, finalizedAbove(2))
	first.stop(60 * time.Second)

	// ── Corrupt: zero the tip's height-index entry while the node is down. ──
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store after run 1: %v", err)
	}
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		db.Close()
		t.Fatalf("GetTip: %v", err)
	}
	if tipHeight < 3 {
		db.Close()
		t.Fatalf("tip height %d after run 1, want >= 3", tipHeight)
	}
	if blk, err := db.GetBlockByHeight(tipHeight); err != nil || blk == nil || blk.Hash != tipHash {
		db.Close()
		t.Fatalf("pre-corruption h/%d not consistent with tip (blk=%v err=%v)", tipHeight, blk, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	raw, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		t.Fatalf("open raw leveldb: %v", err)
	}
	var zero crypto.Hash32
	if err := raw.Put(rawHeightKey(tipHeight), zero[:], &opt.WriteOptions{Sync: true}); err != nil {
		raw.Close()
		t.Fatalf("zero h/%d: %v", tipHeight, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw leveldb: %v", err)
	}

	// Confirm the corruption is visible through the store API the node uses.
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store after corruption: %v", err)
	}
	if blk, err := db.GetBlockByHeight(tipHeight); err != nil || blk != nil {
		db.Close()
		t.Fatalf("zeroed h/%d still resolves (blk=%v err=%v)", tipHeight, blk, err)
	}
	db.Close()

	// ── Run 2: the real binary must self-heal at INFO and keep starting. ────
	second := startAperodNode(t, bin, cfgPath)
	repair := second.waitFor("height-index repair log", 60*time.Second, func(l nodeLogLine) bool {
		return strings.Contains(l.Raw, heightIndexRepairMsg)
	})
	running := second.waitFor("node is running", 60*time.Second, func(l nodeLogLine) bool { return l.Msg == "node is running" })
	second.waitFor(fmt.Sprintf("block finalized above repaired tip %d", tipHeight), 60*time.Second, finalizedAbove(tipHeight))
	second.stop(60 * time.Second)

	if repair.Fields == nil {
		t.Fatalf("repair message was not a structured log record: %q", repair.Raw)
	}
	if repair.Level != "INFO" {
		t.Errorf("repair message level = %q, want INFO\nline: %s", repair.Level, repair.Raw)
	}
	if repair.Msg != heightIndexRepairMsg {
		t.Errorf("repair msg = %q, want %q", repair.Msg, heightIndexRepairMsg)
	}
	if h, ok := repair.Fields["tip_height"].(float64); !ok || uint64(h) != tipHeight {
		t.Errorf("repair tip_height = %v, want %d", repair.Fields["tip_height"], tipHeight)
	}
	if got, want := repair.Fields["tip_hash"], fmt.Sprintf("%x", tipHash[:8]); got != want {
		t.Errorf("repair tip_hash = %v, want %s", got, want)
	}

	lines := second.snapshot()
	repairIdx, runningIdx := -1, -1
	for i, l := range lines {
		if l.Raw == repair.Raw && repairIdx < 0 {
			repairIdx = i
		}
		if l.Raw == running.Raw && runningIdx < 0 {
			runningIdx = i
		}
		if l.Level == "ERROR" {
			t.Errorf("unexpected ERROR record during repaired startup: %s", l.Raw)
		}
		if strings.Contains(l.Msg, "startup integrity") && l.Level != "INFO" {
			t.Errorf("startup-integrity record at %s, want INFO only: %s", l.Level, l.Raw)
		}
		if strings.Contains(l.Raw, "height index repair FAILED") {
			t.Errorf("unexpected repair failure output: %s", l.Raw)
		}
	}
	if repairIdx < 0 || runningIdx < 0 || repairIdx > runningIdx {
		t.Errorf("repair log (index %d) must precede \"node is running\" (index %d)", repairIdx, runningIdx)
	}

	// ── The repair must be persisted to the height index on disk. ───────────
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store after run 2: %v", err)
	}
	defer db.Close()
	blk, err := db.GetBlockByHeight(tipHeight)
	if err != nil || blk == nil {
		t.Fatalf("h/%d still unresolved after repaired startup (blk=%v err=%v)", tipHeight, blk, err)
	}
	if blk.Hash != tipHash {
		t.Errorf("h/%d = %x, want repaired tip hash %x", tipHeight, blk.Hash[:8], tipHash[:8])
	}
	_, newTip, err := db.GetTip()
	if err != nil {
		t.Fatalf("GetTip after run 2: %v", err)
	}
	if newTip <= tipHeight {
		t.Errorf("tip height after run 2 = %d, want > %d (startup must continue producing blocks)", newTip, tipHeight)
	}
}
