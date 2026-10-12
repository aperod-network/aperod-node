package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// The wallet's fee subtraction must use the same wire format as Build.
// A legacy MLSAG quote for a CLSAG payment otherwise creates real change.
func TestVersionedFeeQuote_SendAllLeavesNoChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version core.TxVersion
		inputs  int
		fee     uint64
	}{
		{"legacy-single", core.TxVersionCommitmentBinding, 1, 192000},
		{"clsag-single", core.TxVersionCLSAG, 1, 166350},
		{"clsag-two", core.TxVersionCLSAG, 2, 252750},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatal(err)
			}
			recipient, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatal(err)
			}
			const total = uint64(100 * 100_000_000)
			outputs := make([]core.OwnedUTXO, tc.inputs)
			for i := range outputs {
				hs := crypto.ScalarFromUint64(uint64(i + 1))
				point, err := crypto.ScalarMulBase(hs)
				if err != nil {
					t.Fatal(err)
				}
				pub, err := crypto.AddPoints(keys.Spend.Public, point)
				if err != nil {
					t.Fatal(err)
				}
				outputs[i] = makeOwnedUTXO(t, pub, total/uint64(tc.inputs), byte(i+1))
				outputs[i].HsScalar = hs
			}
			decoys := make([]core.DecoyUTXO, tc.inputs*(crypto.RingSize-1))
			for i := range decoys {
				output := makeOwnedUTXO(t, newPub(t, uint64(i+101)), uint64(i+1), byte(i+101))
				decoys[i] = core.DecoyUTXO{OneTimePub: output.OneTimePub, AmountCommit: output.AmountCommit}
			}
			builder := core.NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, outputs, 50).
				WithVersion(tc.version).WithDecoys(decoys)
			max := builder.MaxSpendable()
			if max.Fee != tc.fee || max.MaxAmount != total-tc.fee || max.InputCount != tc.inputs {
				t.Fatalf("unexpected send-all quote: %+v", max)
			}
			quote := builder.EstimateFeeForAmount(max.MaxAmount)
			if !quote.Sufficient || quote.Fee != max.Fee || uint64(quote.TxSizeBytes)*50 != max.Fee {
				t.Fatalf("fee estimate disagrees with send-all: %+v versus %+v", quote, max)
			}
			result, err := builder.Build(max.MaxAmount,
				crypto.AddressFromKeys(crypto.TestnetByte, recipient),
				crypto.AddressFromKeys(crypto.TestnetByte, keys))
			if err != nil {
				t.Fatal(err)
			}
			if result.TotalFee != quote.Fee || result.ChangeAmount != 0 {
				t.Fatalf("send-all retained funds: fee=%d quoted=%d change=%d",
					result.TotalFee, quote.Fee, result.ChangeAmount)
			}
			if builder.EstimateFeeForAmount(^uint64(0)).Sufficient {
				t.Fatal("overflowing amount was considered affordable")
			}
		})
	}
}
