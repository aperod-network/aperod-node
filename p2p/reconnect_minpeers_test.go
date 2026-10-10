package p2p_test

// TestSync_RelayNode_ReconnectViaMinPeers verifies that a relay node with
// MinPeers=1 and *no* configured Bootnodes automatically reconnects to the
// validator after a transient connection drop — using the MinPeers path inside
// maintainLoop rather than the bootnode bypass.
//
// Without a Bootnodes entry the relay is subject to exponential back-off after
// a short drop (connection lasted < stableConnTime=60 s → OnDialFail fires →
// 5 s window).  The test waits for that window to expire by polling
// HostCanDial, then fires HostTriggerMaintain to advance the maintainLoop
// immediately without waiting for the real 10 s ticker.
//
// maintainLoop then sees len(peers)=0 < MinPeers=1, iterates peerList (seeded
// via HostAddKnownPeer, mirroring what MsgPeers peer-exchange does in
// production), finds validatorAddr not connected, and calls dialPeer.  The
// relay reconnects and re-syncs to the validator tip via GetHeaders.

import (
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/p2p"
)

// minimalChainHandler is a minimal p2p.Handler backed by a *core.Chain, used
// to wire up p2p hosts without depending on the full engine stack.
type minimalChainHandler struct {
	chain        *core.Chain
	onBlockCalls atomic.Int64
}

func (h *minimalChainHandler) OnBlock(b *core.Block) {
	h.onBlockCalls.Add(1)
	if h.chain.GetByHash(b.Hash()) != nil {
		return
	}
	_ = h.chain.AddBlock(b)
}

func TestSync_AheadNodeDoesNotRequestStaleBlocksFromBehindPeer(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()
	genesis := buildTestBlock(t, nil, validatorPriv, validatorPub, 0)

	aheadChain := core.NewChain()
	if err := aheadChain.SetGenesis(genesis); err != nil {
		t.Fatalf("ahead SetGenesis: %v", err)
	}
	prev := genesis
	for height := uint64(1); height <= 8; height++ {
		block := buildTestBlock(t, prev, validatorPriv, validatorPub, height)
		if err := aheadChain.AddBlock(block); err != nil {
			t.Fatalf("ahead AddBlock %d: %v", height, err)
		}
		prev = block
	}

	behindChain := core.NewChain()
	if err := behindChain.SetGenesis(genesis); err != nil {
		t.Fatalf("behind SetGenesis: %v", err)
	}
	prev = genesis
	for height := uint64(1); height <= 3; height++ {
		block := aheadChain.GetByHeight(height)
		if block == nil {
			t.Fatalf("ahead block %d missing", height)
		}
		if err := behindChain.AddBlock(block); err != nil {
			t.Fatalf("behind AddBlock %d: %v", height, err)
		}
		prev = block
	}

	aheadHandler := &minimalChainHandler{chain: aheadChain}
	aheadHost := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "ahead",
		UserAgent:  "aperod/test",
	}, aheadHandler, log)
	aheadHost.SetHeaderProvider(aheadChain)
	if err := aheadHost.Start(); err != nil {
		t.Fatalf("aheadHost.Start: %v", err)
	}
	t.Cleanup(aheadHost.Stop)

	behindHandler := &minimalChainHandler{chain: behindChain}
	behindHost := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "behind",
		UserAgent:  "aperod/test",
	}, behindHandler, log)
	behindHost.SetHeaderProvider(behindChain)
	if err := behindHost.Start(); err != nil {
		t.Fatalf("behindHost.Start: %v", err)
	}
	t.Cleanup(behindHost.Stop)

	aheadHost.DialPeer(behindHost.ListenAddr())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if behindChain.Height() == aheadChain.Height() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if behindChain.Height() != aheadChain.Height() {
		t.Fatalf("behind node did not sync: got height %d, want %d",
			behindChain.Height(), aheadChain.Height())
	}
	if got := aheadHandler.onBlockCalls.Load(); got != 0 {
		t.Fatalf("ahead node received %d stale block(s) from behind peer; want 0", got)
	}

	// A validator that just caught up may relay a historical block back over a
	// parallel connection. The ahead node must discard it before OnBlock and
	// before the per-peer ingest limiter can stall keepalive Pong processing.
	historical := aheadChain.GetByHeight(2)
	if historical == nil {
		t.Fatal("historical block 2 missing")
	}
	behindHost.BroadcastBlock(historical)
	time.Sleep(100 * time.Millisecond)
	if got := aheadHandler.onBlockCalls.Load(); got != 0 {
		t.Fatalf("ahead node dispatched %d relayed historical block(s); want 0", got)
	}
}
func (h *minimalChainHandler) OnTransaction(_ *core.Transaction) {}
func (h *minimalChainHandler) OnVote(_ p2p.VoteMsg)              {}
func (h *minimalChainHandler) CurrentHeight() uint64             { return h.chain.Height() }
func (h *minimalChainHandler) CurrentTailHashes(n int) []crypto.Hash32 {
	tip := h.chain.Tip()
	if tip == nil {
		return nil
	}
	h32 := tip.Hash()
	return []crypto.Hash32{h32}
}
func (h *minimalChainHandler) GetBlock(hash crypto.Hash32) *core.Block {
	return h.chain.GetByHash(hash)
}

