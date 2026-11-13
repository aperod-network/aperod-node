package consensus

import (
	"testing"
	"time"
)

func TestSubmitVoteBackpressuresInsteadOfDropping(t *testing.T) {
	engine := &Engine{
		newVoteCh: make(chan FinalizeMsg, 1),
		done:      make(chan struct{}),
	}
	engine.newVoteCh <- FinalizeMsg{Height: 1}
	result := make(chan bool, 1)
	go func() {
		result <- engine.SubmitVote(FinalizeMsg{Height: 2})
	}()
	select {
	case ok := <-result:
		t.Fatalf("vote submission returned before capacity was available: %v", ok)
	case <-time.After(25 * time.Millisecond):
	}
	<-engine.newVoteCh
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("vote submission was rejected while the engine was running")
		}
	case <-time.After(time.Second):
		t.Fatal("vote submission did not resume when capacity became available")
	}
	if vote := <-engine.newVoteCh; vote.Height != 2 {
		t.Fatalf("queued vote height = %d, want 2", vote.Height)
	}
}

func TestSubmitVoteReturnsWhenEngineStops(t *testing.T) {
	done := make(chan struct{})
	close(done)
	engine := &Engine{newVoteCh: make(chan FinalizeMsg), done: done}
	if engine.SubmitVote(FinalizeMsg{Height: 1}) {
		t.Fatal("vote submission succeeded after the consensus loop stopped")
	}
}
