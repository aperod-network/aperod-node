package config

import (
	"fmt"
	"net"
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

// SnapshotConfig controls snapshot integrity-check behaviour.
type SnapshotConfig struct {
	// UTXOCountTolerancePct is the maximum allowed percentage difference
	// between the snapshot's active UTXO count and the count stored in the
	// DB before the snapshot is rejected and a full block scan is triggered.
	// A value of 1.0 means up to 1 % divergence is tolerated (e.g. due to
	// concurrent writes during shutdown).  Set to 0 for an exact-count match.
	// Default: 1.0.
	UTXOCountTolerancePct float64 `yaml:"utxo_count_tolerance_pct"`

	// PeriodicSnapshotInterval is the number of blocks between periodic
	// in-process UTXO snapshots.  Taking a snapshot creates a full deep copy
	// of the UTXO set and briefly doubles node RSS; on production nodes with a
	// 4-5 GB UTXO set this can push memory past the GOMEMLIMIT ceiling.  A
	// higher value reduces the frequency of those spikes.
	//
	// The shutdown snapshot (taken on clean exit) is unaffected by this
	// setting; it remains the primary crash-recovery mechanism.
	//
	// Default: 10000.  Set to 0 to disable periodic snapshots entirely
	// (shutdown snapshot only).
	PeriodicSnapshotInterval uint64 `yaml:"periodic_snapshot_interval"`
}

// PprofConfig controls the Go pprof HTTP diagnostic endpoint.
// The endpoint exposes CPU/heap/goroutine profiles and should only ever
// bind to localhost (127.0.0.1).  It is disabled by default.
type PprofConfig struct {
	// Enabled turns the pprof HTTP server on.  Default: false.
	Enabled bool `yaml:"enabled"`
	// ListenAddr is the address the pprof server binds to.
	// MUST be a loopback address.  Default: "127.0.0.1:8546".
	ListenAddr string `yaml:"listen_addr"`
}

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
	Snapshot  SnapshotConfig  `yaml:"snapshot"`
	Pprof     PprofConfig     `yaml:"pprof"`
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
	// AllowedPeers is an optional list of hex-encoded SHA-256 SPKI fingerprints
	// that are permitted to join the network.  When non-empty, any peer whose
	// TLS fingerprint is not on the list is disconnected immediately after the
	// TLS handshake.  An empty list means open network (default behaviour).
	AllowedPeers []string `yaml:"allowed_peers"`
	// MaxPendingHandshakes caps the number of inbound connections that are
	// concurrently executing the TLS handshake.  A peer that opens many TCP
	// connections but never sends a ClientHello holds one goroutine each for
	// up to 10 s; this cap limits goroutine exhaustion under a connect-flood.
	// 0 = no limit (not recommended).  Default: 20.
	MaxPendingHandshakes int `yaml:"max_pending_handshakes"`
	// IdentityKey is the path to the node's persistent Ed25519 TLS identity key
	// file.  When empty, defaults to <data_dir>/p2p_identity.key.  The file is
	// created on first start and reused on subsequent starts so the node's TLS
	// fingerprint stays stable across restarts.  Pass --reset-p2p-identity on
	// the command line to force regeneration (e.g. after a key compromise).
	IdentityKey string `yaml:"identity_key"`
}

// ConsensusConfig holds PoA settings.
type ConsensusConfig struct {
	ValidatorKey       string        `yaml:"validator_key"`        // path to ED25519 key file
	ViewKey            string        `yaml:"view_key"`             // hex-encoded Ed25519 view private scalar for automatic UTXO amount decryption (optional)
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
	// Key, when non-empty, requires all write RPC methods (apr_sendRawTransaction
	// etc.) to supply a matching api_key param or X-API-Key header.
	// Empty = dev/open mode (F-5 fix: must be set in production node.yaml).
	Key        string   `yaml:"key"`
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
			ListenAddr:           "/ip4/0.0.0.0/tcp/30303",
			MaxPeers:             50,
			MinPeers:             4,
			MaxPeersPerIP:        3,  // eclipse/partition guard: max 3 connections per source IP
			MinOutbound:          4,  // always keep 4 slots free for outbound dial-outs
			MaxPendingHandshakes: 20, // goroutine-exhaustion guard: cap in-flight TLS handshakes
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
		Snapshot: SnapshotConfig{
			UTXOCountTolerancePct:    1.0,
			PeriodicSnapshotInterval: 10_000,
		},
		Pprof: PprofConfig{
			Enabled:    false,
			ListenAddr: "127.0.0.1:8546",
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

// Warnings returns a list of non-fatal configuration warnings that operators
// should investigate but that do not prevent the node from starting.
func (c *Config) Warnings() []string {
	var ws []string
	if len(c.P2P.AllowedPeers) > 0 && len(c.P2P.Bootnodes) == 0 {
		ws = append(ws, "allowed_peers is set but bootnodes is empty — node may be isolated (no peers to connect to)")
	}
	return ws
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
	if c.Pprof.Enabled {
		addr := c.Pprof.ListenAddr
		if addr == "" {
			addr = "127.0.0.1:8546"
		}
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			return fmt.Errorf("pprof.listen_addr %q is not a valid host:port address: %w", addr, splitErr)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf(
				"pprof.listen_addr %q must bind to a loopback address (127.x.x.x or ::1) — "+
					"binding pprof to a non-loopback interface exposes unauthenticated runtime diagnostics",
				addr,
			)
		}
	}
	return nil
}
