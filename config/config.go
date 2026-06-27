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
}

// P2PConfig holds networking settings.
type P2PConfig struct {
	ListenAddr string   `yaml:"listen_addr"` // e.g. "/ip4/0.0.0.0/tcp/30303"
	Bootnodes  []string `yaml:"bootnodes"`
	MaxPeers   int      `yaml:"max_peers"`
	MinPeers   int      `yaml:"min_peers"`
}

// ConsensusConfig holds PoA settings.
type ConsensusConfig struct {
	ValidatorKey string        `yaml:"validator_key"` // path to ED25519 key file
	BlockTime    time.Duration `yaml:"block_time"`
}

// APIConfig holds RPC/REST settings.
type APIConfig struct {
	Enabled  bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	CORS     []string `yaml:"cors"`
}

// GenesisRef points to the genesis file.
type GenesisRef struct {
	File string `yaml:"file"`
}

// DefaultConfig returns sensible defaults for a testnet node.
func DefaultConfig() *Config {
	return &Config{
		Network:  Testnet,
		DataDir:  "./data",
		LogLevel: "info",
		P2P: P2PConfig{
			ListenAddr: "/ip4/0.0.0.0/tcp/30303",
			MaxPeers:   50,
			MinPeers:   4,
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
	return nil
}
