// Package sale is an INACTIVE, deterministic accounting prototype for a future
// consensus-authorized crypto-to-APRO sale protocol. It creates no UTXOs, does
// not validate a block, and does not grant an API permission to accept payments.
// A signed nominal checkpoint is a trust assumption, NOT historical issuance
// proof. The live legacy admin mint must remain disabled.
package sale

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/aperod/aperod/crypto"
)

const CapNAPRO uint64 = 10_000_000_000 * 100_000_000

var zeroHash crypto.Hash32

// Attestation is from one of the authorities in the node's locally configured,
// immutable trusted set. Never load that set from the checkpoint itself.
type Attestation struct {
	Validator crypto.ValidatorPubKey
	Signature []byte
}

// Checkpoint explicitly commits to a *nominal*, authority-attested starting
// state. HistoricIssued and OtherReserved are declarations, not proof derived
// from incomplete pre-activation blocks. SaleRemaining is distinct from the
// explorer's "available allocation" presentation figure.
type Checkpoint struct {
	Genesis        crypto.Hash32
	Network        crypto.NetworkByte
	Height         uint64
	Parent         crypto.Hash32
	SnapshotRoot   crypto.Hash32
	HistoricIssued uint64
	OtherReserved  uint64
	SaleRemaining  uint64
	SaleAuthority  crypto.ValidatorPubKey
	Attestations   []Attestation
}

func u64(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}

func (c Checkpoint) message() crypto.Hash32 {
	return crypto.HashBytes(
		[]byte("aperod/sale/forward-checkpoint/authorization/v1"),
		c.Genesis[:], []byte{byte(c.Network)}, u64(c.Height), c.Parent[:], c.SnapshotRoot[:],
		u64(c.HistoricIssued), u64(c.OtherReserved), u64(c.SaleRemaining),
		[]byte(c.SaleAuthority),
	)
}

// Message lets authorities attest the exact fields under a domain-separated
// signature. Signed data cannot authorize any other checkpoint version.
func (c Checkpoint) Message() crypto.Hash32 { return c.message() }

func nonzeroPub(key crypto.ValidatorPubKey) bool {
	if len(key) != 32 {
		return false
	}
	for _, b := range key {
		if b != 0 {
			return true
		}
	}
	return false
}

func (c Checkpoint) Verify(genesis, parent crypto.Hash32, height uint64, trusted []crypto.ValidatorPubKey) error {
	if height == 0 || c.Height != height || c.Parent != parent || parent == zeroHash ||
		c.Genesis != genesis || genesis == zeroHash || c.SnapshotRoot == zeroHash ||
		!nonzeroPub(c.SaleAuthority) {
		return fmt.Errorf("sale: checkpoint is not bound to the canonical activation parent and authority")
	}
	if c.Network != crypto.MainnetByte && c.Network != crypto.TestnetByte &&
		c.Network != crypto.DevnetByte {
		return fmt.Errorf("sale: unsupported address network")
	}
	if c.SaleRemaining == 0 || c.HistoricIssued > CapNAPRO ||
		c.OtherReserved > CapNAPRO-c.HistoricIssued ||
		c.SaleRemaining > CapNAPRO-c.HistoricIssued-c.OtherReserved {
		return fmt.Errorf("sale: declared nominal budget exceeds the 10B cap")
	}
	allowed := make(map[string]bool, len(trusted))
	for _, key := range trusted {
		if !nonzeroPub(key) || allowed[string(key)] {
			return fmt.Errorf("sale: invalid trusted authority configuration")
		}
		allowed[string(key)] = true
	}
	if len(allowed) == 0 {
		return fmt.Errorf("sale: missing trusted authorities")
	}
	seen := make(map[string]bool, len(c.Attestations))
	for _, a := range c.Attestations {
		key := string(a.Validator)
		if !allowed[key] || seen[key] || !a.Validator.Verify(c.message(), a.Signature) {
			return fmt.Errorf("sale: forged, duplicate, or untrusted checkpoint attestation")
		}
		seen[key] = true
	}
	// Use division to avoid overflow for unusually large untrusted key sets.
	if len(seen) <= 2*len(allowed)/3 {
		return fmt.Errorf("sale: checkpoint requires greater than two-thirds trusted authority signatures")
	}
	return nil
}

// Purchase is a proposed signed purchase commitment, NOT a coinbase. A future
// block validator must additionally verify the exact credited output and
// persist/rollback state atomically with the block before using this ledger.
type Purchase struct {
	Genesis     crypto.Hash32
	Height      uint64
	Parent      crypto.Hash32
	OrderHash   crypto.Hash32
	PaymentHash crypto.Hash32
	Recipient   crypto.Address
	Amount      uint64
	Signature   []byte
}

