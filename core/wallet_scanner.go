package core

import (
	"encoding/binary"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// OwnedUTXO is a UTXO that belongs to the scanning wallet.
type OwnedUTXO struct {
	UTXO
	HsScalar crypto.Scalar32    // H_s(rV): add spend private key to get one-time spend key
	Amount   uint64             // decrypted amount (0 if decryption failed)
	Blind    crypto.BlindFactor // Pedersen blind; zero = unknown for stealth outputs
	// Blind is populated for transparent mint UTXOs via DeterministicMintBlind.
	// For regular stealth outputs it must be supplied from the utxo_blinds DB.
}

// WalletScanner scans blocks for outputs belonging to a wallet.
type WalletScanner struct {
	viewPriv    crypto.Scalar32
	spendPriv   crypto.Scalar32
	hasSpendKey bool
	spendPub    crypto.Point32
	viewPub     crypto.Point32
	networkByte crypto.NetworkByte
}

// NewWalletScanner creates a scanner for a view-only wallet.
func NewWalletScanner(viewPriv crypto.Scalar32, spendPub, viewPub crypto.Point32, net crypto.NetworkByte) *WalletScanner {
	return &WalletScanner{
		viewPriv:    viewPriv,
		spendPub:    spendPub,
		viewPub:     viewPub,
		networkByte: net,
	}
}

// NewWalletScannerWithSpendKey creates a scanner that can recover version-13+
// encrypted amounts, which are encrypted to the output's one-time spend key.
func NewWalletScannerWithSpendKey(spendPriv, viewPriv crypto.Scalar32, spendPub, viewPub crypto.Point32, net crypto.NetworkByte) *WalletScanner {
	s := NewWalletScanner(viewPriv, spendPub, viewPub, net)
	s.spendPriv = spendPriv
	s.hasSpendKey = true
	return s
}

// ScanBlock scans a block for outputs belonging to this wallet.
// Returns a list of owned UTXOs (may be empty if none belong to us).
func (s *WalletScanner) ScanBlock(block *Block) []OwnedUTXO {
	owned, _ := s.ScanBlockVerified(block)
	return owned
}

// ScanBlockVerified scans outputs and returns an error if the scanner cannot
// authenticate a version-13 amount. View-only scanners intentionally cannot
// claim an amount for new-format outputs.
func (s *WalletScanner) ScanBlockVerified(block *Block) ([]OwnedUTXO, error) {
	var owned []OwnedUTXO
	txHash := func(tx Transaction) crypto.Hash32 { return tx.Hash() }

	for _, tx := range block.Txs {
		hash := txHash(tx)
		for i, out := range tx.Outputs {
			hs, err := crypto.ScanForOutput(
				s.viewPriv,
				s.spendPub,
				out.TxPubKey,
				out.OneTimePub,
			)
			if err != nil || hs == nil {
				continue // not ours
			}

			var amount uint64
			if tx.UsesAmountBinding() {
				if !s.hasSpendKey {
					return nil, fmt.Errorf("transaction %x output %d requires a spend key to read its bound amount",
						hash[:8], i)
				}
				valid, verifyErr := crypto.VerifyAmountBinding(out.AmountCommit, out.OneTimePub, out.AmountBinding)
				if verifyErr != nil {
					return nil, fmt.Errorf("transaction %x output %d amount-binding proof: %w",
						hash[:8], i, verifyErr)
				}
				if !valid {
					return nil, fmt.Errorf("transaction %x output %d has an invalid amount-binding proof",
						hash[:8], i)
				}
				oneTimePriv, keyErr := crypto.AddScalars(*hs, s.spendPriv)
				if keyErr != nil {
					return nil, fmt.Errorf("transaction %x output %d one-time private key: %w",
						hash[:8], i, keyErr)
				}
				amount, verifyErr = crypto.DecryptAmountBinding(out.AmountBinding, oneTimePriv)
				crypto.ZeroBytes(oneTimePriv[:])
				if verifyErr != nil {
					return nil, fmt.Errorf("transaction %x output %d bound amount: %w",
						hash[:8], i, verifyErr)
				}
			} else {
				amount = decryptAmount(out.EncAmount, hs)
			}

			owned = append(owned, OwnedUTXO{
				UTXO: UTXO{
					TxHash:       hash,
					OutputIndex:  uint32(i),
					OneTimePub:   out.OneTimePub,
					TxPubKey:     out.TxPubKey,
					AmountCommit: out.AmountCommit,
					EncAmount:    out.EncAmount,
					BlockHeight:  block.Header.Height,
				},
				HsScalar: *hs,
				Amount:   amount,
			})
		}
	}
	return owned, nil
}

// ScanChain scans all blocks in a range for owned UTXOs.
// chain.GetByHeight is called for each height from startHeight to endHeight inclusive.
func (s *WalletScanner) ScanChain(chain *Chain, startHeight, endHeight uint64) []OwnedUTXO {
	all, _ := s.ScanChainVerified(chain, startHeight, endHeight)
	return all
}

// ScanChainVerified scans a range while preserving amount-proof failures.
func (s *WalletScanner) ScanChainVerified(chain *Chain, startHeight, endHeight uint64) ([]OwnedUTXO, error) {
	var all []OwnedUTXO
	for h := startHeight; h <= endHeight; h++ {
		block := chain.GetByHeight(h)
		if block == nil {
			break
		}
		owned, err := s.ScanBlockVerified(block)
		if err != nil {
			return nil, err
		}
		all = append(all, owned...)
	}
	return all, nil
}

// Balance sums the amounts of all owned UTXOs.
func Balance(utxos []OwnedUTXO) uint64 {
	var total uint64
	for _, u := range utxos {
		total += u.Amount
	}
	return total
}

// DecryptAmount XOR-decrypts an 8-byte encrypted amount using H_s as the key stream.
// Used by the API layer when a view key is available to decode on-chain amounts inline.
func DecryptAmount(enc [8]byte, hs *crypto.Scalar32) uint64 {
	return decryptAmount(enc, hs)
}

// decryptAmount XOR-decrypts an 8-byte encrypted amount using H_s as the key stream.
// Encryption: enc = amount_bytes XOR SHA3("amount" || Hs)[0:8].
func decryptAmount(enc [8]byte, hs *crypto.Scalar32) uint64 {
	keyStream := crypto.HashBytes([]byte("aperod/amount-enc/v1"), hs[:])
	var plain [8]byte
	for i := range plain {
		plain[i] = enc[i] ^ keyStream[i]
	}
	// little-endian uint64
	var v uint64
	for i := 7; i >= 0; i-- {
		v = (v << 8) | uint64(plain[i])
	}
	return v
}

// EncryptAmount encrypts an amount for inclusion in an output.
// Used by the transaction builder.
func EncryptAmount(amount uint64, hs *crypto.Scalar32) [8]byte {
	var plain [8]byte
	binary.LittleEndian.PutUint64(plain[:], amount)
	keyStream := crypto.HashBytes([]byte("aperod/amount-enc/v1"), hs[:])
	var enc [8]byte
	for i := range enc {
		enc[i] = plain[i] ^ keyStream[i]
	}
	return enc
}
