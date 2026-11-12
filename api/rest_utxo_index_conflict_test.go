package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// Exercise the real outpoint endpoint with cryptographically owned outputs,
// not duplicate database descriptions or a mocked successful wallet RPC.
func TestREST_UTXOIndexConflictDistinctOwnedOutputs(t *testing.T) {
	for _, onlyBlindIndex := range []bool{false, true} {
		t.Run(fmt.Sprintf("only_blind_index=%t", onlyBlindIndex), func(t *testing.T) {
			srv, utxos := buildUTXOServer(t)
			keys, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatal(err)
			}
			hash := crypto.Hash32{0x71}
			for index, amount := range []uint64{5_000_000_000, 200_000_000} {
				path := fmt.Sprintf("/api/v1/utxo/%x/%d", hash, index)
				if onlyBlindIndex && index == 0 {
					code, _ := restGet(t, srv, path)
					if code != http.StatusNotFound {
						t.Fatalf("mis-recorded mint index: got %d, want 404", code)
					}
					continue
				}
				output, err := crypto.CreateStealthOutput(keys.Spend.Public, keys.View.Public)
				if err != nil {
					t.Fatal(err)
				}
				var blind crypto.BlindFactor
				blind[0] = byte(index + 11)
				commitment, err := crypto.Commit(amount, blind)
				if err != nil {
					t.Fatal(err)
				}
				owned, err := crypto.ScanForOutput(keys.View.Private, keys.Spend.Public, output.TxPubKey, output.OneTimePub)
				if err != nil || owned == nil || *owned != output.HsScalar {
					t.Fatalf("output %d is not owned by the funded wallet: %v", index, err)
				}
				utxos.Add(&core.UTXO{
					TxHash: hash, OutputIndex: uint32(index), BlockHeight: 1,
					AmountCommit: commitment, OneTimePub: output.OneTimePub,
					TxPubKey: output.TxPubKey, EncAmount: core.EncryptAmount(amount, &output.HsScalar),
				})
				code, body := restGet(t, srv, path)
				if code != http.StatusOK || body["exists"] != true ||
					body["tx_hash"] != fmt.Sprintf("%x", hash) || body["out_idx"] != float64(index) ||
					body["amount_commit_hex"] != fmt.Sprintf("%x", commitment) ||
					body["one_time_pub_hex"] != fmt.Sprintf("%x", output.OneTimePub) {
					t.Fatalf("owned output %d was not independently returned: HTTP %d, %v", index, code, body)
				}
			}
			// Check again after all outputs were added: sharing a transaction
			// hash must not let the second index overwrite the first.
			for index := 0; index < 2; index++ {
				code, _ := restGet(t, srv, fmt.Sprintf("/api/v1/utxo/%x/%d", hash, index))
				want := http.StatusOK
				if onlyBlindIndex && index == 0 {
					want = http.StatusNotFound
				}
				if code != want {
					t.Fatalf("final output %d: HTTP %d, want %d", index, code, want)
				}
			}
		})
	}
}