func (p Purchase) Message() crypto.Hash32 {
	return crypto.HashBytes(
		[]byte("aperod/sale/purchase/authorization/v1"),
		p.Genesis[:], u64(p.Height), p.Parent[:], p.OrderHash[:],
		p.PaymentHash[:], u64(p.Amount), []byte(p.Recipient),
	)
}

// Ledger is immutable to callers; Apply returns a successor or an error,
// leaving the parent intact for reorg rollback. The seen-order index must be
// made durable alongside the canonical block in the eventual integration.
type Ledger struct {
	checkpoint Checkpoint
	height     uint64
	hash       crypto.Hash32
	remaining  uint64
	sold       uint64
	seen       map[crypto.Hash32]struct{}
	paid       map[crypto.Hash32]struct{}
}

func Begin(c Checkpoint, genesis, parent crypto.Hash32, height uint64, trusted []crypto.ValidatorPubKey) (*Ledger, error) {
	if err := c.Verify(genesis, parent, height, trusted); err != nil {
		return nil, err
	}
	// The caller owns the witness and can mutate its byte slices afterwards.
	// Retain only immutable fields and our independent authority copy.
	c.SaleAuthority = append(crypto.ValidatorPubKey(nil), c.SaleAuthority...)
	c.Attestations = nil
	return &Ledger{checkpoint: c, height: height - 1, hash: parent,
		remaining: c.SaleRemaining, seen: make(map[crypto.Hash32]struct{}),
		paid: make(map[crypto.Hash32]struct{})}, nil
}

// ApplyAccountedPurchases checks signatures and the nominal debit. It is NOT
// the validator or a usable mint: no UTXO is created or verified here.
func (s *Ledger) ApplyAccountedPurchases(height uint64, parent, blockHash crypto.Hash32, purchases []Purchase) (*Ledger, error) {
	if s == nil || s.seen == nil || s.paid == nil || height == 0 || height != s.height+1 ||
		parent != s.hash || blockHash == zeroHash || blockHash == parent {
		return nil, fmt.Errorf("sale: missing or noncanonical ledger parent")
	}
	next := &Ledger{checkpoint: s.checkpoint, height: height, hash: blockHash,
		remaining: s.remaining, sold: s.sold,
		seen: make(map[crypto.Hash32]struct{}, len(s.seen)+len(purchases)),
		paid: make(map[crypto.Hash32]struct{}, len(s.paid)+len(purchases))}
	for hash := range s.seen {
		next.seen[hash] = struct{}{}
	}
	for hash := range s.paid {
		next.paid[hash] = struct{}{}
	}
	for _, p := range purchases {
		addressNetwork, _, _, addressErr := crypto.DecodeAddress(p.Recipient)
		if p.Genesis != s.checkpoint.Genesis || p.Height != height || p.Parent != parent ||
			p.OrderHash == zeroHash || p.PaymentHash == zeroHash || p.Amount == 0 ||
			addressErr != nil || addressNetwork != s.checkpoint.Network ||
			!s.checkpoint.SaleAuthority.Verify(p.Message(), p.Signature) {
			return nil, fmt.Errorf("sale: invalid signed purchase commitment")
		}
		if _, exists := next.seen[p.OrderHash]; exists {
			return nil, fmt.Errorf("sale: purchase order already consumed")
		}
		if _, exists := next.paid[p.PaymentHash]; exists {
			return nil, fmt.Errorf("sale: payment already consumed")
		}
		if p.Amount > next.remaining || p.Amount > math.MaxUint64-next.sold {
			return nil, fmt.Errorf("sale: insufficient remaining nominal allocation")
		}
		next.seen[p.OrderHash] = struct{}{}
		next.paid[p.PaymentHash] = struct{}{}
		next.remaining -= p.Amount
		next.sold += p.Amount
	}
	return next, nil
}

// Seen does not expose the mutable backing map.
func (s *Ledger) Seen(order crypto.Hash32) bool {
	if s == nil {
		return false
	}
	_, ok := s.seen[order]
	return ok
}

func (s *Ledger) Remaining() uint64   { return s.remaining }
func (s *Ledger) Sold() uint64        { return s.sold }
func (s *Ledger) Height() uint64      { return s.height }
func (s *Ledger) Hash() crypto.Hash32 { return s.hash }
