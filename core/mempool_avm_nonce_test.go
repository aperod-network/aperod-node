package core

import (
	stded25519 "crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

func mempoolAVMTx(t *testing.T, nonce uint64, keyImageByte byte) Transaction {
	t.Helper()
	publicKey, privateKey, err := stded25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var signer [32]byte
	copy(signer[:], publicKey)
	payload := &AVMPayload{
		Action:   AVMDeployContract,
		Code:     []byte{0, 'a', 's', 'm'},
		Entry:    "run",
		GasLimit: AVMMinGasLimit,
		Nonce:    nonce,
		Signer:   signer,
	}
	payload.ContractID = DeriveAVMContractID(signer, nonce, payload.Code)
	signingHash := payload.SigningHash()
	copy(payload.Signature[:], stded25519.Sign(privateKey, signingHash[:]))

	ring := make([]crypto.RingMember, crypto.RingSize)
	commitments := make([]crypto.Commitment, crypto.RingSize)
	responses := make([][32]byte, crypto.RingSize)
	var keyImage crypto.KeyImage
	keyImage[0] = keyImageByte
	if keyImage == (crypto.KeyImage{}) {
		keyImage[0] = 1
	}
	return Transaction{
		Version: TxVersionAVM,
		Inputs: []RingInput{{
			KeyImage:        keyImage,
			Ring:            ring,
			RingCommitments: commitments,
		}},
		Outputs:         []Output{{OneTimePub: crypto.Point32{1}}},
		Fee:             1_000_000_000,
		RangeProofs:     []*crypto.RangeProof{{}},
		CLSAGSignatures: []*crypto.CLSAGSignature{{S: responses}},
		AVM:             payload,
	}
}

func avmNonceMempool(t *testing.T, lookup func([32]byte) (uint64, error)) *Mempool {
	t.Helper()
	cfg := DefaultMempoolConfig()
	cfg.CurrentHeight = func() uint64 { return 100 }
	cfg.RingCTV4ActivationHeight = 1
	cfg.RingCTCLSAGActivationHeight = 1
	cfg.AVMActivationHeight = 1
	cfg.AVMNonceLookup = lookup
	return NewMempool(cfg)
}

func TestMempoolRejectsStaleAndFutureAVMNoncesBeforeAdmission(t *testing.T) {
	for _, nonce := range []uint64{6, 8} {
		pool := avmNonceMempool(t, func([32]byte) (uint64, error) { return 7, nil })
		err := pool.Add(mempoolAVMTx(t, nonce, byte(nonce)))
		if err == nil || !strings.Contains(err.Error(), "AVM nonce") {
			t.Fatalf("nonce %d: got %v, want nonce rejection", nonce, err)
		}
		if pool.Count() != 0 {
			t.Fatalf("nonce %d entered mempool", nonce)
		}
	}
}

func TestMempoolAVMNonceLookupFailsClosed(t *testing.T) {
	pool := avmNonceMempool(t, func([32]byte) (uint64, error) {
		return 0, errors.New("store unavailable")
	})
	err := pool.Add(mempoolAVMTx(t, 0, 1))
	if err == nil || !strings.Contains(err.Error(), "nonce lookup failed") {
		t.Fatalf("got %v, want fail-closed lookup error", err)
	}
}

func TestMempoolAllowsOnlyOnePendingAVMTransactionPerSigner(t *testing.T) {
	pool := avmNonceMempool(t, func([32]byte) (uint64, error) { return 0, nil })
	first := mempoolAVMTx(t, 0, 1)
	second := first
	second.Inputs = append([]RingInput(nil), first.Inputs...)
	second.Inputs[0].KeyImage[0] = 2

	if err := pool.Add(first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	if err := pool.Add(second); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("got %v, want per-signer pending rejection", err)
	}
	pool.Remove(first.Hash())
	if err := pool.Add(second); err != nil {
		t.Fatalf("reservation not released after removal: %v", err)
	}
}

func TestMempoolConcurrentAVMSignerFloodAdmitsOne(t *testing.T) {
	pool := avmNonceMempool(t, func([32]byte) (uint64, error) { return 0, nil })
	template := mempoolAVMTx(t, 0, 1)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 1; i <= 32; i++ {
		tx := template
		tx.Inputs = append([]RingInput(nil), template.Inputs...)
		tx.Inputs[0].KeyImage[0] = byte(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if pool.Add(tx) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || pool.Count() != 1 {
		t.Fatalf("accepted=%d count=%d, want exactly one", accepted.Load(), pool.Count())
	}
}

func TestMempoolAVMSignerReservationReleasedByBanAndTTL(t *testing.T) {
	for _, release := range []struct {
		name string
		run  func(*Mempool, Transaction)
	}{
		{"ban", func(pool *Mempool, tx Transaction) { pool.BanTx(tx.Hash()) }},
		{"ttl", func(pool *Mempool, tx Transaction) {
			pool.mu.Lock()
			pool.entries[tx.Hash()].Received = time.Now().Add(-2 * pool.cfg.TTL)
			pool.mu.Unlock()
			if evicted := pool.Evict(); evicted != 1 {
				t.Fatalf("evicted=%d, want 1", evicted)
			}
		}},
	} {
		t.Run(release.name, func(t *testing.T) {
			pool := avmNonceMempool(t, func([32]byte) (uint64, error) { return 0, nil })
			first := mempoolAVMTx(t, 0, 1)
			replacement := first
			replacement.Inputs = append([]RingInput(nil), first.Inputs...)
			replacement.Inputs[0].KeyImage[0] = 2
			if err := pool.Add(first); err != nil {
				t.Fatal(err)
			}
			release.run(pool, first)
			if err := pool.Add(replacement); err != nil {
				t.Fatalf("reservation not released: %v", err)
			}
		})
	}
}

func TestMempoolLoadRestoresAVMSignerReservation(t *testing.T) {
	lookup := func([32]byte) (uint64, error) { return 0, nil }
	firstPool := avmNonceMempool(t, lookup)
	first := mempoolAVMTx(t, 0, 1)
	if err := firstPool.Add(first); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := firstPool.Save(dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}

	loadedPool := avmNonceMempool(t, lookup)
	if loaded := loadedPool.Load(dataDir, nil); loaded != 1 {
		t.Fatalf("loaded=%d, want 1", loaded)
	}
	conflict := first
	conflict.Inputs = append([]RingInput(nil), first.Inputs...)
	conflict.Inputs[0].KeyImage[0] = 2
	if err := loadedPool.Add(conflict); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("got %v, want restored signer reservation", err)
	}
}
