package consensus

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestSlashing_NoDoubleSign(t *testing.T) {
	sd := newSlashingDetector()
	_, pub, _ := crypto.GenerateValidatorKey()
	var h1 crypto.Hash32
	h1[0] = 1

	ev := sd.DetectDoubleSign(1, 0, pub, h1, []byte("sig1"))
	if ev != nil {
		t.Errorf("expected no evidence for first block, got: %s", ev.String())
	}
	if ev := sd.RecordAcceptedBlock(1, 0, pub, h1, []byte("sig1")); ev != nil {
		t.Fatalf("record accepted block: %s", ev.String())
	}
}

func TestSlashing_SameBlockTwice(t *testing.T) {
	sd := newSlashingDetector()
	_, pub, _ := crypto.GenerateValidatorKey()
	var h1 crypto.Hash32
	h1[0] = 1

	sd.RecordAcceptedBlock(1, 0, pub, h1, []byte("sig1"))
	ev := sd.DetectDoubleSign(1, 0, pub, h1, []byte("sig1"))
	if ev != nil {
		t.Error("same block twice should not produce evidence")
	}
}

func TestSlashing_DoubleSign(t *testing.T) {
	sd := newSlashingDetector()
	_, pub, _ := crypto.GenerateValidatorKey()
	var h1, h2 crypto.Hash32
	h1[0] = 0xAA
	h2[0] = 0xBB

	sd.RecordAcceptedBlock(2, 0, pub, h1, []byte("sig1"))
	ev := sd.DetectDoubleSign(2, 0, pub, h2, []byte("sig2"))
	if ev == nil {
		t.Fatal("expected double-sign evidence")
	}
	if ev.Height != 2 || ev.Round1 != 0 || ev.Round2 != 0 {
		t.Errorf("wrong evidence location: height=%d rounds=%d,%d", ev.Height, ev.Round1, ev.Round2)
	}
	if ev.Hash1 != h1 || ev.Hash2 != h2 {
		t.Error("wrong hashes in evidence")
	}
	if len(ev.String()) == 0 {
		t.Error("evidence String() must not be empty")
	}
}

func TestSlashing_DifferentSlots(t *testing.T) {
	sd := newSlashingDetector()
	_, pub, _ := crypto.GenerateValidatorKey()
	var h1, h2 crypto.Hash32
	h1[0] = 0xAA
	h2[0] = 0xBB

	// Same height, different rounds is also a double-sign: validators must
	// not make competing proposals for one height.
	sd.RecordAcceptedBlock(3, 0, pub, h1, []byte("sig1"))
	ev := sd.DetectDoubleSign(3, 1, pub, h2, []byte("sig2"))
	if ev == nil {
		t.Fatal("different rounds at one height should trigger double-sign evidence")
	}
	if ev.Round1 != 0 || ev.Round2 != 1 {
		t.Errorf("evidence rounds = %d,%d, want 0,1", ev.Round1, ev.Round2)
	}
}

func TestSlashing_DifferentValidators(t *testing.T) {
	sd := newSlashingDetector()
	_, pub1, _ := crypto.GenerateValidatorKey()
	_, pub2, _ := crypto.GenerateValidatorKey()
	var h1, h2 crypto.Hash32
	h1[0] = 1
	h2[0] = 2

	sd.RecordAcceptedBlock(1, 0, pub1, h1, []byte("sig1"))
	ev := sd.DetectDoubleSign(1, 0, pub2, h2, []byte("sig2"))
	if ev != nil {
		t.Error("different validators in same slot should not trigger evidence")
	}
}

func TestSlashing_InvalidCandidateDoesNotPoisonAcceptedSlot(t *testing.T) {
	sd := newSlashingDetector()
	_, pub, _ := crypto.GenerateValidatorKey()
	var invalid, valid crypto.Hash32
	invalid[0], valid[0] = 1, 2

	// A signed candidate that later fails transaction or state validation is
	// checked but must not be committed to the accepted-slot detector.
	if ev := sd.DetectDoubleSign(4, 0, pub, invalid, []byte("invalid-sig")); ev != nil {
		t.Fatalf("unexpected evidence before first accepted block: %v", ev)
	}
	if ev := sd.DetectDoubleSign(4, 0, pub, valid, []byte("valid-sig")); ev != nil {
		t.Fatalf("invalid candidate poisoned the slot: %s", ev.String())
	}
	if ev := sd.RecordAcceptedBlock(4, 0, pub, valid, []byte("valid-sig")); ev != nil {
		t.Fatalf("valid candidate could not be recorded: %s", ev.String())
	}
	if ev := sd.DetectDoubleSign(4, 0, pub, invalid, []byte("invalid-sig")); ev == nil {
		t.Fatal("conflict with an accepted block was not detected")
	}
}
