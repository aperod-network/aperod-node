package store

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"filippo.io/edwards25519"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/aperod/aperod/crypto"
)

// SaleRecovery is an owner-signed acknowledgement of a particular canonical
// delivery. It is not consensus issuance and becomes stale after a reorg.
type SaleRecovery struct {
	Block       crypto.Hash32 `json:"block"`
	Transaction crypto.Hash32 `json:"transaction"`
	Signature   string        `json:"signature"`
}

func SaleRecoveryChallenge(genesis crypto.Hash32, order *SaleOrder, delivery *SaleDelivery) string {
	return "APRO_SALE_RECOVERY_V1\x00" + hex.EncodeToString(genesis[:]) + ":" +
		hex.EncodeToString(order.Checkpoint[:]) + ":" + hex.EncodeToString(order.Order[:]) + ":" +
		hex.EncodeToString(delivery.Block[:]) + ":" + hex.EncodeToString(delivery.Transaction[:]) + ":" +
		strconv.FormatUint(order.Amount, 10)
}

func validSaleRecoveryProof(addr crypto.Address, challenge, signature string) bool {
	if len(signature) != 128 {
		return false
	}
	raw, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	_, spend, _, err := crypto.DecodeAddress(addr)
	if err != nil {
		return false
	}
	var nonce crypto.Point32
	copy(nonce[:], raw[:32])
	if err := crypto.ValidateAuthorizedOutputPoints(spend, nonce); err != nil {
		return false
	}
	P, err := new(edwards25519.Point).SetBytes(spend[:])
	if err != nil {
		return false
	}
	R, err := new(edwards25519.Point).SetBytes(nonce[:])
	if err != nil {
		return false
	}
	s, err := new(edwards25519.Scalar).SetCanonicalBytes(raw[32:])
	if err != nil {
		return false
	}
	h := sha512.New()
	h.Write([]byte("APRO_WEB_AUTH_CHALLENGE_V1\x00"))
	h.Write([]byte(challenge))
	h.Write(spend[:])
	h.Write(nonce[:])
	c, err := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	if err != nil {
		return false
	}
	lhs := new(edwards25519.Point).ScalarBaseMult(s)
	rhs := new(edwards25519.Point).Add(R, new(edwards25519.Point).ScalarMult(c, P))
	return lhs.Equal(rhs) == 1
}

func saleRecoveryKey(id crypto.Hash32) []byte {
	return append([]byte("sale/recovery/"), id[:]...)
}

// RecordSaleRecovery checks the canonical fence under the same lock used by
// block commit and rollback. A signed old receipt cannot acknowledge a new
// delivery even if that order later returns to the pending queue.
func (d *DB) RecordSaleRecovery(genesis, id, block, tx crypto.Hash32, signature string) error {
	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()
	raw, err := d.get(saleOrderKey(id))
	if err != nil || raw == nil {
		return fmt.Errorf("sale: order unavailable: %v", err)
	}
	var order SaleOrder
	if err := json.Unmarshal(raw, &order); err != nil {
		return err
	}
	if order.Payment == (crypto.Hash32{}) {
		return fmt.Errorf("sale: payment not confirmed")
	}
	delivery, err := d.saleOrderDeliveryLocked(id)
	if err != nil || delivery == nil || delivery.Block != block || delivery.Transaction != tx {
		return fmt.Errorf("sale: canonical delivery mismatch: %v", err)
	}
	if !validSaleRecoveryProof(order.Recipient, SaleRecoveryChallenge(genesis, &order, delivery), signature) {
		return fmt.Errorf("sale: invalid owner recovery signature")
	}
	encoded, err := json.Marshal(SaleRecovery{Block: block, Transaction: tx, Signature: signature})
	if err != nil {
		return err
	}
	return d.db.Put(saleRecoveryKey(id), encoded, &opt.WriteOptions{Sync: true})
}

func (d *DB) SaleOrderRecovery(id crypto.Hash32) (*SaleRecovery, error) {
	raw, err := d.get(saleRecoveryKey(id))
	if err != nil || raw == nil {
		return nil, err
	}
	var result SaleRecovery
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
