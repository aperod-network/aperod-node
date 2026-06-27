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

	ev := sd.CheckBlock(1, 0, pub, h1, []byte("sig1"))
	if ev != nil {
		t.Errorf("expected no evidence for first block, got: %s", ev.String())
	}
}

func TestSlashing_SameBlockTwice(t *testing.T) {
	sd := newSlashingDetector()
	_, pub, _ := crypto.GenerateValidatorKey()
	var h1 crypto.Hash32
	h1[0] = 1

	sd.CheckBlock(1, 0, pub, h1, []byte("sig1"))
	ev := sd.CheckBlock(1, 0, pub, h1, []byte("sig1"))
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

	sd.CheckBlock(2, 0, pub, h1, []byte("sig1"))
	ev := sd.CheckBlock(2, 0, pub, h2, []byte("sig2"))
	if ev == nil {
		t.Fatal("expected double-sign evidence")
	}
	if ev.Height != 2 || ev.Round != 0 {
		t.Errorf("wrong slot: height=%d round=%d", ev.Height, ev.Round)
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

	// Same height, different rounds — not a double-sign
	sd.CheckBlock(3, 0, pub, h1, []byte("sig1"))
	ev := sd.CheckBlock(3, 1, pub, h2, []byte("sig2"))
	if ev != nil {
		t.Error("different rounds should not trigger double-sign")
	}
}

func TestSlashing_DifferentValidators(t *testing.T) {
	sd := newSlashingDetector()
	_, pub1, _ := crypto.GenerateValidatorKey()
	_, pub2, _ := crypto.GenerateValidatorKey()
	var h1, h2 crypto.Hash32
	h1[0] = 1
	h2[0] = 2

	sd.CheckBlock(1, 0, pub1, h1, []byte("sig1"))
	ev := sd.CheckBlock(1, 0, pub2, h2, []byte("sig2"))
	if ev != nil {
		t.Error("different validators in same slot should not trigger evidence")
	}
}
