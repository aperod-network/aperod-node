package main

import (
	"errors"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestUTXOMaterialCoverageDoesNotInferNetHoldings(t *testing.T) {
	rows := []*store.StoredUTXO{
		{AmountNAPRO: 100, ProtocolLocked: true},
		{TxPubKey: crypto.Point32{1}, EncAmount: [8]byte{2}},
		{},
	}
	report, err := reviewUTXOMaterial(func(visit func(*store.StoredUTXO) error) error {
		for _, row := range rows {
			if err := visit(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Rows != 3 || report.AmountMetadataPresent != 1 ||
		report.AmountMetadataMissing != 2 || report.EncryptedAmountRows != 1 ||
		report.ZeroTxPublicKeyRows != 2 || report.ProtocolLockedRows != 1 {
		t.Fatalf("incorrect material coverage: %+v", report)
	}
	if report.GlobalBalanceVerified || report.OwnerSpentnessVerified {
		t.Fatal("material metadata was promoted to global holdings or spentness proof")
	}
	if rows[1].AmountNAPRO != 0 {
		t.Fatal("review must not invent or rewrite amounts")
	}
}

func TestUTXOMaterialCoverageFailsWithoutPartialEvidence(t *testing.T) {
	want := errors.New("storage read failed")
	report, err := reviewUTXOMaterial(func(visit func(*store.StoredUTXO) error) error {
		if err := visit(&store.StoredUTXO{AmountNAPRO: 100}); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) || report.Rows != 0 {
		t.Fatalf("partial coverage survived storage failure: %+v, %v", report, err)
	}
	if _, err := reviewUTXOMaterial(func(visit func(*store.StoredUTXO) error) error {
		return visit(nil)
	}); err == nil {
		t.Fatal("nil stored output accepted")
	}
}
