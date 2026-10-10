// Package wallet — high-level wallet transaction builder.
//
// Builder ties together the HD-wallet Scanner (view key) and the core
// TxBuilder (spend key) to produce a fully-signed RingCT transaction ready
// for broadcast to an Aperod node.
//
// Typical flow:
//
//      dk, _ := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
//      b, _ := wallet.NewBuilder(dk, chain)
//      result, _ := b.Send(1_000_000, recipientAddr)
//      // result.Tx is a signed core.Transaction — broadcast via API.
package wallet

import (
        "encoding/json"
        "fmt"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// DefaultFeePerByte is the baseline fee rate used when no custom rate is set.
// 1 base unit per byte is the testnet minimum.
const DefaultFeePerByte uint64 = 1

// Broadcaster is an optional hook the Builder calls after signing a transaction.
// Implementations can POST to the JSON-RPC endpoint or any other transport.
type Broadcaster interface {
        Broadcast(tx *core.Transaction) error
}

// Builder assembles and optionally broadcasts signed RingCT transactions.
// It owns a Scanner to discover owned UTXOs and a DerivedKeys bundle for
// spend-key signing.
type Builder struct {
        dk      *DerivedKeys
        scanner *Scanner
        chain   *core.Chain

        feePerByte  uint64
        broadcaster Broadcaster
}

// BuildResult wraps the signed transaction and accounting summary.
type BuildResult struct {
        Tx           core.Transaction `json:"tx"`
        ChangeAmount uint64           `json:"change_amount"`
        TotalFee     uint64           `json:"total_fee"`
        InputCount   int              `json:"input_count"`
        OutputCount  int              `json:"output_count"`
}

// MarshalJSON serialises BuildResult to JSON (for API / CLI use).
func (r *BuildResult) MarshalJSON() ([]byte, error) {
        type Alias BuildResult
        return json.Marshal((*Alias)(r))
}

// Option is a functional option for Builder.
type Option func(*Builder)

// WithFeePerByte overrides the default fee rate (base units per byte).
func WithFeePerByte(rate uint64) Option {
        return func(b *Builder) {
                if rate > 0 {
                        b.feePerByte = rate
                }
        }
}

// WithBroadcaster sets a Broadcaster that is called automatically by Send().
func WithBroadcaster(br Broadcaster) Option {
        return func(b *Builder) { b.broadcaster = br }
}

// NewBuilder creates a Builder for the given HD-wallet keys and chain.
// The chain is used by the Scanner to discover owned UTXOs.
func NewBuilder(dk *DerivedKeys, chain *core.Chain, opts ...Option) (*Builder, error) {
        if dk == nil {
                return nil, fmt.Errorf("builder: DerivedKeys must not be nil")
        }
        if chain == nil {
                return nil, fmt.Errorf("builder: Chain must not be nil")
        }
        scanner := NewScannerFromDerived(dk)
        b := &Builder{
                dk:         dk,
                scanner:    scanner,
                chain:      chain,
                feePerByte: DefaultFeePerByte,
        }
        for _, o := range opts {
                o(b)
        }
        return b, nil
}

// Balance scans the chain for owned UTXOs and returns the total spendable
// balance in base units together with the individual UTXOs.
func (b *Builder) Balance() (balance uint64, owned []core.OwnedUTXO, err error) {
        balance, owned = b.scanner.Balance(b.chain)
        return
}

// Address returns the Aperod address derived from the wallet keys.
func (b *Builder) Address() crypto.Address {
        return b.scanner.Address()
}

// Send builds, signs and (if a Broadcaster is configured) broadcasts a
// RingCT payment of amount base units to recipient.
//
// changeAddr defaults to the sender's own address when left empty.
func (b *Builder) Send(amount uint64, recipient crypto.Address, changeAddr ...crypto.Address) (*BuildResult, error) {
        // ── Resolve change address ─────────────────────────────────────────────
        var change crypto.Address
        if len(changeAddr) > 0 && changeAddr[0] != "" {
                change = changeAddr[0]
        } else {
                change = b.scanner.Address()
        }

        // ── Scan for spendable UTXOs ───────────────────────────────────────────
        _, owned, err := b.Balance()
        if err != nil {
                return nil, fmt.Errorf("builder.Send: balance scan: %w", err)
        }
        spendable := SpendableUTXOs(owned)
        if len(spendable) == 0 {
                return nil, fmt.Errorf("builder.Send: no spendable UTXOs")
        }

        // ── Delegate to core.TxBuilder ────────────────────────────────────────
        tb := core.NewTxBuilder(
                b.dk.Keys.Spend.Private,
                b.dk.Keys.View.Private,
                b.dk.Keys.Spend.Public,
                spendable,
                b.feePerByte,
        )
        coreResult, err := tb.Build(amount, recipient, change)
        if err != nil {
                return nil, fmt.Errorf("builder.Send: build tx: %w", err)
        }

        result := &BuildResult{
                Tx:           coreResult.Tx,
                ChangeAmount: coreResult.ChangeAmount,
                TotalFee:     coreResult.TotalFee,
                InputCount:   coreResult.InputCount,
                OutputCount:  coreResult.OutputCount,
        }

        // ── Optional broadcast ─────────────────────────────────────────────────
        if b.broadcaster != nil {
                if err := b.broadcaster.Broadcast(&result.Tx); err != nil {
                        return nil, fmt.Errorf("builder.Send: broadcast: %w", err)
                }
        }

        return result, nil
}

// EstimateFee returns the estimated fee in base units for a payment that
// requires nInputs ring inputs and nOutputs outputs, at the configured rate.
// Useful for showing a fee preview in the UI before building the transaction.
func (b *Builder) EstimateFee(nInputs, nOutputs int) uint64 {
        return core.ExportedEstimateFee(nInputs, nOutputs, b.feePerByte)
}
