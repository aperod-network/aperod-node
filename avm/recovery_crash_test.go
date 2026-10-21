package avm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

// This helper deliberately exits without Close or deferred cleanup. The parent
// must reopen LevelDB, rather than inspect state through the original handle.
func TestAVMCanonicalCommitCrashHelper(t *testing.T) {
	mode := os.Getenv("APEROD_TEST_AVM_CRASH")
	if mode == "" {
		return
	}
	dir := os.Getenv("APEROD_TEST_AVM_DIR")
	raw, err := os.ReadFile(filepath.Join(dir, "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := NewBlockExecutor(LevelStore{DB: db}).PrepareBlock(context.Background(), &block)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "before" {
		os.Exit(73)
	}
	if mode == "legacy-incomplete" {
		// Reproduce a legacy non-atomic write: canonical data is fsynced,
		// but neither AVM effects nor their commitment marker exists.
		if err := db.PutRawBlock(block.Hash(), block.Header.Height, raw); err != nil {
			t.Fatal(err)
		}
		if err := db.PutTip(block.Hash(), block.Header.Height); err != nil {
			t.Fatal(err)
		}
		os.Exit(73)
	}
	if mode != "after" {
		t.Fatalf("unknown crash phase %q", mode)
	}
	if err := CommitCanonicalBlock(db, &block, raw, prepared); err != nil {
		t.Fatal(err)
	}
	os.Exit(73)
}

func TestAVMCanonicalCommitSurvivesProcessCrash(t *testing.T) {
	for _, phase := range []string{"before", "after", "legacy-incomplete"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "db")
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			genesis := &core.Block{Header: core.BlockHeader{MerkleRoot: core.MerkleRoot(nil)}}
			empty, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), genesis)
			if err != nil {
				t.Fatal(err)
			}
			rawGenesis, err := json.Marshal(genesis)
			if err != nil {
				t.Fatal(err)
			}
			if err := CommitCanonicalBlock(db, genesis, rawGenesis, empty); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			tx := signedAVMTransaction(t, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule(false))
			block := &core.Block{
				Header: core.BlockHeader{Height: 1, PrevHash: genesis.Hash()},
				Txs:    []core.Transaction{tx},
			}
			block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
			prepared, err := NewBlockExecutor(NewMemoryStore()).PrepareBlock(context.Background(), block)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(block)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "candidate.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAVMCanonicalCommitCrashHelper$")
			cmd.Env = append(os.Environ(), "APEROD_TEST_AVM_CRASH="+phase, "APEROD_TEST_AVM_DIR="+dir)
			output, runErr := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 73 || ctx.Err() != nil {
				t.Fatalf("crash helper: %v, context: %v\n%s", runErr, ctx.Err(), output)
			}
			db, err = store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tipHash, tipHeight, err := db.GetTip()
			if err != nil {
				t.Fatal(err)
			}
			wantHash, wantHeight := genesis.Hash(), uint64(0)
			if phase != "before" {
				wantHash, wantHeight = block.Hash(), 1
			}
			if tipHash != wantHash || tipHeight != wantHeight {
				t.Fatalf("durable tip = %x/%d, want %x/%d", tipHash, tipHeight, wantHash, wantHeight)
			}
			body, err := db.GetRawBlock(block.Hash())
			if err != nil || (len(body) > 0) != (phase != "before") {
				t.Fatalf("candidate body: bytes=%d err=%v", len(body), err)
			}
			markerHash, commitment, found, err := db.GetAVMCommitment(1)
			if err != nil || found != (phase == "after") {
				t.Fatalf("candidate marker: found=%v err=%v", found, err)
			}
			if found && (markerHash != block.Hash() || commitment != prepared.WriteSetCommitment) {
				t.Fatal("durable marker differs from prepared effects")
			}
			for _, write := range prepared.Writes {
				value, found, err := db.GetAVMState(write.Key)
				if err != nil {
					t.Fatal(err)
				}
				if phase != "after" {
					if found {
						t.Fatalf("uncommitted AVM write survived crash: %x", write.Key)
					}
				} else if !found || !bytes.Equal(value, write.Value) {
					t.Fatalf("committed AVM write lost after crash: %x", write.Key)
				}
			}
			rebuilt, err := EnsureCanonicalState(context.Background(), db, 1, tipHeight, tipHash)
			if err != nil || rebuilt != (phase == "legacy-incomplete") {
				t.Fatalf("startup after %s crash: rebuilt=%v err=%v", phase, rebuilt, err)
			}
			if phase == "legacy-incomplete" {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := store.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				for _, write := range prepared.Writes {
					value, found, err := reopened.GetAVMState(write.Key)
					if err != nil || !found || !bytes.Equal(value, write.Value) {
						t.Fatalf("replayed write not durable: %x found=%v err=%v", write.Key, found, err)
					}
				}
				hash, commitment, found, err := reopened.GetAVMCommitment(1)
				if err != nil || !found || hash != block.Hash() || commitment != prepared.WriteSetCommitment {
					t.Fatalf("replayed marker not durable: found=%v err=%v", found, err)
				}
				rebuilt, err := EnsureCanonicalState(context.Background(), reopened, 1, tipHeight, tipHash)
				if err != nil || rebuilt {
					t.Fatalf("second restart: rebuilt=%v err=%v", rebuilt, err)
				}
			}
		})
	}
}
