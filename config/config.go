package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Network identifies which chain this node is on.
type Network string

const (
	Mainnet Network = "mainnet"
	Testnet Network = "testnet"
	Devnet  Network = "devnet"
)

// Config is the top-level node configuration.
type Config struct {
	Network   Network         `yaml:"network"`
	DataDir   string          `yaml:"data_dir"`
	LogLevel  string          `yaml:"log_level"`
	P2P       P2PConfig       `yaml:"p2p"`
	Consensus ConsensusConfig `yaml:"consensus"`
	API       APIConfig       `yaml:"api"`
	Genesis   GenesisRef      `yaml:"genesis"`
	Pruning   PruningConfig   `yaml:"pruning"`
}

// P2PConfig holds networking settings.
// Bootnodes may be specified as plain "host:port" addresses where host is
// either an IPv4/IPv6 literal or a DNS hostname.  DNS names are resolved
// at connect-time so that node operators can rotate IPs without changing
// their configuration file.
type P2PConfig struct {
	ListenAddr    string   `yaml:"listen_addr"`      // e.g. "/ip4/0.0.0.0/tcp/30303"
	Bootnodes     []string `yaml:"bootnodes"`        // "domain:port" or "ip:port"
	MaxPeers      int      `yaml:"max_peers"`
	MinPeers      int      `yaml:"min_peers"`
	MaxPeersPerIP int      `yaml:"max_peers_per_ip"` // max inbound connections per source IP (0 = unlimited, recommended: 3)
	// MinOutbound reserves this many peer slots exclusively for outbound dial-outs.
	// Inbound connections are capped at (MaxPeers − MinOutbound) so a validator
	// under an inbound flood can always gossip produced blocks to the network.
	// Recommended: 4.  0 = feature disabled.
	MinOutbound int `yaml:"min_outbound"`
}

// ConsensusConfig holds PoA settings.
type ConsensusConfig struct {
	ValidatorKey       string        `yaml:"validator_key"`        // path to ED25519 key file
	RewardAddress      string        `yaml:"reward_address"`       // APRO wallet address for block rewards
	BlockRewardNAPR    uint64        `yaml:"block_reward_napro"`   // reward in nAPRO; mainnet: 500_000_000 = 5 APRO; testnet default: 10_000_000 = 0.1 APRO. See deploy/BURN_POLICY.md.
	BlockTime          time.Duration `yaml:"block_time"`
	OracleURL          string        `yaml:"oracle_url"`           // HTTP endpoint returning {"price_usd": <float>}; empty = skip
	OracleMaxDeviation float64       `yaml:"oracle_max_deviation"` // max fractional price deviation (e.g. 0.05 = 5%); 0 = disabled
}

// APIConfig holds RPC/REST settings.
type APIConfig struct {
	Enabled    bool     `yaml:"enabled"`
	ListenAddr string   `yaml:"listen_addr"`
	CORS       []string `yaml:"cors"`
}

// GenesisRef points to the genesis file.
type GenesisRef struct {
	File string `yaml:"file"`
}

// PruningConfig controls how much historical block data is kept on disk.
type PruningConfig struct {
	// Mode is either "archive" (keep all data forever) or "light" (strip old tx
	// data, keeping only block headers + height index).  Default: "archive".
	Mode string `yaml:"mode"`
	// KeepBlocks is the number of recent blocks whose full transaction data is
	// retained.  Blocks older than (tip − KeepBlocks) have their TxData erased.
	// Only effective in "light" mode.  Default: 100_000 (~3.5 days at 3 s/block).
	KeepBlocks uint64 `yaml:"keep_blocks"`
}

// DefaultConfig returns sensible defaults for a testnet node.
func DefaultConfig() *Config {
	return &Config{
		Network:  Testnet,
		DataDir:  "./data",
		LogLevel: "info",
		P2P: P2PConfig{
			ListenAddr:    "/ip4/0.0.0.0/tcp/30303",
			MaxPeers:      50,
			MinPeers:      4,
			MaxPeersPerIP: 3, // eclipse/partition guard: max 3 connections per source IP
			MinOutbound:   4, // always keep 4 slots free for outbound dial-outs
		},
		Consensus: ConsensusConfig{
			BlockTime: time.Second,
		},
		API: APIConfig{
			Enabled:    true,
			ListenAddr: "127.0.0.1:8545",
		},
		Genesis: GenesisRef{
			File: "config/genesis-testnet.yaml",
		},
		Pruning: PruningConfig{
			Mode:       "archive",
			KeepBlocks: 100_000,
		},
	}
}

// Load reads a YAML config file and overlays environment variables.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Environment overrides
	if v := os.Getenv("APEROD_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("APEROD_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("APEROD_P2P_ADDR"); v != "" {
		cfg.P2P.ListenAddr = v
	}
	if v := os.Getenv("APEROD_VALIDATOR_KEY"); v != "" {
		cfg.Consensus.ValidatorKey = v
	}
	if v := os.Getenv("APEROD_REWARD_ADDRESS"); v != "" {
		cfg.Consensus.RewardAddress = v
	}

	return cfg, nil
}

// Validate checks that required fields are present and valid.
func (c *Config) Validate() error {
	if c.Network == "" {
		return fmt.Errorf("network must be set")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must be set")
	}
	if c.P2P.MaxPeers < c.P2P.MinPeers {
		return fmt.Errorf("max_peers (%d) must be >= min_peers (%d)", c.P2P.MaxPeers, c.P2P.MinPeers)
	}
	if c.Consensus.BlockTime <= 0 {
		return fmt.Errorf("block_time must be positive")
	}
	if c.Pruning.Mode != "" && c.Pruning.Mode != "archive" && c.Pruning.Mode != "light" {
		return fmt.Errorf("pruning.mode must be \"archive\" or \"light\", got %q", c.Pruning.Mode)
	}
	return nil
}
