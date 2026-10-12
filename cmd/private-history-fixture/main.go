// Offline cryptographic scanner fixture. This is not production data or an
// end-to-end consensus/issuance proof. No payment or network request is made.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/wallet"
)

type output struct {
	Amount    string `json:"amount_napr,omitempty"`
	Hash      string `json:"tx_hash"`
	Index     int    `json:"out_idx"`
	Pub       string `json:"one_time_pub"`
	TxPub     string `json:"tx_pub_key"`
	Commit    string `json:"amount_commit"`
	Encrypted string `json:"enc_amount"`
	Height    uint64 `json:"block_height"`
}
type evidence struct {
	Height uint64 `json:"block_height"`
	Hash   string `json:"block_hash"`
	Fee    string `json:"fee_napro"`
	Origin string `json:"origin"`
}
type fixture struct {
	Scope      string              `json:"scope"`
	Mnemonic   string              `json:"test_mnemonic"`
	Address    crypto.Address      `json:"address"`
	Snapshot   string              `json:"snapshot_id"`
	Checkpoint string              `json:"checkpoint_hash"`
	Height     uint64              `json:"checkpoint_height"`
	Expected   string              `json:"expected_napro"`
	Outputs    []output            `json:"outputs"`
	Evidence   map[string]evidence `json:"canonical_outputs"`
	Images     []string            `json:"owner_key_images"`
	Bodies     []core.Block        `json:"bodies"`
}

