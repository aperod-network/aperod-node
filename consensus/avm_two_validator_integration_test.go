package consensus

// This is intentionally in package consensus (rather than consensus_test): the
// assertion that a durability failure halts the engine is part of the consensus
// transition, not an API promise.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
	"github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/wasm"
)

// TestAVMTwoValidatorEngineIntegration replaces the old deploy-package
// simulation.  Every normal transition below goes through handleIncomingBlock,
// including CLSAG verification, staking, AVM preparation, and the canonical
// LevelDB commit boundary.
func TestAVMTwoValidatorEngineIntegration(t *testing.T) {
	keys := []crypto.ValidatorPrivKey{validatorKey(t, 0x11), validatorKey(t, 0x22)}
	pubs := []crypto.ValidatorPubKey{keys[0].Public(), keys[1].Public()}
	root := t.TempDir()
	a := newAVMEngineNode(t, root+"/a", keys, pubs)
	b := newAVMEngineNode(t, root+"/b", keys, pubs)

	alice, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	carol, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	dave, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	genesis, blinds := engineGenesis(t, keys[0], []*crypto.WalletKeyPair{alice, carol, dave}, 50_000_000)
	a.installGenesis(t, genesis)
	b.installGenesis(t, genesis)

	// A real v3 deposit is deliberately in the same accepted block as the
	// fee-paying v6 transaction.  It exercises registry and staked-UTXO state.
	staker, stakerPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	stake := a.addStakeUTXO(t, staker, stakerPub)
	b.addStakeUTXO(t, staker, stakerPub)

	_, avmSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deploy, _, decoys := buildAVMCLSAG(t, genesis, alice, bob, blinds[0], avmSigner, core.AVMDeployContract, [32]byte{}, 0, stateWriteModule())
	call, _, callDecoys := buildAVMCLSAG(t, genesis, carol, bob, blinds[1], avmSigner, core.AVMExecuteContract, deploy.AVM.ContractID, 1, nil)
	replay, _, replayDecoys := buildAVMCLSAG(t, genesis, dave, bob, blinds[2], avmSigner, core.AVMExecuteContract, deploy.AVM.ContractID, 1, nil)
	decoys = append(decoys, callDecoys...)
	decoys = append(decoys, replayDecoys...)
	for _, d := range decoys {
		a.utxos.Add(&core.UTXO{OneTimePub: d.OneTimePub, AmountCommit: d.AmountCommit})
		b.utxos.Add(&core.UTXO{OneTimePub: d.OneTimePub, AmountCommit: d.AmountCommit})
	}
	block1 := engineBlock(t, 1, 1, genesis.Hash(), keys[0], []core.Transaction{stake, deploy})
	a.accept(t, block1)
	b.accept(t, block1)
	a.assertSame(t, b, block1.Header.Height, deploy)
	block2 := engineBlock(t, 2, 2, block1.Hash(), keys[1], []core.Transaction{call})
	setBlockBaseFee(t, block2, a.engine.expectedBaseFee(), keys[1])
	a.accept(t, block2)
	b.accept(t, block2)
	a.assertSame(t, b, block2.Header.Height, call)

	// Model an interrupted durable commit on B: canonical blocks remain, while
	// AVM state is gone. EnsureCanonicalState must replay rather than merely
	// trusting the in-memory executor.
	var deletes []store.AVMWrite
	if err := b.db.IterAVMState(func(key, _ []byte) error {
		deletes = append(deletes, store.AVMWrite{Key: append([]byte(nil), key...)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.db.ApplyAVMState(deletes); err != nil {
		t.Fatal(err)
	}
	b.close(t)
	b = recoverAVMEngineNode(t, root+"/b", keys, pubs, decoys, staker, stakerPub)
	a.assertSame(t, b, block2.Header.Height, call)

	// A nonce replay is signed at the AVM layer but retains the prior CLSAG
	// witness, so Engine must reject it before any complete state transition.
	// Snapshot all observable state (including the durable tip) on both peers.
	beforeA, beforeB := a.snapshot(t), b.snapshot(t)
	bad := engineBlock(t, 3, 3, block2.Hash(), keys[0], []core.Transaction{replay})
	setBlockBaseFee(t, bad, a.engine.expectedBaseFee(), keys[0])
	if err := a.engine.handleIncomingBlock(bad); err == nil {
		t.Fatal("accepted invalid nonce/execution block on A")
	}
	if err := b.engine.handleIncomingBlock(bad); err == nil {
		t.Fatal("accepted invalid nonce/execution block on B")
	}
	if got := a.snapshot(t); !reflect.DeepEqual(beforeA, got) {
		t.Fatalf("A mutated after rejected block: %#v != %#v", beforeA, got)
	}
	if got := b.snapshot(t); !reflect.DeepEqual(beforeB, got) {
		t.Fatalf("B mutated after rejected block: %#v != %#v", beforeB, got)
	}

	// The failure path is also reached through Engine, not by calling the
	// executor/store directly. No callback writes anything, so the DB tip must
	// remain at block 1 and the package-local halted latch must be set.
	failing := newAVMEngineNode(t, root+"/failing", keys, pubs)
	failing.installGenesis(t, genesis)
	for _, d := range decoys {
		failing.utxos.Add(&core.UTXO{OneTimePub: d.OneTimePub, AmountCommit: d.AmountCommit})
	}
	failing.addStakeUTXO(t, staker, stakerPub)
	beforeFail := failing.snapshot(t)
	failing.engine.cfg.OnCanonicalBlock = func(block *core.Block, prepared *avm.PreparedBlock) error {
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		if err := failing.db.Close(); err != nil {
			return err
		}
		return avm.CommitCanonicalBlock(failing.db, block, raw, prepared)
	}
	if err := failing.engine.handleIncomingBlock(block1); err == nil {
		t.Fatal("canonical persistence failure accepted")
	}
	if !failing.engine.halted.Load() {
		t.Fatal("engine was not halted after canonical persistence failure")
	}
	reopened, err := store.Open(failing.path)
	if err != nil {
		t.Fatal(err)
	}
	failing.db = reopened
	afterFail := failing.snapshot(t)
	if !reflect.DeepEqual(beforeFail, afterFail) {
		t.Fatalf("canonical persistence failure changed state: %#v != %#v", beforeFail, afterFail)
	}
	if err := failing.engine.handleIncomingBlock(block1); err == nil {
		t.Fatal("halted engine accepted a subsequent block")
	}
}

func TestEngineRequiresCanonicalPersistenceForEveryBlock(t *testing.T) {
	keys := []crypto.ValidatorPrivKey{validatorKey(t, 0x31), validatorKey(t, 0x32)}
	pubs := []crypto.ValidatorPubKey{keys[0].Public(), keys[1].Public()}
	wallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	genesis, _ := engineGenesis(t, keys[0], []*crypto.WalletKeyPair{wallet}, 50_000_000)

	for _, test := range []struct {
		name             string
		activationHeight uint64
	}{
		{name: "ordinary pre-AVM block", activationHeight: 0},
		{name: "empty block in active AVM era", activationHeight: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := newAVMEngineNodeAtActivation(
				t, t.TempDir(), pubs, test.activationHeight,
			)
			node.installGenesis(t, genesis)
			node.engine.cfg.OnCanonicalBlock = nil
			before := node.snapshot(t)

			expected := node.engine.proposerAt(1)
			var proposer crypto.ValidatorPrivKey
			for _, key := range keys {
				if expected != nil && expected.Equals(key.Public()) {
					proposer = key
					break
				}
			}
			if proposer == nil {
				t.Fatal("scheduled proposer private key not found")
			}
			block := engineBlock(t, 1, 1, genesis.Hash(), proposer, nil)
			err := node.engine.handleIncomingBlock(block)
			if err == nil {
				t.Fatal("accepted block without canonical persistence callback")
			}
			if !strings.Contains(err.Error(), "OnCanonicalBlock is required") {
				t.Fatalf("block failed before durability boundary: %v", err)
			}
			if !node.engine.halted.Load() {
				t.Fatal("engine did not halt after missing canonical persistence callback")
			}
			if after := node.snapshot(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("missing persistence callback changed state: %#v != %#v", before, after)
			}
		})
	}
}

type avmEngineNode struct {
	path     string
	db       *store.DB
	chain    *core.Chain
	utxos    *core.UTXOSet
	registry *core.ValidatorRegistry
	executor *avm.BlockExecutor
	engine   *Engine
}

func newAVMEngineNode(t *testing.T, path string, _ []crypto.ValidatorPrivKey, pubs []crypto.ValidatorPubKey) *avmEngineNode {
	return newAVMEngineNodeAtActivation(t, path, pubs, 1)
}

func newAVMEngineNodeAtActivation(
	t *testing.T,
	path string,
	pubs []crypto.ValidatorPubKey,
	activationHeight uint64,
) *avmEngineNode {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	n := &avmEngineNode{path: path, db: db, chain: core.NewChain(), utxos: core.NewUTXOSet()}
	n.registry = core.NewValidatorRegistry()
	n.registry.SetUTXOSet(n.utxos)
	n.executor = avm.NewBlockExecutor(avm.LevelStore{DB: db})
	poolCfg := core.DefaultMempoolConfig()
	poolCfg.CurrentHeight = n.chain.Height
	poolCfg.RingCTV4ActivationHeight = 1
	poolCfg.RingCTCLSAGActivationHeight = 1
	poolCfg.AVMActivationHeight = activationHeight
	avmStore := avm.LevelStore{DB: db}
	poolCfg.AVMNonceLookup = func(signer [32]byte) (uint64, error) {
		return avm.SignerNonce(avmStore, signer)
	}
	poolCfg.AVMAdmissionCheck = func(payload *core.AVMPayload) error {
		return avm.ValidateMempoolAdmission(avmStore, payload)
	}
	n.engine = NewEngine(Config{BlockTime: time.Hour, BFTThreshold: .667, Validators: pubs,
		Registry: n.registry, RingCTCLSAGActivationHeight: 1,
		AVMActivationHeight: activationHeight, AVMExecutor: n.executor,
		OnCanonicalBlock: func(block *core.Block, prepared *avm.PreparedBlock) error {
			raw, err := json.Marshal(block)
			if err != nil {
				return err
			}
			return avm.CommitCanonicalBlock(db, block, raw, prepared)
		}}, n.chain, core.NewMempool(poolCfg), testNopLogger())
	n.engine.SetTxVerifier(core.NewTxVerifier(n.utxos), n.utxos)
	t.Cleanup(func() {
		if n.db != nil {
			_ = n.db.Close()
		}
	})
	return n
}

func testNopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func engineGenesis(t *testing.T, validator crypto.ValidatorPrivKey, owners []*crypto.WalletKeyPair, amount uint64) (*core.Block, []crypto.BlindFactor) {
	t.Helper()
	var txs []core.Transaction
	blinds := make([]crypto.BlindFactor, len(owners))
	for i, owner := range owners {
		addr := crypto.AddressFromKeys(crypto.TestnetByte, owner)
		_, spend, view, err := crypto.DecodeAddress(addr)
		if err != nil {
			t.Fatal(err)
		}
		stealth, err := crypto.CreateStealthOutput(spend, view)
		if err != nil {
			t.Fatal(err)
		}
		blind, err := crypto.NewBlindFactor()
		if err != nil {
			t.Fatal(err)
		}
		commit, err := crypto.Commit(amount, blind)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := crypto.ProveRange(amount, blind)
		if err != nil {
			t.Fatal(err)
		}
		coinbase := core.Transaction{Version: 1, Outputs: []core.Output{{OneTimePub: stealth.OneTimePub,
			TxPubKey: stealth.TxPubKey, AmountCommit: commit, EncAmount: core.EncryptAmount(amount, &stealth.HsScalar)}},
			RangeProofs: []*crypto.RangeProof{proof}}
		blinds[i] = blind
		txs = append(txs, coinbase)
	}
	return engineBlock(t, 0, 0, crypto.Hash32{}, validator, txs), blinds
}

func buildAVMCLSAG(t *testing.T, genesis *core.Block, owner, recipient *crypto.WalletKeyPair, blind crypto.BlindFactor, priv ed25519.PrivateKey,
	action core.AVMAction, contract [32]byte, nonce uint64, code []byte) (core.Transaction, *core.BuildResult, []core.DecoyUTXO) {
	t.Helper()
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	scanner := core.NewWalletScanner(owner.View.Private, owner.Spend.Public, owner.View.Public, crypto.TestnetByte)
	owned := scanner.ScanChain(chain, 0, 0)
	if len(owned) != 1 {
		t.Fatalf("owned genesis outputs=%d", len(owned))
	}
	owned[0].Blind = blind
	decoys := make([]core.DecoyUTXO, crypto.RingSize-1)
	for i := range decoys {
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		b, err := crypto.NewBlindFactor()
		if err != nil {
			t.Fatal(err)
		}
		c, err := crypto.Commit(uint64(i+1), b)
		if err != nil {
			t.Fatal(err)
		}
		decoys[i] = core.DecoyUTXO{OneTimePub: keys.Spend.Public, AmountCommit: c}
	}
	pub := priv.Public().(ed25519.PublicKey)
	payload := core.AVMPayload{Action: action, ContractID: contract, Code: code, Entry: "run", GasLimit: 1_000,
		Nonce: nonce, AccessList: []core.AVMAccess{{Key: []byte("key"), Write: true}}}
	copy(payload.Signer[:], pub)
	if action == core.AVMDeployContract {
		payload.ContractID = core.DeriveAVMContractID(payload.Signer, nonce, code)
	}
	hash := payload.SigningHash()
	copy(payload.Signature[:], ed25519.Sign(priv, hash[:]))
	from, to := crypto.AddressFromKeys(crypto.TestnetByte, owner), crypto.AddressFromKeys(crypto.TestnetByte, recipient)
	result, err := core.NewTxBuilder(owner.Spend.Private, owner.View.Private, owner.Spend.Public, owned,
		core.InitialBaseFeePerByte).WithAVM(payload).WithDecoys(decoys).Build(1_000_000, to, from)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Tx.UsesCLSAG() || result.Tx.Fee < result.Tx.MinFeeAt(core.InitialBaseFeePerByte) {
		t.Fatal("fixture did not build a fee-paying CLSAG v6 transaction")
	}
	return result.Tx, result, decoys
}

func engineBlock(t *testing.T, height uint64, round uint32, previous crypto.Hash32, signer crypto.ValidatorPrivKey, txs []core.Transaction) *core.Block {
	t.Helper()
	header := core.BlockHeader{Height: height, Round: round, PrevHash: previous, Timestamp: time.Now().Add(-time.Second).UnixNano(),
		ValidatorPub: signer.Public(), BaseFee: core.InitialBaseFeePerByte, MerkleRoot: core.MerkleRoot(txs)}
	if err := header.Sign(signer); err != nil {
		t.Fatal(err)
	}
	return &core.Block{Header: header, Txs: txs}
}

func setBlockBaseFee(t *testing.T, block *core.Block, fee uint64, signer crypto.ValidatorPrivKey) {
	t.Helper()
	block.Header.BaseFee = fee
	if err := block.Header.Sign(signer); err != nil {
		t.Fatal(err)
	}
}

func stateWriteModule() []byte {
	module := &wasm.Module{
		TypeSection:     []*wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32, wasm.ValueTypeI32, wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}}, {}},
		ImportSection:   []*wasm.Import{{Type: wasm.ExternTypeFunc, Module: "apro", Name: "state_write", DescFunc: 0}},
		FunctionSection: []wasm.Index{1}, MemorySection: &wasm.Memory{Min: 1, Max: avm.MaxMemoryPages, IsMaxEncoded: true},
		ExportSection: []*wasm.Export{{Type: wasm.ExternTypeFunc, Name: "run", Index: 1}, {Type: wasm.ExternTypeMemory, Name: "memory", Index: 0}},
		CodeSection:   []*wasm.Code{{Body: []byte{byte(wasm.OpcodeI32Const), 0, byte(wasm.OpcodeI32Const), 3, byte(wasm.OpcodeI32Const), 3, byte(wasm.OpcodeI32Const), 5, byte(wasm.OpcodeCall), 0, byte(wasm.OpcodeDrop), byte(wasm.OpcodeEnd)}}},
		DataSection:   []*wasm.DataSegment{{OffsetExpression: &wasm.ConstantExpression{Opcode: wasm.OpcodeI32Const, Data: []byte{0}}, Init: []byte("keyvalue")}},
	}
	return binary.EncodeModule(module)
}

