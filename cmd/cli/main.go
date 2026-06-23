// Command cli is the Aperod command-line interface.
// Provides: node management, wallet operations, transaction sending, chain inspection.
package main

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/aperod/aperod/crypto"
	"github.com/spf13/cobra"
)

var (
	flagConfig  string
	flagNetwork string
	flagLogLevel string
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "aperod",
	Short: "Aperod blockchain CLI",
	Long: `Aperod (APR) — A privacy-focused blockchain with RingCT transactions.

Commands:
  node      — Start and manage the Aperod node
  wallet    — Create and manage wallets
  tx        — Create and broadcast transactions
  chain     — Inspect the blockchain`,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "config/testnet.yaml", "Config file path")
	rootCmd.PersistentFlags().StringVar(&flagNetwork, "network", "testnet", "Network: mainnet|testnet|devnet")
	rootCmd.PersistentFlags().StringVar(&flagLogLevel, "log-level", "info", "Log level: debug|info|warn|error")

	rootCmd.AddCommand(nodeCmd)
	rootCmd.AddCommand(walletCmd)
	rootCmd.AddCommand(txCmd)
	rootCmd.AddCommand(chainCmd)
}

// ─── node ─────────────────────────────────────────────────────────────────────

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Node management commands",
}

var nodeStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Aperod node",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("Starting Aperod node (config: %s, network: %s)\n", flagConfig, flagNetwork)
		fmt.Println("Tip: run 'aperod-node --config <path>' directly for the full node process.")
		return nil
	},
}

var nodeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show node status",
	RunE: func(cmd *cobra.Command, args []string) error {
		rpcAddr, _ := cmd.Flags().GetString("rpc")
		fmt.Printf("Querying node at %s...\n", rpcAddr)
		// TODO Phase 2: call JSON-RPC apr_getNodeInfo
		fmt.Println(`{
  "status": "running",
  "network": "testnet",
  "height": 0,
  "peers": 0,
  "validators": 0,
  "tps": 0
}`)
		return nil
	},
}

func init() {
	nodeStatusCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
	nodeCmd.AddCommand(nodeStartCmd, nodeStatusCmd)
}

// ─── wallet ───────────────────────────────────────────────────────────────────

var walletCmd = &cobra.Command{
	Use:   "wallet",
	Short: "Wallet management commands",
}

var walletCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Generate a new wallet key pair",
	RunE: func(cmd *cobra.Command, args []string) error {
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			return fmt.Errorf("generate wallet: %w", err)
		}

		net := crypto.TestnetByte
		if flagNetwork == "mainnet" {
			net = crypto.MainnetByte
		}

		addr := crypto.AddressFromKeys(net, keys)

		outFile, _ := cmd.Flags().GetString("output")

		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Println("  APEROD WALLET CREATED — KEEP YOUR KEYS SECRET!")
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Printf("  Address:       %s\n", addr)
		fmt.Printf("  Spend Private: %s\n", hex.EncodeToString(keys.Spend.Private[:]))
		fmt.Printf("  Spend Public:  %s\n", hex.EncodeToString(keys.Spend.Public[:]))
		fmt.Printf("  View Private:  %s\n", hex.EncodeToString(keys.View.Private[:]))
		fmt.Printf("  View Public:   %s\n", hex.EncodeToString(keys.View.Public[:]))
		fmt.Println("═══════════════════════════════════════════════════════════")
		fmt.Println("  WARNING: Write down your keys. There is no recovery!")
		fmt.Println("═══════════════════════════════════════════════════════════")

		if outFile != "" {
			keyJSON := fmt.Sprintf(`{
  "address": %q,
  "spend_private": %q,
  "spend_public": %q,
  "view_private": %q,
  "view_public": %q,
  "network": %q
}`, addr,
				hex.EncodeToString(keys.Spend.Private[:]),
				hex.EncodeToString(keys.Spend.Public[:]),
				hex.EncodeToString(keys.View.Private[:]),
				hex.EncodeToString(keys.View.Public[:]),
				flagNetwork,
			)
			if err := os.WriteFile(outFile, []byte(keyJSON), 0600); err != nil {
				return fmt.Errorf("write keyfile: %w", err)
			}
			fmt.Printf("\n  Saved to: %s\n", outFile)
		}

		return nil
	},
}

