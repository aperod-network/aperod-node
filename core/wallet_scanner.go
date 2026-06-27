package core

import (
	"github.com/aperod/aperod/crypto"
)

// OwnedUTXO is a UTXO that belongs to the scanning wallet.
type OwnedUTXO struct {
	UTXO
	HsScalar crypto.Scalar32 // H_s(rV): add spend private key to get one-time spend key
	Amount   uint64          // decrypted amount (0 if decryption failed)
}

// WalletScanner scans blocks for outputs belonging to a wallet.
type WalletScanner struct {
	viewPriv  crypto.Scalar32
	spendPub  crypto.Point32
	viewPub   crypto.Point32
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

// ScanBlock scans a block for outputs belonging to this wallet.
// Returns a list of owned UTXOs (may be empty if none belong to us).
func (s *WalletScanner) ScanBlock(block *Block) []OwnedUTXO {
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

			amount := decryptAmount(out.EncAmount, hs)

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
	return owned
}

// ScanChain scans all blocks in a range for owned UTXOs.
// chain.GetByHeight is called for each height from startHeight to endHeight inclusive.
func (s *WalletScanner) ScanChain(chain *Chain, startHeight, endHeight uint64) []OwnedUTXO {
	var all []OwnedUTXO
	for h := startHeight; h <= endHeight; h++ {
		block := chain.GetByHeight(h)
		if block == nil {
			break
		}
		all = append(all, s.ScanBlock(block)...)
	}
	return all
}

// Balance sums the amounts of all owned UTXOs.
func Balance(utxos []OwnedUTXO) uint64 {
	var total uint64
	for _, u := range utxos {
		total += u.Amount
	}
	return total
}

// decryptAmount XOR-decrypts an 8-byte encrypted amount using H_s as the key stream.
// Encryption: enc = amount_bytes XOR SHA3("amount" || Hs)[0:8]
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
	for i := range plain {
		plain[i] = byte(amount >> (uint(i) * 8))
	}
	keyStream := crypto.HashBytes([]byte("aperod/amount-enc/v1"), hs[:])
	var enc [8]byte
	for i := range enc {
		enc[i] = plain[i] ^ keyStream[i]
	}
	return enc
}
