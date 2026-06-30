package core

import (
        "fmt"
        "os"
        "time"

        "github.com/aperod/aperod/crypto"
        "gopkg.in/yaml.v3"
)

// StakingGenesisConfig holds stake-based validator-selection parameters
// read from the genesis YAML staking: section.
type StakingGenesisConfig struct {
        MaxValidators   int    `yaml:"max_validators"`    // hard cap (default 21)
        MinStakeAPR     uint64 `yaml:"min_stake_apr"`     // minimum deposit in whole APRO
        EpochLength     uint64 `yaml:"epoch_length"`      // blocks between set updates
        ChurnLimit      int    `yaml:"churn_limit"`       // max new activations per epoch
        UnbondingBlocks uint64 `yaml:"unbonding_blocks"`  // withdrawal lock duration in blocks
        SlashPercent    int    `yaml:"slash_percent"`     // % slashed for proven double-sign
        GenesisStakeAPR uint64 `yaml:"genesis_stake_apr"` // stake credited to genesis validators
}

// GenesisConfig is the configuration loaded from genesis.yaml.
type GenesisConfig struct {
        ChainID         string               `yaml:"chain_id"`
        Timestamp       int64                `yaml:"timestamp"`        // Unix seconds; 0 = use current time
        InitialSupply   uint64               `yaml:"initial_supply"`   // total APRO (in APRO, not base units)
        BlockTimeMs     int64                `yaml:"block_time_ms"`
        RingSize        int                  `yaml:"ring_size"`
        MinValidators   int                  `yaml:"min_validators"`
        BFTThreshold    float64              `yaml:"bft_threshold"`    // e.g. 0.667
        CheckpointEvery uint64               `yaml:"checkpoint_every"`
        Validators      []string             `yaml:"validators"`       // hex-encoded ED25519 public keys
        Staking         StakingGenesisConfig `yaml:"staking"`
        // Allocations for pre-mine / dev fund (optional)
        Allocations []GenesisAlloc `yaml:"allocations"`
}

// GenesisAlloc is a pre-mine allocation with optional vesting schedule.
type GenesisAlloc struct {
        Address string           `yaml:"address"`           // Aperod address
        Amount  uint64           `yaml:"amount"`            // in base units (APRO × 10^8)
        Label   string           `yaml:"label,omitempty"`   // human-readable name, e.g. "Team & Advisors"
        Vesting *VestingSchedule `yaml:"vesting,omitempty"` // nil means immediate unlock
}

// VestedAmount returns how many base units of this allocation are unlocked at
// `now` (Unix seconds), given the genesis timestamp.
// If Vesting is nil the entire amount is immediately available.
func (a *GenesisAlloc) VestedAmount(now, genesisTime int64) uint64 {
        if a.Vesting == nil {
                return a.Amount
        }
        return a.Vesting.VestedAmount(a.Amount, genesisTime, now)
}

// LockedAmount returns how many base units are still vesting at `now`.
func (a *GenesisAlloc) LockedAmount(now, genesisTime int64) uint64 {
        if a.Vesting == nil {
                return 0
        }
        return a.Vesting.LockedAmount(a.Amount, genesisTime, now)
}

// BaseUnitsPerAPR is the number of base units in one APRO (like satoshi in Bitcoin).
const BaseUnitsPerAPR = 100_000_000

// LoadGenesis reads a genesis YAML file.
func LoadGenesis(path string) (*GenesisConfig, error) {
        data, err := os.ReadFile(path)
        if err != nil {
                return nil, fmt.Errorf("read genesis %s: %w", path, err)
        }
        var g GenesisConfig
        if err := yaml.Unmarshal(data, &g); err != nil {
                return nil, fmt.Errorf("parse genesis %s: %w", path, err)
        }
        if err := g.Validate(); err != nil {
                return nil, fmt.Errorf("invalid genesis: %w", err)
        }
        return &g, nil
}

// Validate checks the genesis config for required fields and sane values.
func (g *GenesisConfig) Validate() error {
        if g.ChainID == "" {
                return fmt.Errorf("chain_id is required")
        }
        if g.InitialSupply == 0 {
                return fmt.Errorf("initial_supply must be > 0")
        }
        if g.MinValidators < 1 {
                return fmt.Errorf("min_validators must be >= 1")
        }
        if g.BFTThreshold <= 0 || g.BFTThreshold > 1 {
                return fmt.Errorf("bft_threshold must be in (0, 1]")
        }
        if g.RingSize < 2 {
                return fmt.Errorf("ring_size must be >= 2")
        }
        if len(g.Validators) < g.MinValidators {
                return fmt.Errorf("genesis has %d validators but min_validators=%d",
                        len(g.Validators), g.MinValidators)
        }
        return nil
}

// CreateGenesisBlock builds the deterministic genesis block from configuration.
// The genesis block has height 0, zero PrevHash, and no real transactions.
func CreateGenesisBlock(g *GenesisConfig, validatorPriv crypto.ValidatorPrivKey) (*Block, error) {
        ts := g.Timestamp
        if ts == 0 {
                ts = time.Now().UTC().Unix()
        }

        // Build coinbase-like allocations as outputs (simplified: no ring signatures).
        // Each allocation with a valid, non-empty address becomes a genesis output.
        // Entries with an empty address are silently skipped (placeholder config).
        var txs []Transaction
        for _, alloc := range g.Allocations {
                if alloc.Address == "" {
                        continue // placeholder — skip until a real address is configured
                }
                _, spendPub, _, decErr := crypto.DecodeAddress(crypto.Address(alloc.Address))
                if decErr != nil {
                        return nil, fmt.Errorf("genesis: invalid allocation address %q: %w", alloc.Address, decErr)
                }
                blind, err := crypto.NewBlindFactor()
                if err != nil {
                        return nil, err
                }
                commit, err := crypto.Commit(alloc.Amount, blind)
                if err != nil {
                        return nil, err
                }
                txs = append(txs, Transaction{
                        Version: TxVersionBase,
                        Outputs: []Output{{
                                OneTimePub:   spendPub,
                                AmountCommit: commit,
                        }},
                })
        }

        root := MerkleRoot(txs)

        header := BlockHeader{
                Height:       0,
                PrevHash:     crypto.Hash32{}, // all zeros
                MerkleRoot:   root,
                Timestamp:    ts * 1e9, // convert to nanoseconds
                Round:        0,
                ValidatorPub: validatorPriv.Public(),
        }
        if err := header.Sign(validatorPriv); err != nil {
                return nil, fmt.Errorf("sign genesis: %w", err)
        }

        return &Block{Header: header, Txs: txs}, nil
}

// GenesisHash returns the deterministic chain ID hash derived from the genesis config.
// Used to prevent cross-chain replay attacks.
func GenesisHash(g *GenesisConfig) crypto.Hash32 {
        return crypto.HashStr("aperod/" + g.ChainID + "/genesis/v1")
}
