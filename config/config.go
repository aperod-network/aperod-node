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

	// ScanCheckpointInterval is the number of blocks between intermediate
	// checkpoints saved during the startup block scan.  A smaller value means
	// the node can resume from a more recent checkpoint after a crash mid-scan
	// (faster crash recovery) but takes more frequent TakeSnapshot() calls,
	// each of which briefly doubles node RSS.  A larger value reduces peak
	// memory pressure at the cost of replaying more blocks on restart.
	//
	// Default: 50000.  Must be > 0; a value of 0 is replaced by the default
	// at runtime.
	ScanCheckpointInterval uint64 `yaml:"scan_checkpoint_interval"`

	// MaxMissingBlocks is the maximum number of individual blocks that may be
	// absent from the LevelDB store before the startup block scan returns a
	// fatal error.  Isolated store gaps (e.g. a single block lost during a
	// hard-kill) are logged at ERROR level and skipped; the node still starts.
	// If more than this many blocks are missing the node refuses to start so
	// that severe store corruption is caught rather than silently producing a
	// wrong UTXO set.
	//
	// Default: 10.  Set to 0 to use the default.
	MaxMissingBlocks uint64 `yaml:"max_missing_blocks"`
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
	// MemoryLimitBytes, when positive, is passed to runtime/debug.SetMemoryLimit
	// at startup if the GOMEMLIMIT environment variable is not already set.
	// This lets operators running outside of systemd (Docker, bare shell, CI)
	// set a safe memory cap without needing OS-level service configuration.
	// Recommended production value: 5905580032 (≈5.5 GB).
	// Set to 0 (default) to rely solely on the GOMEMLIMIT env var.
	MemoryLimitBytes int64 `yaml:"memory_limit_bytes"`

	// GCPercent sets the Go garbage-collector target percentage via
	// debug.SetGCPercent.  The runtime triggers a GC cycle when the live
	// heap has grown by this percentage since the previous cycle.
	// Lower values → more frequent GC → lower peak RSS, slightly higher CPU.
	// Higher values → less frequent GC → higher peak RSS, lower CPU overhead.
	//
	// Default: 50.  The Go runtime default is 100 (heap may double between
	// collections); 50 halves the maximum heap growth between GC cycles and
	// is appropriate for long-running validator nodes where low memory
	// variance matters more than minimal GC CPU.
	// Set to -1 to disable the GC entirely (not recommended in production).
	// Set to 0 to use the Go runtime default (100).
	GCPercent int `yaml:"gc_percent"`

	// MaxInMemoryBlocks is the sliding-window size of blocks kept in RAM.
	// Blocks older than (tip − MaxInMemoryBlocks) are evicted from the
	// in-memory maps; they remain available on disk via the LevelDB store.
	// 1 000 blocks at 3 s/block ≈ 50 minutes of reorg history.
	//
	// Operators with very low RAM can lower this value; operators that
	// serve large GetBlock requests to many peers can raise it to reduce
	// disk I/O at the cost of higher baseline RSS.
	// Default: 1000.  Set to 0 to use the default.
	MaxInMemoryBlocks uint64 `yaml:"max_in_memory_blocks"`

	// MempoolEvictIntervalSec is the period between background mempool
	// eviction runs.  Each run removes transactions whose TTL has expired
	// (default TTL: 2 h) and enforces the MaxBytes RAM cap.  Lower values
	// keep the mempool tighter at the cost of slightly more CPU; higher
	// values let old transactions linger longer.
	// Default: 300 (5 minutes).  Set to 0 to use the default.
	MempoolEvictIntervalSec uint64 `yaml:"mempool_evict_interval_sec"`
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
	// PeerWhitelist is an optional list of IP addresses and/or CIDR ranges
	// (e.g. "1.2.3.4", "10.0.0.0/8") whose inbound connections are accepted.
	// When non-empty, any inbound TCP connection whose source IP is not covered
	// by an entry is rejected immediately — before any P2P handshake occurs.
	// Outbound dial-outs to bootnodes and discovery peers are not affected.
	// An empty list means all source IPs are allowed (default behaviour).
	PeerWhitelist []string `yaml:"peer_whitelist"`
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
	// BadBlockHeightLead is how many blocks ahead of our current tip a peer's
	// advertised block height must be before it is counted as an out-of-range
	// (wrong-fork) strike.  Larger values tolerate faster-syncing peers but
	// reduce sensitivity to rogue-fork spam.  Default: 1000.
	BadBlockHeightLead uint64 `yaml:"bad_block_height_lead"`
	// BadBlockBanThreshold is the number of out-of-range blocks a peer may send
	// (within the strike TTL window) before it is temporarily banned.
	// Default: 10.
	BadBlockBanThreshold int `yaml:"bad_block_ban_threshold"`
	// BadBlockBanDuration is how long a peer IP is banned after exceeding
	// BadBlockBanThreshold.  Default: 24h.
	BadBlockBanDuration time.Duration `yaml:"bad_block_ban_duration"`
	// BanFile is the path to the JSON file where active P2P bans are
	// persisted across restarts.  When empty, defaults to
	// <data_dir>/p2p_bans.json.  Set to "-" to disable persistence.
	BanFile string `yaml:"ban_file"`
	// WhitelistFile is the path to a JSON file where admin-added peer
	// whitelist entries are persisted across restarts.  When empty,
	// defaults to <data_dir>/p2p_whitelist.json.  Set to "-" to disable
	// persistence (whitelist changes via Admin Panel are lost on restart).
	WhitelistFile string `yaml:"whitelist_file"`
	// KeepaliveInterval is how often the keepalive goroutine sends a MsgPing
	// to each connected peer.  The peer's ReadTimeout (30 s) must never fire
	// due to silence from our side; the interval must therefore be well below
	// ReadTimeout.  Allowed range: [1s, 15s].  Default: 10s (0 = use default).
	// Operators on high-latency links may lower this value; operators on fast
	// LANs may raise it to reduce traffic.
	KeepaliveInterval time.Duration `yaml:"keepalive_interval"`
	// MaxBlockIngestPerSec caps the number of blocks per second that any single
	// peer may deliver to this node.  When a syncing peer pushes blocks faster
	// than this limit the dispatch goroutine sleeps just long enough to restore
	// the token count, creating TCP-level backpressure without dropping blocks.
	// Burst is fixed at the same value (one second worth of tokens).
	// Default: 50.  Set to 0 to disable rate limiting (not recommended on
	// resource-constrained nodes).
	MaxBlockIngestPerSec int `yaml:"max_block_ingest_per_sec"`
	// TxRateBurst is the per-source-IP token-bucket capacity for incoming P2P
	// transactions: up to this many transactions may arrive back-to-back from
	// one IP before throttling kicks in.  Protects block-production latency
	// from slow mempool floods that force constant eviction churn.
	// Default: 50.  Set to 0 to disable tx rate limiting.
	TxRateBurst int `yaml:"tx_rate_burst"`
	// TxRateSustained is the sustained transaction rate (tx/sec) each source
	// IP may maintain once its burst allowance is used up.
	// Only effective when tx_rate_burst > 0.  Default: 10.
	TxRateSustained int `yaml:"tx_rate_sustained"`
	// TxRateBanThreshold is the number of throttled (dropped) transactions
	// after which the flooding IP is temporarily banned.  The counter resets
	// as soon as the peer drops back below the rate limit, so only sustained
	// flooding accumulates toward a ban.  Whitelisted IPs are throttled but
	// never banned.  Default: 100.  Set to 0 to throttle without banning.
	TxRateBanThreshold int `yaml:"tx_rate_ban_threshold"`
	// TxRateBanDuration is how long a tx-flooding IP stays banned after
	// exceeding tx_rate_ban_threshold.  Default: 1h.
	TxRateBanDuration time.Duration `yaml:"tx_rate_ban_duration"`
	// MaxStaleBootnodeAge is the maximum time a bootnode may go without a
	// successful DNS resolution before a WARN is emitted on every discovery
	// tick.  The warning includes the bootnode address and the exact age since
	// last successful resolution so operators can identify and fix stale
	// entries before the peer count silently drops to zero.
	// Default: 24h.  Set to 0 to use the default.
	MaxStaleBootnodeAge time.Duration `yaml:"max_stale_bootnode_age"`
	// GetBlockStallTimeout is how long the node waits for a MsgBlock reply
	// after sending MsgGetBlock before the request is considered stalled.  On
	// stall detection a WARN is logged and MsgGetHeaders is re-issued to
	// restart the sync pipeline from the current tip.
	//
	// Tradeoff: lower values recover from a silently-dropped response faster
	// but may produce false-stall warnings on high-latency or congested links.
	// Higher values are safer on slow networks but delay recovery if a peer
	// drops the response without disconnecting.
	//
	// Default: 15s.  Set to 0 to use the default.
	GetBlockStallTimeout time.Duration `yaml:"get_block_stall_timeout"`
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
	// NonValidator, when true, disables block production on this node. The node
	// still validates and relays all blocks, maintains a full chain copy, and
	// participates in P2P gossip — it simply never proposes or signs blocks.
	// Use this for sync/relay/RPC nodes that should not interfere with consensus.
	// validator_key is still loaded for API identity if specified, but is not
	// passed to the consensus engine.
	NonValidator bool `yaml:"non_validator"`

	// StakingPoolNAPR is the total pre-allocated validator reward pool in nAPRO.
	// When > 0, block rewards are drawn from this pool instead of minting new
	// tokens, keeping Total Supply at 10 B during the pool phase.  After the
	// pool is exhausted, tail_reward_napro is minted per block instead.
	// Default: 200_000_000_000_000_000 (= 2 000 000 000 APRO × 10^8 nAPRO/APRO).
	// Set to 0 to disable pool-based rewards and use the legacy mint schedule.
	StakingPoolNAPR uint64 `yaml:"staking_pool_napro"`

	// TailRewardNAPR is the per-block mint amount in nAPRO once the staking pool
	// is exhausted.  Unlike pool rewards, tail rewards ARE minted (create new
	// supply).  0 uses the default: 100_000_000 nAPRO = 1 APRO/block.
	TailRewardNAPR uint64 `yaml:"tail_reward_napro"`
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
			BadBlockHeightLead:   1000,
			BadBlockBanThreshold: 5,
			BadBlockBanDuration:  24 * time.Hour,
			MaxBlockIngestPerSec: 50,       // cap sync-peer block delivery to prevent CPU spikes