var walletBalanceCmd = &cobra.Command{
	Use:   "balance [address]",
	Short: "Check wallet balance (requires view key for full scan)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		addr := args[0]
		rpc, _ := cmd.Flags().GetString("rpc")
		viewKey, _ := cmd.Flags().GetString("view-key")

		fmt.Printf("Scanning for UTXOs belonging to %s...\n", addr)
		fmt.Printf("RPC: %s | View key: %s\n", rpc, viewKey)
		// TODO Phase 2: call apr_getBalance with view key
		fmt.Println("Balance: 0.00000000 APR  (0 confirmed outputs)")
		return nil
	},
}

var walletValidateCmd = &cobra.Command{
	Use:   "validate [address]",
	Short: "Validate an Aperod address",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		addr := crypto.Address(args[0])
		if err := crypto.Validate(addr); err != nil {
			fmt.Printf("INVALID: %v\n", err)
			os.Exit(1)
		}
		net, spend, view, _ := crypto.DecodeAddress(addr)
		fmt.Printf("VALID\n")
		fmt.Printf("  Network:      0x%02x\n", net)
		fmt.Printf("  Spend public: %s\n", hex.EncodeToString(spend[:]))
		fmt.Printf("  View public:  %s\n", hex.EncodeToString(view[:]))
		return nil
	},
}

func init() {
	walletCreateCmd.Flags().String("output", "", "Save keystore to file (JSON)")
	walletBalanceCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
	walletBalanceCmd.Flags().String("view-key", "", "View private key (hex) for scanning")
	walletCmd.AddCommand(walletCreateCmd, walletBalanceCmd, walletValidateCmd)
}

// ─── tx ───────────────────────────────────────────────────────────────────────

var txCmd = &cobra.Command{
	Use:   "tx",
	Short: "Transaction commands",
}

var txSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send APR to an address",
	RunE: func(cmd *cobra.Command, args []string) error {
		to, _ := cmd.Flags().GetString("to")
		amount, _ := cmd.Flags().GetFloat64("amount")
		rpc, _ := cmd.Flags().GetString("rpc")
		keyFile, _ := cmd.Flags().GetString("key-file")

		if to == "" {
			return fmt.Errorf("--to is required")
		}
		if err := crypto.Validate(crypto.Address(to)); err != nil {
			return fmt.Errorf("invalid recipient address: %w", err)
		}
		if amount <= 0 {
			return fmt.Errorf("--amount must be positive")
		}

		fmt.Printf("Sending %.8f APR → %s\n", amount, to)
		fmt.Printf("RPC: %s | Key: %s\n", rpc, keyFile)
		// TODO Phase 1.4 / Phase 2: build and broadcast RingCT transaction
		fmt.Println("Transaction submitted. Hash: 0x0000...0000 (placeholder)")
		return nil
	},
}

func init() {
	txSendCmd.Flags().String("to", "", "Recipient Aperod address")
	txSendCmd.Flags().Float64("amount", 0, "Amount in APR")
	txSendCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
	txSendCmd.Flags().String("key-file", "", "Wallet keystore file")
	txCmd.AddCommand(txSendCmd)
}

// ─── chain ────────────────────────────────────────────────────────────────────

var chainCmd = &cobra.Command{
	Use:   "chain",
	Short: "Chain inspection commands",
}

var chainInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show chain information",
	RunE: func(cmd *cobra.Command, args []string) error {
		rpc, _ := cmd.Flags().GetString("rpc")
		fmt.Printf("Chain info from %s:\n", rpc)
		// TODO Phase 2: call apr_getNodeInfo
		fmt.Println(`{
  "chain_id": "aperod-testnet-1",
  "height": 0,
  "genesis_hash": "0x0000...0000",
  "validators": [],
  "tps_1m": 0,
  "tps_10m": 0
}`)
		return nil
	},
}

func init() {
	chainInfoCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
	chainCmd.AddCommand(chainInfoCmd)
}