func run() error {
	count := flag.Int("rows", 12000, "owned cryptographic output count (256..12000)")
	path := flag.String("out", "", "temporary output JSON path")
	mainnet := flag.Bool("mainnet", true, "derive the address prefix used by native wallets")
	historical := flag.Int("historical-rows", 0, "synthetic old mint/reward outputs before the sale anchor (0..512)")
	flag.Parse()
	if *count < 256 || *count > 12000 || *path == "" || *historical < 0 || *historical > 512 || *historical >= *count {
		return fmt.Errorf("rows must be 256..12000 and out is required")
	}
	// Public, standardized synthetic mnemonic; never import it with real funds.
	phrase := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	dk, err := wallet.DeriveFromMnemonic(phrase, "", 0, 0)
	if err != nil {
		return err
	}
	if *mainnet {
		dk.Address = crypto.AddressFromKeys(crypto.MainnetByte, dk.Keys)
	}
	network, _, _, err := crypto.DecodeAddress(dk.Address)
	if err != nil {
		return err
	}
	authority, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		return err
	}
	genesis := core.Block{Header: core.BlockHeader{MerkleRoot: core.MerkleRoot(nil)}}
	cp := sale.Checkpoint{Genesis: genesis.Hash(), Parent: genesis.Hash(), Height: uint64(*historical + 1), Network: network,
		SnapshotRoot:  crypto.HashBytes([]byte("synthetic forward anchor; no historical issuance claim")),
		SaleRemaining: 100000 * 100000000, SaleAuthority: pub}
	trusted := []crypto.ValidatorPubKey{}
	authorities := []crypto.ValidatorPrivKey{}
	for i := 0; i < 4; i++ {
		k, p, err := crypto.GenerateValidatorKey()
		if err != nil {
			return err
		}
		trusted = append(trusted, p)
		authorities = append(authorities, k)
	}
	var ledger *sale.Ledger
	f := fixture{Scope: "synthetic signed-body/scanner rehearsal, not network consensus proof", Mnemonic: phrase, Address: dk.Address,
		Snapshot: hex.EncodeToString(cp.SnapshotRoot[:]), Evidence: map[string]evidence{}, Bodies: []core.Block{genesis}}
	scanner := wallet.NewScannerFromDerived(dk)
	parent := genesis.Hash()
	var expected uint64
	for i := 0; i < *count; i++ {
		height := uint64(i + 1)
		amount := uint64(i%7+1) * 100000000
		origin := "protocol_sale"
		var tx *core.Transaction
		if i < *historical {
			amount = 3 * 100000000
			origin = "validator_reward"
			if i < *historical/2 {
				tx, err = core.BuildMintTx(dk.Address, amount, height)
			} else {
				tx, err = core.BuildAuthorizedRewardTx(dk.Address, amount, height, parent, authority)
			}
		} else {
			if ledger == nil {
				cp.Parent, cp.HistoricIssued = parent, expected
				for j := 0; j < 3; j++ {
					sig, e := authorities[j].Sign(cp.Message())
					if e != nil {
						return e
					}
					cp.Attestations = append(cp.Attestations, sale.Attestation{Validator: trusted[j], Signature: sig})
				}
				ledger, err = sale.Begin(cp, cp.Genesis, cp.Parent, cp.Height, trusted)
				if err != nil {
					return err
				}
			}
			tx, err = core.BuildSaleTransaction(core.SaleAuthorization{Genesis: cp.Genesis, Height: height, Parent: parent,
				Order:   crypto.HashBytes([]byte(fmt.Sprintf("fixture-order-%d", i))),
				Payment: crypto.HashBytes([]byte(fmt.Sprintf("fixture-payment-%d", i))), Amount: amount}, dk.Address, cp.Network, authority)
		}
		if err != nil {
			return err
		}
		b := core.Block{Header: core.BlockHeader{Height: height, PrevHash: parent, Timestamp: int64(height) * 3000000000, ValidatorPub: pub}, Txs: []core.Transaction{*tx}}
		b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
		if err = b.Header.Sign(authority); err != nil {
			return err
		}
		if tx.IsSale() {
			ledger, err = ledger.ApplyCanonicalTransactions(height, parent, b.Hash(), []*core.Transaction{tx})
			if err != nil {
				return err
			}
		}
		owned := scanner.ScanBlock(&b)
		legacyAmount := ""
		if i < *historical {
			// The native legacy path takes the historical reward amount hint,
			// then checks its deterministic commitment and height-derived key.
			// The generic stealth scanner deliberately skips these outputs.
			hs := crypto.ScalarFromUint64(height)
			offset, e := crypto.ScalarMulBase(hs)
			if e != nil {
				return e
			}
			expectedPub, e := crypto.AddPoints(dk.Keys.Spend.Public, offset)
			if e != nil {
				return e
			}
			blind, e := crypto.DeterministicMintBlindV2(dk.Keys.Spend.Public, amount, height)
			if e != nil {
				return e
			}
			commit, e := crypto.Commit(amount, blind)
			if e != nil {
				return e
			}
			if tx.Outputs[0].OneTimePub != expectedPub || tx.Outputs[0].AmountCommit != commit {
				return fmt.Errorf("historical opening or ownership failed at %d", height)
			}
			owned = []core.OwnedUTXO{{Amount: amount, HsScalar: hs, Blind: blind}}
			legacyAmount = fmt.Sprint(amount)
		}
		if len(owned) != 1 || owned[0].Amount != amount {
			return fmt.Errorf("scanner did not independently recover row %d", i)
		}
		secret, err := crypto.AddScalars(owned[0].HsScalar, dk.Keys.Spend.Private)
		if err != nil {
			return err
		}
		image, err := crypto.ComputeKeyImage(secret, tx.Outputs[0].OneTimePub)
		if err != nil {
			return err
		}
		f.Images = append(f.Images, hex.EncodeToString(image[:]))
		o := tx.Outputs[0]
		hash := tx.Hash()
		bh := b.Hash()
		f.Outputs = append(f.Outputs, output{Amount: legacyAmount, Hash: hex.EncodeToString(hash[:]), Pub: hex.EncodeToString(o.OneTimePub[:]),
			TxPub: hex.EncodeToString(o.TxPubKey[:]), Commit: hex.EncodeToString(o.AmountCommit[:]),
			Encrypted: hex.EncodeToString(o.EncAmount[:]), Height: height})
		f.Evidence[fmt.Sprintf("%x:0", hash)] = evidence{Height: height, Hash: hex.EncodeToString(bh[:]), Fee: "0", Origin: origin}
		f.Bodies = append(f.Bodies, b)
		parent = bh
		expected += amount
	}
	f.Checkpoint = hex.EncodeToString(parent[:])
	f.Height = uint64(*count)
	f.Expected = fmt.Sprint(expected)
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(*path, raw, 0600)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