TxRateBurst:          50,       // per-IP tx burst allowance (mempool-flood guard)
TxRateSustained:      10,       // per-IP sustained tx/sec after burst is spent
TxRateBanThreshold:   100,      // sustained violations before a temporary ban
TxRateBanDuration:    time.Hour,
			MaxStaleBootnodeAge:  24 * time.Hour, // warn when a bootnode DNS hasn't resolved for this long
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
			PeriodicSnapshotInterval: 500,
			ScanCheckpointInterval:   50_000,
		},
		Pprof: PprofConfig{
			Enabled:    false,
			ListenAddr: "127.0.0.1:8546",
		},
		MaxInMemoryBlocks:       1_000,
		MempoolEvictIntervalSec: 300,
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
	// Rogue-peer ban safety checks.  These are warnings (not hard failures) because
	// operators may have deliberate reasons to raise the thresholds (e.g. a private
	// test network with a single trusted peer that produces many chain-fork messages).
	// However, values well above the safe defaults effectively disable the rogue-fork
	// ban and leave the node unprotected against adversarial peers.
	const safeBanThreshold = 50
	const safeHeightLead = 10_000
	if c.P2P.BadBlockBanThreshold > safeBanThreshold {
		ws = append(ws, fmt.Sprintf(
			"p2p.bad_block_ban_threshold is %d, which is above the recommended safe maximum of %d — "+
				"a very large threshold effectively disables the rogue-fork ban and allows adversarial peers "+
				"to send wrong-fork blocks indefinitely without being banned; recommended value: 5–10",
			c.P2P.BadBlockBanThreshold, safeBanThreshold,
		))
	}
	if c.P2P.BadBlockHeightLead > safeHeightLead {
		ws = append(ws, fmt.Sprintf(
			"p2p.bad_block_height_lead is %d, which is above the recommended safe maximum of %d — "+
				"a very large height lead reduces sensitivity to rogue-fork spam and may allow adversarial "+
				"peers to avoid ban detection for extended periods; recommended value: 500–2000",
			c.P2P.BadBlockHeightLead, safeHeightLead,
		))
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
	// memory_limit_bytes = 0 means "not set"; negative values panic debug.SetMemoryLimit;
	// values below 512 MiB cause immediate GC thrash before the UTXO set can load.
	const minSafeMemoryLimit int64 = 512 * 1024 * 1024 // 512 MiB
	if c.MemoryLimitBytes < 0 {
		return fmt.Errorf(
			"memory_limit_bytes (%d) must not be negative — use 0 to disable or set a value >= %d (512 MiB)",
			c.MemoryLimitBytes, minSafeMemoryLimit,
		)
	}
	if c.MemoryLimitBytes > 0 && c.MemoryLimitBytes < minSafeMemoryLimit {
		return fmt.Errorf(
			"memory_limit_bytes (%d) is below the safe floor of %d (512 MiB) — "+
				"values this small cause instant GC thrash; set 0 to disable or use >= 512 MiB",
			c.MemoryLimitBytes, minSafeMemoryLimit,
		)
	}
	// Peer whitelist: every entry must be a valid IP address or CIDR range.
	// A single malformed entry would silently fail-open (the entry is ignored
	// at runtime but the operator believes the whitelist is active), so we
	// refuse to start rather than silently degrading to an open network.
	for _, entry := range c.P2P.PeerWhitelist {
		if net.ParseIP(entry) == nil {
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return fmt.Errorf(
					"p2p.peer_whitelist entry %q is not a valid IP address or CIDR range — "+
						"fix or remove this entry; the node will not start with a malformed whitelist",
					entry,
				)
			}
		}
	}

	// MaxBlockIngestPerSec: 0 = disabled (no throttle), positive = cap in
	// blocks/sec, negative = configuration error.
	if c.P2P.MaxBlockIngestPerSec < 0 {
		return fmt.Errorf(
			"p2p.max_block_ingest_per_sec (%d) must not be negative — "+
				"use 0 to disable rate limiting or set a positive value (recommended: 50)",
			c.P2P.MaxBlockIngestPerSec,
		)
	}

	// Rogue-peer ban knob validation.  Zero means "use the built-in default"
	// (applied in p2p.NewHost); only explicit non-default values are validated.
	if c.P2P.BadBlockBanThreshold < 0 {
		return fmt.Errorf(
			"p2p.bad_block_ban_threshold (%d) must not be negative — "+
				"negative values ban peers on the very first strike; use 0 for the default (10)",
			c.P2P.BadBlockBanThreshold,
		)
	}
	if c.P2P.BadBlockBanDuration < 0 {
		return fmt.Errorf(
			"p2p.bad_block_ban_duration (%v) must not be negative — "+
				"negative durations create instantly-expired bans; use 0 for the default (24h)",
			c.P2P.BadBlockBanDuration,
		)
	}
	// A height lead above 1 billion blocks would overflow the uint64 tip+lead
	// comparison in p2p.Host, wrapping to a small value and silently disabling
	// the rogue-fork ban.  Cap at a value that is orders of magnitude above any
	// realistic chain height.
	const maxSafeHeightLead uint64 = 1_000_000_000
	if c.P2P.BadBlockHeightLead > maxSafeHeightLead {
		return fmt.Errorf(
			"p2p.bad_block_height_lead (%d) exceeds the safe maximum of %d — "+
				"values this large can overflow the tip+lead comparison and disable the rogue-fork ban",
			c.P2P.BadBlockHeightLead, maxSafeHeightLead,
		)
	}
	// GetBlockStallTimeout: zero means "use the built-in default (15s)".
	// Negative values would reach p2p.NewHost and cause time.NewTicker to panic
	// on the first connection; reject them at config-load time instead.
	if c.P2P.GetBlockStallTimeout < 0 {
		return fmt.Errorf(
			"p2p.get_block_stall_timeout (%v) must not be negative — "+
				"negative durations panic when the stall-detection ticker is created; "+
				"use 0 for the default (15s) or set a positive value (e.g. \"30s\")",
			c.P2P.GetBlockStallTimeout,
		)
	}

	// KeepaliveInterval validation.  Zero means "use the built-in default (10s)"
	// so it is accepted without complaint.  Explicit values must be in [1s, 15s]:
	//   • < 1s would flood slow peers with pings and may saturate a CPU core.
	//   • > 15s (ReadTimeout/2 = 15s) risks the peer's 30s ReadTimeout firing
	//     during a quiet period before the next ping is sent.
	const keepaliveMin = 1 * time.Second
	const keepaliveMax = 15 * time.Second // ReadTimeout / 2
	if c.P2P.KeepaliveInterval != 0 && (c.P2P.KeepaliveInterval < keepaliveMin || c.P2P.KeepaliveInterval > keepaliveMax) {
		return fmt.Errorf(
			"p2p.keepalive_interval (%v) is out of the allowed range [%v, %v] — "+
				"values shorter than 1s flood peers; values longer than 15s risk the peer's 30s ReadTimeout firing; use 0 for the default (10s)",
			c.P2P.KeepaliveInterval, keepaliveMin, keepaliveMax,
		)
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
