package p2p

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

type voteBackpressureHandler struct {
	votes atomic.Int32
}

func (*voteBackpressureHandler) OnBlock(*core.Block)                   {}
func (*voteBackpressureHandler) OnTransaction(*core.Transaction)       {}
func (h *voteBackpressureHandler) OnVote(VoteMsg)                      { h.votes.Add(1) }
func (*voteBackpressureHandler) CurrentHeight() uint64                 { return 0 }
func (*voteBackpressureHandler) CurrentTailHashes(int) []crypto.Hash32 { return nil }
func (*voteBackpressureHandler) GetBlock(crypto.Hash32) *core.Block    { return nil }

func TestVoteDispatchAppliesPerPeerBackpressureWithoutDropping(t *testing.T) {
	handler := &voteBackpressureHandler{}
	host := &Host{
		handler: handler,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	peer := &Peer{
		addr: "8.8.8.8:30303",
		voteBucket: peerTokenBucket{
			tokens:   1,
			lastTime: time.Now(),
			rate:     50,
			burst:    1,
		},
	}
	payload, err := json.Marshal(VoteMsg{Height: 1, ValidatorPub: make([]byte, 32), Signature: make([]byte, 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(peer, MsgVote, payload); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := host.dispatch(peer, MsgVote, payload); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 10*time.Millisecond {
		t.Fatalf("second vote was not rate limited: elapsed %s", elapsed)
	}
	if got := handler.votes.Load(); got != 2 {
		t.Fatalf("authenticated votes delivered to consensus = %d, want 2", got)
	}
}
