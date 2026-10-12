package sale

import (
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// State is constant-sized. Replay identities belong in the transactional store,
// not a ledger map copied in full on every block.
type State struct {
	Checkpoint crypto.Hash32 `json:"checkpoint"`
	Height     uint64        `json:"height"`
	Hash       crypto.Hash32 `json:"hash"`
	Remaining  uint64        `json:"remaining_napro,string"`
	Sold       uint64        `json:"sold_napro,string"`
}

func InitialState(c Checkpoint) State {
	return State{Checkpoint: c.Message(), Height: c.Height - 1, Hash: c.Parent, Remaining: c.SaleRemaining}
}

// Advance requires an already authenticated checkpoint. The caller must commit
// the returned state and both identity fences in the same batch as the outputs.
func (s State) Advance(c Checkpoint, height uint64, parent, hash crypto.Hash32,
	txs []*core.Transaction, consumed func(byte, crypto.Hash32) (bool, error)) (State, error) {
	if s.Checkpoint != c.Message() || s.Height+1 != height || height < c.Height ||
		s.Hash != parent || hash == zeroHash || hash == parent ||
		s.Remaining > c.SaleRemaining || s.Sold != c.SaleRemaining-s.Remaining || consumed == nil {
		return State{}, fmt.Errorf("sale: inconsistent canonical state")
	}
	ledger := &Ledger{checkpoint: c, height: s.Height, hash: s.Hash, remaining: s.Remaining,
		sold: s.Sold, seen: map[crypto.Hash32]struct{}{}, paid: map[crypto.Hash32]struct{}{}}
	for _, tx := range txs {
		a, err := tx.SaleAuthorization()
		if err != nil {
			return State{}, err
		}
		for _, ref := range []struct {
			kind byte
			id   crypto.Hash32
		}{{'o', a.Order}, {'p', a.Payment}} {
			used, err := consumed(ref.kind, ref.id)
			if err != nil {
				return State{}, err
			}
			if used {
				return State{}, fmt.Errorf("sale: durable replay fence consumed")
			}
		}
	}
	next, err := ledger.ApplyCanonicalTransactions(height, parent, hash, txs)
	if err != nil {
		return State{}, err
	}
	return State{Checkpoint: s.Checkpoint, Height: height, Hash: hash, Remaining: next.remaining, Sold: next.sold}, nil
}