// buildTestBlock constructs and signs a block extending parent for use in p2p
// package tests.
func buildTestBlock(t *testing.T, parent *core.Block, priv []byte, pub crypto.ValidatorPubKey, height uint64) *core.Block {
	t.Helper()
	var prevHash crypto.Hash32
	if parent != nil {
		prevHash = parent.Hash()
	}
	cb := core.CoinbaseTx(crypto.Point32(pub), 1_000_000)
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       height,
		PrevHash:     prevHash,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: pub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	if err := hdr.Sign(priv); err != nil {
		t.Fatalf("buildTestBlock: Sign height=%d: %v", height, err)
	}
	return &core.Block{Header: hdr, Txs: txs}
}

func TestSync_RelayNode_ReconnectViaMinPeers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	const numInitial = 4 // blocks pre-mined before the connection drop
	const numExtra = 3   // blocks mined while relay is disconnected

	validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()

	// ── Build genesis + numInitial blocks on the validator chain ─────────────
	genesis := buildTestBlock(t, nil, validatorPriv, validatorPub, 0)

	validatorChain := core.NewChain()
	if err := validatorChain.SetGenesis(genesis); err != nil {
		t.Fatalf("validator SetGenesis: %v", err)
	}

	prev := genesis
	for i := 1; i <= numInitial; i++ {
		b := buildTestBlock(t, prev, validatorPriv, validatorPub, uint64(i))
		if err := validatorChain.AddBlock(b); err != nil {
			t.Fatalf("validator AddBlock %d: %v", i, err)
		}
		prev = b
	}

	// ── Relay chain: starts with only genesis ────────────────────────────────
	relayChain := core.NewChain()
	if err := relayChain.SetGenesis(genesis); err != nil {
		t.Fatalf("relay SetGenesis: %v", err)
	}

	// ── Validator p2p host ───────────────────────────────────────────────────
	validatorHandler := &minimalChainHandler{chain: validatorChain}
	hostValidator := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		NodeID:     "validator",
		UserAgent:  "aperod/test",
	}, validatorHandler, log)
	hostValidator.SetHeaderProvider(validatorChain)
	if err := hostValidator.Start(); err != nil {
		t.Fatalf("hostValidator.Start: %v", err)
	}
	t.Cleanup(hostValidator.Stop)

	validatorAddr := hostValidator.ListenAddr()
	if validatorAddr == "" {
		t.Skip("ListenAddr not available")
	}

	// ── Relay p2p host — MinPeers=1, NO Bootnodes ────────────────────────────
	// Without a Bootnodes entry the relay cannot bypass exponential back-off.
	// After the connection is dropped (lasting < stableConnTime=60 s) the peer
	// manager records a dial failure and blocks re-dialling for 5 s.  The test
	// waits for that window via HostCanDial, then fires HostTriggerMaintain to
	// immediately advance the maintainLoop — which sees count < MinPeers and
	// re-dials the peer from peerList.
	relayHandler := &minimalChainHandler{chain: relayChain}
	hostRelay := p2p.NewHost(p2p.Config{
		ListenAddr: "127.0.0.1:0",
		MaxPeers:   10,
		MinPeers:   1,
		NodeID:     "relay",
		UserAgent:  "aperod/test",
		// No Bootnodes — this is the key difference from the existing
		// TestSync_RelayNode_ReconnectAfterDrop test which bypasses back-off
		// by registering the validator as a bootnode.
		GetBlockStallTimeout: 500 * time.Millisecond,
	}, relayHandler, log)
	hostRelay.SetHeaderProvider(relayChain)
	if err := hostRelay.Start(); err != nil {
		t.Fatalf("hostRelay.Start: %v", err)
	}
	t.Cleanup(hostRelay.Stop)

	// ── Phase 1: initial sync via explicit DialPeer ───────────────────────────
	hostRelay.DialPeer(validatorAddr)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if relayChain.Height() >= uint64(numInitial) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if relayChain.Height() != uint64(numInitial) {
		t.Fatalf("phase 1: relay height = %d, want %d — initial sync timed out",
			relayChain.Height(), numInitial)
	}
	t.Logf("✓ phase 1: relay synced to height=%d", numInitial)

	// ── Phase 2: seed peerList and drop the connection ────────────────────────
	// HostAddKnownPeer mirrors what the MsgPeers peer-exchange protocol does in
	// production: it records the validator's address in the relay's peerList so
	// that maintainLoop can re-dial it when the peer count falls below MinPeers.
	// In a real deployment the handshake triggers a MsgGetPeers / MsgPeers
	// exchange that populates peerList automatically; we use the test helper here
	// to avoid depending on that timing.
	p2p.HostAddKnownPeer(hostRelay, validatorAddr)

	// DropPeer closes the TCP connection without banning the address.  Since the
	// session lasted well under stableConnTime (60 s), the handleConn deferred
	// back-off registers an OnDialFail, starting the 5 s back-off window for the
	// validator address.
	dropped := hostRelay.DropPeer(validatorAddr)
	if !dropped {
		t.Fatal("phase 2: DropPeer returned false — connection was not found")
	}
	t.Logf("✓ phase 2: connection dropped; back-off window started")

	// ── Phase 3: validator mines extra blocks while relay is offline ──────────
	for i := 1; i <= numExtra; i++ {
		height := uint64(numInitial + i)
		b := buildTestBlock(t, prev, validatorPriv, validatorPub, height)
		if err := validatorChain.AddBlock(b); err != nil {
			t.Fatalf("validator AddBlock height=%d: %v", height, err)
		}
		prev = b
	}
	wantFinalHeight := uint64(numInitial + numExtra)
	wantFinalTip := validatorChain.Tip().Hash()
	t.Logf("✓ phase 3: validator mined %d more blocks; tip height=%d", numExtra, wantFinalHeight)

	// ── Phase 4: wait for back-off to expire, then trigger maintainLoop ───────
	// DropPeer closes the TCP connection, but the handleConn goroutine's
	// deferred OnDialFail runs asynchronously after the close returns.  We
	// must first poll until HostCanDial returns false — that proves OnDialFail
	// has been recorded and the 5 s back-off window has started.  Only then
	// do we poll for the window to expire (CanDial returns true again).
	// Polling for false first eliminates the race where CanDial is true
	// because no failure state exists yet.
	onDialFailFired := false
	onDialFailDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(onDialFailDeadline) {
		if !p2p.HostCanDial(hostRelay, validatorAddr) {
			onDialFailFired = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !onDialFailFired {
		t.Fatal("phase 4: OnDialFail did not fire within 2 s — CanDial never went false after DropPeer")
	}
	t.Logf("✓ phase 4a: OnDialFail confirmed (CanDial=false); waiting for 5 s back-off to expire")

	// Now wait for the back-off window to expire.  The first failure gives
	// backoffDuration(1) = 5 s.  Allow up to 8 s for scheduler jitter.
	backoffExpired := false
	backoffDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(backoffDeadline) {
		if p2p.HostCanDial(hostRelay, validatorAddr) {
			backoffExpired = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !backoffExpired {
		t.Fatal("phase 4b: back-off window did not expire within 8 s — CanDial still false")
	}
	t.Logf("✓ phase 4b: back-off expired; firing maintainLoop tick")

	// Fire one maintain tick: maintainLoop sees len(peers)=0 < MinPeers=1,
	// iterates peerList (which now contains validatorAddr), and calls
	// dialPeer — this is the MinPeers reconnect path under test.
	p2p.HostTriggerMaintain(hostRelay)

	// ── Phase 5: wait for relay to reconnect and re-sync ─────────────────────
	// The fresh handshake Pong carries height=wantFinalHeight; the relay sees
	// peerHeight > localHeight and issues MsgGetHeaders to catch up — the same
	// path as the initial sync in phase 1.
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if relayChain.Height() >= wantFinalHeight {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	gotHeight := relayChain.Height()
	if gotHeight != wantFinalHeight {
		t.Errorf("phase 5: relay height = %d, want %d — MinPeers reconnect/resync timed out or regressed",
			gotHeight, wantFinalHeight)
		return
	}

	gotTip := relayChain.Tip().Hash()
	if gotTip != wantFinalTip {
		t.Errorf("phase 5: relay tip hash mismatch:\n  got  %x\n  want %x", gotTip[:8], wantFinalTip[:8])
		return
	}
	t.Logf("✓ phase 5: relay reconnected via MinPeers path and resynced: height=%d tip=%x",
		gotHeight, gotTip[:8])
}