func (n *avmEngineNode) installGenesis(t *testing.T, block *core.Block) {
	t.Helper()
	if err := n.chain.SetGenesis(block); err != nil {
		t.Fatal(err)
	}
	if err := n.utxos.ApplyBlock(block); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	p, err := n.executor.PrepareBlock(context.Background(), block)
	if err != nil {
		t.Fatal(err)
	}
	if err := avm.CommitCanonicalBlock(n.db, block, raw, p); err != nil {
		t.Fatal(err)
	}
}
func (n *avmEngineNode) accept(t *testing.T, block *core.Block) {
	t.Helper()
	if err := n.engine.handleIncomingBlock(block); err != nil {
		t.Fatal(err)
	}
}
func (n *avmEngineNode) close(t *testing.T) {
	t.Helper()
	if err := n.db.Close(); err != nil {
		t.Fatal(err)
	}
	n.db = nil
}

func recoverAVMEngineNode(t *testing.T, path string, keys []crypto.ValidatorPrivKey, pubs []crypto.ValidatorPubKey, decoys []core.DecoyUTXO, staker crypto.ValidatorPrivKey, stakerPub crypto.ValidatorPubKey) *avmEngineNode {
	t.Helper()
	n := newAVMEngineNode(t, path, keys, pubs)
	// This test's stake source is a pre-genesis fixture, analogous to the
	// fixture ring members below; production startup restores it from its UTXO
	// index before replaying stake transitions.
	n.addStakeUTXO(t, staker, stakerPub)
	_, tip, err := n.db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	for h := uint64(0); h <= tip; h++ {
		raw, err := n.db.GetRawBlockByHeight(h)
		if err != nil {
			t.Fatal(err)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		if h == 0 {
			err = n.chain.SetGenesis(&block)
		} else {
			err = n.chain.AddBlock(&block)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = n.utxos.ApplyBlock(&block); err != nil {
			t.Fatal(err)
		}
		if err = n.registry.ReplayBlockStakeTxs(block.Txs, h); err != nil {
			t.Fatal(err)
		}
	}
	if rebuilt, err := avm.EnsureCanonicalState(context.Background(), n.db, 1, tip, n.chain.Tip().Hash()); err != nil || !rebuilt {
		t.Fatalf("EnsureCanonicalState replay: rebuilt=%v err=%v", rebuilt, err)
	}
	// The fixture's ring members are pre-genesis test UTXOs. Production startup
	// loads these from its UTXO index; install the same deterministic fixture
	// before comparing the reconstructed in-memory set.
	for _, d := range decoys {
		n.utxos.Add(&core.UTXO{OneTimePub: d.OneTimePub, AmountCommit: d.AmountCommit})
	}
	return n
}

func (n *avmEngineNode) addStakeUTXO(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey) core.Transaction {
	t.Helper()
	const amount = uint64(10_000_000_000_000)
	var one crypto.Point32
	copy(one[:], []byte(pub))
	blind, err := crypto.DeterministicMintBlind(one, amount)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := crypto.Commit(amount, blind)
	if err != nil {
		t.Fatal(err)
	}
	var hash crypto.Hash32
	hash[0] = 0xcc
	n.utxos.Add(&core.UTXO{TxHash: hash, OneTimePub: one, AmountCommit: commit})
	sig, err := priv.Sign(core.StakeSignMsgV2(core.StakeDeposit, pub, amount, hash, 0))
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := priv.Sign(core.StakeOwnershipSignMsg(hash, 0))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := core.EncodeStakeExtraV3(core.StakeDeposit, pub, amount, sig, hash, 0, blind, ownership)
	if err != nil {
		t.Fatal(err)
	}
	return core.Transaction{Version: core.TxVersionStake, Extra: extra}
}

type avmStateEntry struct{ Key, Value []byte }
type engineSnapshot struct {
	DBHeight, ChainHeight uint64
	DBHash, ChainHash     crypto.Hash32
	UTXO                  core.UTXOSnapshot
	Validators            []core.ValidatorEntry
	Mempool               int
	AVM                   []avmStateEntry
}

func (n *avmEngineNode) snapshot(t *testing.T) engineSnapshot {
	t.Helper()
	hash, height, err := n.db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	s := n.utxos.TakeSnapshot()
	sort.Slice(s.ActiveUTXOs, func(i, j int) bool {
		return bytes.Compare(s.ActiveUTXOs[i].OneTimePub[:], s.ActiveUTXOs[j].OneTimePub[:]) < 0
	})
	sort.Slice(s.StakedUTXOs, func(i, j int) bool {
		return bytes.Compare(s.StakedUTXOs[i].OneTimePub[:], s.StakedUTXOs[j].OneTimePub[:]) < 0
	})
	sort.Slice(s.SpentDecoys, func(i, j int) bool {
		return bytes.Compare(s.SpentDecoys[i].OneTimePub[:], s.SpentDecoys[j].OneTimePub[:]) < 0
	})
	validators := n.registry.AllEntries()
	sort.Slice(validators, func(i, j int) bool { return validators[i].PubKey.Hex() < validators[j].PubKey.Hex() })
	var state []avmStateEntry
	if err := n.db.IterAVMState(func(key, value []byte) error {
		state = append(state, avmStateEntry{append([]byte(nil), key...), append([]byte(nil), value...)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return engineSnapshot{
		DBHeight: height, DBHash: hash, ChainHeight: n.chain.Height(), ChainHash: n.chain.Tip().Hash(),
		UTXO: s, Validators: validators, Mempool: n.engine.pool.Count(), AVM: state,
	}
}
func (n *avmEngineNode) assertSame(t *testing.T, other *avmEngineNode, height uint64, tx core.Transaction) {
	t.Helper()
	if !reflect.DeepEqual(n.snapshot(t), other.snapshot(t)) {
		t.Fatal("validator state diverged")
	}
	_, a, af, err := n.db.GetAVMCommitment(height)
	if err != nil || !af {
		t.Fatal(err)
	}
	_, b, bf, err := other.db.GetAVMCommitment(height)
	if err != nil || !bf || a != b {
		t.Fatal("AVM commitments diverged")
	}
	h := tx.Hash()
	key := append([]byte{0xff, 'a', 'v', 'm', '/', 'r', '/'}, h[:]...)
	va, oka, _ := n.db.GetAVMState(key)
	vb, okb, _ := other.db.GetAVMState(key)
	if !oka || !okb || !bytes.Equal(va, vb) {
		t.Fatal("AVM receipts diverged")
	}
	stateKey := append(append([]byte(nil), tx.AVM.ContractID[:]...), []byte("/key")...)
	nonceKey := append([]byte{0xff, 'a', 'v', 'm', '/', 'n', '/'}, tx.AVM.Signer[:]...)
	for _, key := range [][]byte{stateKey, nonceKey} {
		va, oka, _ = n.db.GetAVMState(key)
		vb, okb, _ = other.db.GetAVMState(key)
		if !oka || !okb || !bytes.Equal(va, vb) {
			t.Fatalf("AVM durable state diverged at %x", key)
		}
	}
}

func validatorKey(t *testing.T, b byte) crypto.ValidatorPrivKey {
	t.Helper()
	k, err := crypto.ValidatorPrivKeyFromBytes(bytes.Repeat([]byte{b}, ed25519.SeedSize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
