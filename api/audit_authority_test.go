package api

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestAuditHeaderAuthorityEraAndRetiredKeyRejection(t *testing.T) {
	old, oldPub, err := crypto.GenerateValidatorKey()
	if err != nil { t.Fatal(err) }
	next, nextPub, err := crypto.GenerateValidatorKey()
	if err != nil { t.Fatal(err) }
	rule := &core.ValidatorAuthorityFork{
		Version: 1, Height: 5, Identity: oldPub, Replacement: nextPub,
		Genesis: crypto.HashBytes([]byte("genesis")), Parent: crypto.HashBytes([]byte("parent")),
		RegistrySHA256: crypto.HashBytes([]byte("registry")),
	}
	rule.Possession, err = next.Sign(rule.Message())
	if err != nil { t.Fatal(err) }
	if err := rule.Validate(); err != nil { t.Fatal(err) }
	s := &Server{validatorAuthorityFork: rule.Clone()}
	for _, height := range []uint64{4, 5, 6} {
		for _, signing := range []struct{ key crypto.ValidatorPrivKey; replacement bool }{
			{old, false}, {next, true},
		} {
			header := core.BlockHeader{Height: height, ValidatorPub: oldPub, PrevHash: rule.Parent}
			header.Signature, err = signing.key.Sign(header.Hash())
			if err != nil { t.Fatal(err) }
			want := signing.replacement == (height >= rule.Height)
			if got := s.verifyAuditHeader(&header); got != want {
				t.Fatalf("height=%d replacement=%v got=%v want=%v", height, signing.replacement, got, want)
			}
			if height == rule.Height && want {
				header.PrevHash = crypto.HashBytes([]byte("wrong-parent"))
				header.Signature, _ = next.Sign(header.Hash())
				if s.verifyAuditHeader(&header) { t.Fatal("accepted wrong activation parent") }
			}
		}
	}
	if err := s.SetAuditValidatorAuthorityFork(&core.ValidatorAuthorityFork{}); err == nil {
		t.Fatal("accepted unvalidated authority rule")
	}
}