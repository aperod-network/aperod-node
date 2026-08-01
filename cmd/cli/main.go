// Command cli is the Aperod command-line interface.
// Provides: node management, wallet operations, transaction sending, chain inspection.
package main

import (
        "bytes"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "os"
        "strings"
        "syscall"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
        "github.com/aperod/aperod/wallet"
        "github.com/spf13/cobra"
        "golang.org/x/term"
)

// ─── JSON-RPC helper ──────────────────────────────────────────────────────────

// rpcCall makes a JSON-RPC 2.0 call to the node and returns the raw result
// bytes (or an error extracted from the response).
func rpcCall(endpoint, method string, params interface{}) (json.RawMessage, error) {
        body, err := json.Marshal(map[string]interface{}{
                "jsonrpc": "2.0",
                "id":      1,
                "method":  method,
                "params":  params,
        })
        if err != nil {
                return nil, err
        }
        resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body)) //nolint:noctx
        if err != nil {
                return nil, fmt.Errorf("RPC request failed: %w", err)
        }
        defer resp.Body.Close()
        raw, err := io.ReadAll(resp.Body)
        if err != nil {
                return nil, fmt.Errorf("RPC read body: %w", err)
        }
        var rr struct {
                Result json.RawMessage `json:"result"`
                Error  *struct {
                        Message string `json:"message"`
                } `json:"error"`
        }
        if err := json.Unmarshal(raw, &rr); err != nil {
                return nil, fmt.Errorf("RPC decode response: %w", err)
        }
        if rr.Error != nil {
                return nil, fmt.Errorf("RPC error: %s", rr.Error.Message)
        }
        return rr.Result, nil
}

var (
        flagConfig   string
        flagNetwork  string
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
        Long: `Aperod (APRO) — A privacy-focused blockchain with RingCT transactions.

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

// readPassword reads a password from the terminal without echoing.
func readPassword(prompt string) (string, error) {
        fmt.Print(prompt)
        pw, err := term.ReadPassword(int(syscall.Stdin))
        fmt.Println()
        if err != nil {
                return "", err
        }
        return strings.TrimSpace(string(pw)), nil
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
                result, err := rpcCall(rpcAddr, "apr_getNodeInfo", nil)
                if err != nil {
                        return fmt.Errorf("node status: %w", err)
                }
                // Pretty-print the JSON result
                var pretty bytes.Buffer
                if e := json.Indent(&pretty, result, "", "  "); e != nil {
                        fmt.Println(string(result))
                } else {
                        fmt.Println(pretty.String())
                }
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

// wallet create — BIP39 mnemonic + HD derivation + encrypted keystore
var walletCreateCmd = &cobra.Command{
        Use:   "create",
        Short: "Generate a new HD wallet with BIP39 mnemonic",
        RunE: func(cmd *cobra.Command, args []string) error {
                words, _ := cmd.Flags().GetInt("words")
                outFile, _ := cmd.Flags().GetString("output")
                accountIdx, _ := cmd.Flags().GetUint32("account")

                // Map word count to entropy strength
                strength := wallet.Strength128
                if words == 24 {
                        strength = wallet.Strength256
                }

                // Generate mnemonic
                phrase, err := wallet.GenerateMnemonic(strength)
                if err != nil {
                        return fmt.Errorf("generate mnemonic: %w", err)
                }

                // Derive HD keys at m/44'/7777'/account'/0/0
                dk, err := wallet.DeriveFromMnemonic(phrase, "", accountIdx, 0)
                if err != nil {
                        return fmt.Errorf("derive keys: %w", err)
                }

                net := crypto.TestnetByte
                if flagNetwork == "mainnet" {
                        net = crypto.MainnetByte
                }

                addr := crypto.AddressFromKeys(net, dk.Keys)

                fmt.Println()
                fmt.Println("╔══════════════════════════════════════════════════════════════╗")
                fmt.Println("║            APEROD WALLET CREATED — KEEP MNEMONIC SAFE!      ║")
                fmt.Println("╠══════════════════════════════════════════════════════════════╣")
                fmt.Printf( "║  Address:   %-50s║\n", addr)
                fmt.Println("╠══════════════════════════════════════════════════════════════╣")
                fmt.Println("║  RECOVERY PHRASE (write these down in order, keep offline): ║")
                fmt.Println("║                                                              ║")
                phraseWords := strings.Fields(phrase)
                for i, w := range phraseWords {
                        fmt.Printf("║  %2d. %-58s║\n", i+1, w)
                }
                fmt.Println("║                                                              ║")
                fmt.Println("╠══════════════════════════════════════════════════════════════╣")
                fmt.Printf( "║  Derivation: m/44'/7777'/%d'/0/0%s║\n", accountIdx, strings.Repeat(" ", 28-len(fmt.Sprintf("%d", accountIdx))))
                fmt.Println("╚══════════════════════════════════════════════════════════════╝")
                fmt.Println()

                if outFile != "" {
                        pw, err := readPassword("  Enter keystore password: ")
                        if err != nil {
                                return fmt.Errorf("read password: %w", err)
                        }
                        pw2, err := readPassword("  Confirm password:        ")
                        if err != nil {
                                return fmt.Errorf("read password: %w", err)
                        }
                        if pw != pw2 {
                                return fmt.Errorf("passwords do not match")
                        }
                        if len(pw) < 8 {
                                return fmt.Errorf("password must be at least 8 characters")
                        }

                        ks, err := wallet.EncryptMnemonic(phrase, pw, string(addr))
                        if err != nil {
                                return fmt.Errorf("encrypt keystore: %w", err)
                        }
                        ksJSON, err := ks.Marshal()
                        if err != nil {
                                return fmt.Errorf("marshal keystore: %w", err)
                        }
                        if err := os.WriteFile(outFile, ksJSON, 0600); err != nil {
                                return fmt.Errorf("write keystore: %w", err)
                        }
                        fmt.Printf("  Encrypted keystore saved to: %s\n\n", outFile)
                } else {
                        // Show raw keys if no output file
                        fmt.Printf("  Spend Private: %s\n", hex.EncodeToString(dk.Keys.Spend.Private[:]))
                        fmt.Printf("  Spend Public:  %s\n", hex.EncodeToString(dk.Keys.Spend.Public[:]))
                        fmt.Printf("  View Private:  %s\n", hex.EncodeToString(dk.Keys.View.Private[:]))
                        fmt.Printf("  View Public:   %s\n\n", hex.EncodeToString(dk.Keys.View.Public[:]))
                }

                return nil
        },
}

// wallet restore — restore from BIP39 mnemonic phrase
var walletRestoreCmd = &cobra.Command{
        Use:   "restore",
        Short: "Restore wallet from BIP39 mnemonic phrase",
        RunE: func(cmd *cobra.Command, args []string) error {
                phrase, _ := cmd.Flags().GetString("mnemonic")
                outFile, _ := cmd.Flags().GetString("output")
                accountIdx, _ := cmd.Flags().GetUint32("account")

                if phrase == "" {
                        fmt.Print("  Enter mnemonic phrase: ")
                        var line string
                        fmt.Scanln(&line)
                        phrase = strings.TrimSpace(line)
                }

                if err := wallet.ValidateMnemonic(phrase); err != nil {
                        return fmt.Errorf("invalid mnemonic: %w", err)
                }

                dk, err := wallet.DeriveFromMnemonic(phrase, "", accountIdx, 0)
                if err != nil {
                        return fmt.Errorf("derive keys: %w", err)
                }

                net := crypto.TestnetByte
                if flagNetwork == "mainnet" {
                        net = crypto.MainnetByte
                }

                addr := crypto.AddressFromKeys(net, dk.Keys)

                fmt.Println()
                fmt.Println("  ✓ Mnemonic verified")
                fmt.Printf("  Address:      %s\n", addr)
                fmt.Printf("  Derivation:   m/44'/7777'/%d'/0/0\n\n", accountIdx)

                if outFile != "" {
                        pw, err := readPassword("  Enter keystore password: ")
                        if err != nil {
                                return fmt.Errorf("read password: %w", err)
                        }
                        pw2, err := readPassword("  Confirm password:        ")
                        if err != nil {
                                return fmt.Errorf("read password: %w", err)
                        }
                        if pw != pw2 {
                                return fmt.Errorf("passwords do not match")
                        }
                        if len(pw) < 8 {
                                return fmt.Errorf("password must be at least 8 characters")
                        }

                        ks, err := wallet.EncryptMnemonic(phrase, pw, string(addr))
                        if err != nil {
                                return fmt.Errorf("encrypt keystore: %w", err)
                        }
                        ksJSON, err := ks.Marshal()
                        if err != nil {
                                return fmt.Errorf("marshal keystore: %w", err)
                        }
                        if err := os.WriteFile(outFile, ksJSON, 0600); err != nil {
                                return fmt.Errorf("write keystore: %w", err)
                        }
                        fmt.Printf("  Encrypted keystore saved to: %s\n\n", outFile)
                }

                return nil
        },
}

// wallet unlock — decrypt keystore and show keys
var walletUnlockCmd = &cobra.Command{
        Use:   "unlock [keystore-file]",
        Short: "Decrypt a keystore file and show wallet info",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
                raw, err := os.ReadFile(args[0])
                if err != nil {
                        return fmt.Errorf("read keystore: %w", err)
                }

                ks, err := wallet.UnmarshalKeystore(raw)
                if err != nil {
                        return fmt.Errorf("parse keystore: %w", err)
                }

                pw, err := readPassword("  Enter keystore password: ")
                if err != nil {
                        return fmt.Errorf("read password: %w", err)
                }

                phrase, err := wallet.DecryptMnemonic(ks, pw)
                if err != nil {
                        return fmt.Errorf("decrypt keystore (wrong password?): %w", err)
                }

                accountIdx, _ := cmd.Flags().GetUint32("account")
                dk, err := wallet.DeriveFromMnemonic(phrase, "", accountIdx, 0)
                if err != nil {
                        return fmt.Errorf("derive keys: %w", err)
                }

                net := crypto.TestnetByte
                if flagNetwork == "mainnet" {
                        net = crypto.MainnetByte
                }

                addr := crypto.AddressFromKeys(net, dk.Keys)

                fmt.Println()
                fmt.Println("  ✓ Keystore unlocked")
                fmt.Printf("  Address:       %s\n", addr)
                fmt.Printf("  Derivation:    m/44'/7777'/%d'/0/0\n", accountIdx)
                fmt.Printf("  Spend Private: %s\n", hex.EncodeToString(dk.Keys.Spend.Private[:]))
                fmt.Printf("  Spend Public:  %s\n", hex.EncodeToString(dk.Keys.Spend.Public[:]))
                fmt.Printf("  View Private:  %s\n", hex.EncodeToString(dk.Keys.View.Private[:]))
                fmt.Printf("  View Public:   %s\n\n", hex.EncodeToString(dk.Keys.View.Public[:]))

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

                params := map[string]interface{}{"address": addr}
                if viewKey != "" {
                        params["view_key"] = viewKey
                }
                result, err := rpcCall(rpc, "apr_getBalance", params)
                if err != nil {
                        return fmt.Errorf("wallet balance: %w", err)
                }
                var resp struct {
                        Balance uint64 `json:"balance"`
                        Unit    string `json:"unit"`
                }
                if e := json.Unmarshal(result, &resp); e == nil && resp.Unit != "" {
                        balanceAPRO := float64(resp.Balance) / 1e8
                        fmt.Printf("Balance: %.8f APRO  (%d nAPRO)\n", balanceAPRO, resp.Balance)
                } else {
                        var pretty bytes.Buffer
                        if e2 := json.Indent(&pretty, result, "", "  "); e2 != nil {
                                fmt.Println(string(result))
                        } else {
                                fmt.Println(pretty.String())
                        }
                }
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

// wallet keygen — generate a raw Ed25519 validator key pair (not an HD wallet)
var walletKeygenCmd = &cobra.Command{
        Use:   "keygen",
        Short: "Generate a new raw Ed25519 validator key pair",
        Long:  "Generates a random Ed25519 private key for use as a validator node key.\nOutputs the private key (64-byte hex) and public key (32-byte hex).",
        RunE: func(cmd *cobra.Command, args []string) error {
                priv, pub, err := crypto.GenerateValidatorKey()
                if err != nil {
                        return fmt.Errorf("generate key: %w", err)
                }
                fmt.Printf("Private: %s\n", hex.EncodeToString(priv.Bytes()))
                fmt.Printf("Public:  %s\n", pub.Hex())
                return nil
        },
}

// wallet pubkey — derive public key from a private key hex
var walletPubkeyCmd = &cobra.Command{
        Use:   "pubkey <hex-private-key>",
        Short: "Derive the Ed25519 public key from a private key (32 or 64-byte hex)",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
                raw, err := hex.DecodeString(strings.TrimSpace(args[0]))
                if err != nil {
                        return fmt.Errorf("invalid hex: %w", err)
                }
                priv, err := crypto.ValidatorPrivKeyFromBytes(raw)
                if err != nil {
                        return fmt.Errorf("invalid key: %w", err)
                }
                fmt.Println(priv.Public().Hex())
                return nil
        },
}

func init() {
        walletCreateCmd.Flags().Int("words", 12, "Mnemonic length: 12 or 24 words")
        walletCreateCmd.Flags().String("output", "", "Save encrypted keystore to file (prompted for password)")
        walletCreateCmd.Flags().Uint32("account", 0, "BIP44 account index")
        walletRestoreCmd.Flags().String("mnemonic", "", "BIP39 mnemonic phrase (prompted if omitted)")
        walletRestoreCmd.Flags().String("output", "", "Save encrypted keystore to file (prompted for password)")
        walletRestoreCmd.Flags().Uint32("account", 0, "BIP44 account index")
        walletUnlockCmd.Flags().Uint32("account", 0, "BIP44 account index")
        walletBalanceCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
        walletBalanceCmd.Flags().String("view-key", "", "View private key (hex) for scanning")
        walletCmd.AddCommand(walletCreateCmd, walletRestoreCmd, walletUnlockCmd, walletBalanceCmd, walletValidateCmd, walletKeygenCmd, walletPubkeyCmd)
}

// ─── tx ───────────────────────────────────────────────────────────────────────

var txCmd = &cobra.Command{
        Use:   "tx",
        Short: "Transaction commands",
}

var txSendCmd = &cobra.Command{
        Use:   "send",
        Short: "Send APRO to an address",
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

                fmt.Printf("Sending %.8f APRO → %s\n", amount, to)
                fmt.Printf("RPC: %s | Key: %s\n", rpc, keyFile)
                // TODO Phase 1.4 / Phase 2: build and broadcast RingCT transaction
                fmt.Println("Transaction submitted. Hash: 0x0000...0000 (placeholder)")
                return nil
        },
}

// ─── validator partial-unstake (Task #356) ───────────────────────────────────

var validatorCmd = &cobra.Command{
        Use:   "validator",
        Short: "Validator management commands",
}

// validatorPartialUnstakeCmd signs and broadcasts a StakePartialWithdraw
// transaction via the node's admin REST endpoint.
//
// Usage:
//
//	aperod validator partial-unstake --node http://localhost:8080 \
//	        --pub-key <64-hex> --amount 500
var validatorPartialUnstakeCmd = &cobra.Command{
        Use:   "partial-unstake",
        Short: "Withdraw excess stake from a validator (7-day unbonding period)",
        Long: `Send a StakePartialWithdraw transaction that moves AMOUNT APRO from the
validator's stake into the unbonding queue (PartialUnbondingBlocks ≈ 7 days).

The amount must not exceed (current_stake - 10 000 APRO). The node must have the
validator key configured; this command calls its admin REST endpoint.`,
        RunE: func(cmd *cobra.Command, _ []string) error {
                nodeURL, _ := cmd.Flags().GetString("node")
                pubKey, _ := cmd.Flags().GetString("pub-key")
                amount, _ := cmd.Flags().GetFloat64("amount")

                if pubKey == "" {
                        return fmt.Errorf("--pub-key is required (64-hex validator public key)")
                }
                if len(pubKey) != 64 {
                        return fmt.Errorf("--pub-key must be exactly 64 hex characters (got %d)", len(pubKey))
                }
                if _, err := hex.DecodeString(pubKey); err != nil {
                        return fmt.Errorf("--pub-key is not valid hex: %w", err)
                }
                if amount <= 0 {
                        return fmt.Errorf("--amount must be a positive number of APRO")
                }

                amountNAPR := uint64(amount * 1e8)
                payload := fmt.Sprintf(`{"pub_key":"%s","amount_napr":%d}`, pubKey, amountNAPR)

                url := strings.TrimRight(nodeURL, "/") + "/api/v1/admin/partial-unstake"
                fmt.Printf("Submitting partial-unstake to %s\n", url)
                fmt.Printf("  pub_key    : %s…%s\n", pubKey[:8], pubKey[56:])
                fmt.Printf("  amount     : %.8f APRO (%d nAPRO)\n", amount, amountNAPR)

                httpResp, err := http.Post(url, "application/json", strings.NewReader(payload)) //nolint:noctx
                if err != nil {
                        return fmt.Errorf("partial-unstake request failed: %w", err)
                }
                defer httpResp.Body.Close()
                body, _ := io.ReadAll(httpResp.Body)
                if httpResp.StatusCode != 200 {
                        return fmt.Errorf("partial-unstake: server returned %d — %s", httpResp.StatusCode, strings.TrimSpace(string(body)))
                }
                // Pretty-print the JSON response
                var pretty bytes.Buffer
                if e := json.Indent(&pretty, body, "", "  "); e != nil {
                        fmt.Println(string(body))
                } else {
                        fmt.Println(pretty.String())
                }
                fmt.Println()
                fmt.Println("✅  Partial-unstake request sent. The unbonding period (~7 days) begins once")
                fmt.Println("    the transaction is included in a block.")
                return nil
        },
}

// ─── validator stake ──────────────────────────────────────────────────────────
//
// Builds, signs, and broadcasts a v2 UTXO-backed stake deposit.
// All cryptographic operations are performed locally — the private key is
// never sent to the node.
//
// Steps:
//  1. Derive the validator public key from --priv-key.
//  2. Fetch the UTXO from the node (GET /api/v1/utxo/{txhash}/{idx}).
//  3. Derive the deterministic Pedersen blind (mint-output formula).
//  4. Pre-flight check: Commit(amount, blind) == UTXO.AmountCommit.
//  5. Sign StakeSignMsgV2 locally.
//  6. Encode 173-byte v2 payload (EncodeStakeExtraV2).
//  7. Broadcast to POST /api/v1/stake.
//
// Usage:
//
//	aperod validator stake \
//	  --node         http://localhost:8080 \
//	  --priv-key     <hex validator private key: 64-byte (seed||pub) or 32-byte seed> \
//	  --utxo-txhash  <64-hex UTXO transaction hash> \
//	  --utxo-idx     <output index, default 0> \
//	  --amount       <APRO, e.g. 100000>
var validatorStakeCmd = &cobra.Command{
	Use:   "stake",
	Short: "Register validator stake — build and broadcast a v2 UTXO-backed deposit",
	Long: `Constructs a StakeDeposit v2 transaction locally and broadcasts it to the node.

The command derives your validator public key from --priv-key, fetches the
referenced UTXO from the node to read its on-chain Pedersen commitment, derives
the deterministic blinding factor (applicable to mint/coinbase outputs where the
APRO was minted to the address derived from your validator key), performs a
pre-flight check to confirm the commitment matches, signs the canonical
StakeSignMsgV2 locally, and broadcasts the 173-byte signed payload.

Your private key is never sent to the node.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		nodeURL, _ := cmd.Flags().GetString("node")
		privKeyHex, _ := cmd.Flags().GetString("priv-key")
		utxoTxHashHex, _ := cmd.Flags().GetString("utxo-txhash")
		utxoIdx, _ := cmd.Flags().GetUint32("utxo-idx")
		amountAPR, _ := cmd.Flags().GetFloat64("amount")

		// ── 1. Parse private key and derive public key ────────────────────────
		if privKeyHex == "" {
			return fmt.Errorf("--priv-key is required (hex Ed25519 key: 64-byte seed||pub or 32-byte seed)")
		}
		privKeyBytes, err := hex.DecodeString(strings.TrimSpace(privKeyHex))
		if err != nil {
			return fmt.Errorf("--priv-key is not valid hex: %w", err)
		}
		privKey, err := crypto.ValidatorPrivKeyFromBytes(privKeyBytes)
		if err != nil {
			return fmt.Errorf("--priv-key: %w", err)
		}
		pubKey := privKey.Public()
		pubKeyHex := hex.EncodeToString([]byte(pubKey))

		// ── 2. Validate UTXO flags ────────────────────────────────────────────
		if utxoTxHashHex == "" {
			return fmt.Errorf("--utxo-txhash is required (64-hex UTXO transaction hash)")
		}
		if len(utxoTxHashHex) != 64 {
			return fmt.Errorf("--utxo-txhash must be exactly 64 hex characters (got %d)", len(utxoTxHashHex))
		}
		txHashRaw, err := hex.DecodeString(utxoTxHashHex)
		if err != nil {
			return fmt.Errorf("--utxo-txhash is not valid hex: %w", err)
		}
		var burnTxHash crypto.Hash32
		copy(burnTxHash[:], txHashRaw)

		if amountAPR <= 0 {
			return fmt.Errorf("--amount must be a positive number of APRO (e.g. 100000)")
		}
		amountNAPR := uint64(amountAPR * 1e8)

		// ── 3. Fetch UTXO from the node ───────────────────────────────────────
		utxoURL := fmt.Sprintf("%s/api/v1/utxo/%s/%d",
			strings.TrimRight(nodeURL, "/"), utxoTxHashHex, utxoIdx)
		fmt.Printf("Fetching UTXO from %s ...\n", utxoURL)
		utxoResp, err := http.Get(utxoURL) //nolint:noctx
		if err != nil {
			return fmt.Errorf("fetch UTXO: %w", err)
		}
		defer utxoResp.Body.Close()
		utxoBody, _ := io.ReadAll(utxoResp.Body)
		if utxoResp.StatusCode != 200 {
			errMsg := strings.TrimSpace(string(utxoBody))
			// When the node is in light-pruning mode it embeds a hint in the
			// error text; propagate it directly so the operator knows what to do.
			if strings.Contains(errMsg, "pruning mode") || strings.Contains(errMsg, "pruned") {
				return fmt.Errorf("UTXO not found (%d): %s\n\n"+
					"Hint: Use an archive node (pruning.mode: archive in node.yaml) or "+
					"acquire a UTXO from a recent block to stake.",
					utxoResp.StatusCode, errMsg)
			}
			return fmt.Errorf("UTXO not found (%d): %s\n"+
				"  Check --utxo-txhash / --utxo-idx and ensure the UTXO is unspent.",
				utxoResp.StatusCode, errMsg)
		}
		var utxoData struct {
			AmountCommitHex  string  `json:"amount_commit_hex"`
			BlocksUntilPruned *uint64 `json:"blocks_until_pruned"`
		}
		if err := json.Unmarshal(utxoBody, &utxoData); err != nil {
			return fmt.Errorf("parse UTXO response: %w", err)
		}
		// Check whether the UTXO's originating block will be pruned before the
		// unbonding period completes.  The node only includes blocks_until_pruned
		// when the UTXO is within 10 % of keep_blocks from being pruned.
		if utxoData.BlocksUntilPruned != nil {
			bup := *utxoData.BlocksUntilPruned
			if bup < core.PartialUnbondingBlocks {
				// Hard rejection: the block carrying this UTXO will disappear
				// before the unbonding period (~7 days) ends, breaking the
				// commitment-verification path during unbonding.
				return fmt.Errorf(
					"UTXO will be pruned in %d blocks, which is less than the "+
						"unbonding period (%d blocks, ≈7 days).\n"+
						"   The node cannot verify the original commitment before "+
						"unbonding completes, which would break the unbonding flow.\n"+
						"   Use a UTXO from a more recent block, or switch to an "+
						"archive node (pruning.mode: archive in node.yaml).",
					bup, core.PartialUnbondingBlocks,
				)
			}
			// Close to pruning but still within the safe window — warn only.
			fmt.Printf("\n⚠️  WARNING: this UTXO's block will be pruned in approximately %d blocks.\n"+
				"   Once pruned, the stake transaction will be rejected.\n"+
				"   Broadcast your stake transaction as soon as possible, or switch\n"+
				"   to an archive node (pruning.mode: archive in node.yaml).\n\n",
				bup)
		}
		commitRaw, err := hex.DecodeString(utxoData.AmountCommitHex)
		if err != nil || len(commitRaw) != 32 {
			return fmt.Errorf("node returned invalid amount_commit_hex")
		}
		var onChainCommit crypto.Commitment
		copy(onChainCommit[:], commitRaw)
		fmt.Printf("  on-chain commit : %x…\n", onChainCommit[:8])

		// ── 4. Derive the deterministic Pedersen blinding factor ──────────────
		// For mint (coinbase) outputs the blind is deterministic:
		//   SHA-512("aperod-mint-blind-v1" || spendPub || amount_LE) → scalar
		// Validators who receive APRO via coinbase use their validator pub key as
		// the wallet spend pub key, so spendPub = validatorPub bytes.
		var spendPub crypto.Point32
		copy(spendPub[:], []byte(pubKey))
		burnBlind, err := crypto.DeterministicMintBlind(spendPub, amountNAPR)
		if err != nil {
			return fmt.Errorf("derive blinding factor: %w", err)
		}

		// ── 5. Pre-flight check: Commit(amount, blind) must match on-chain ────
		expectedCommit, err := crypto.Commit(amountNAPR, burnBlind)
		if err != nil {
			return fmt.Errorf("compute Pedersen commitment: %w", err)
		}
		if expectedCommit != onChainCommit {
			return fmt.Errorf(
				"AmountCommit mismatch — the UTXO's on-chain commitment does not match\n"+
					"the amount/blind derived from your key.\n\n"+
					"  on-chain : %x\n"+
					"  derived  : %x\n\n"+
					"Ensure --amount exactly matches the UTXO output amount and that the UTXO\n"+
					"was created by a coinbase mint to the address derived from --priv-key.",
				onChainCommit[:], expectedCommit[:],
			)
		}
		fmt.Println("✓  AmountCommit verified — blinding factor matches on-chain UTXO")

		// ── 6. Sign StakeSignMsgV2 locally ────────────────────────────────────
		msg := core.StakeSignMsgV2(core.StakeDeposit, pubKey, amountNAPR, burnTxHash, utxoIdx)
		sig, err := privKey.Sign(msg)
		if err != nil {
			return fmt.Errorf("sign stake message: %w", err)
		}

		// ── 7. Encode 173-byte v2 payload ─────────────────────────────────────
		extra, err := core.EncodeStakeExtraV2(
			core.StakeDeposit, pubKey, amountNAPR, sig,
			burnTxHash, utxoIdx, burnBlind,
		)
		if err != nil {
			return fmt.Errorf("encode StakeExtraV2: %w", err)
		}
		txExtraHex := hex.EncodeToString(extra)

		// ── 8. Broadcast to node ──────────────────────────────────────────────
		broadcastURL := strings.TrimRight(nodeURL, "/") + "/api/v1/stake"
		broadcastPayload := fmt.Sprintf(`{"tx_extra_hex":%q}`, txExtraHex)

		fmt.Println()
		fmt.Printf("  validator pub  : %s…%s\n", pubKeyHex[:8], pubKeyHex[56:])
		fmt.Printf("  amount         : %.8f APRO (%d nAPRO)\n", amountAPR, amountNAPR)
		fmt.Printf("  utxo_txhash    : %s…%s\n", utxoTxHashHex[:8], utxoTxHashHex[56:])
		fmt.Printf("  utxo_out_idx   : %d\n", utxoIdx)
		fmt.Printf("Broadcasting to %s ...\n", broadcastURL)

		httpResp, err := http.Post(broadcastURL, "application/json", strings.NewReader(broadcastPayload)) //nolint:noctx
		if err != nil {
			return fmt.Errorf("broadcast stake tx: %w", err)
		}
		defer httpResp.Body.Close()
		body, _ := io.ReadAll(httpResp.Body)
		if httpResp.StatusCode != 201 && httpResp.StatusCode != 200 {
			errMsg := strings.TrimSpace(string(body))
			// Surface pruning-aware hint when the node returns a pruning error.
			if strings.Contains(errMsg, "pruning mode") || strings.Contains(errMsg, "pruned") {
				return fmt.Errorf("broadcast failed (%d): %s\n\n"+
					"Hint: Use an archive node (pruning.mode: archive in node.yaml) or "+
					"acquire a UTXO from a recent block to stake.",
					httpResp.StatusCode, errMsg)
			}
			return fmt.Errorf("broadcast failed (%d): %s",
				httpResp.StatusCode, errMsg)
		}
		var pretty bytes.Buffer
		if e := json.Indent(&pretty, body, "", "  "); e != nil {
			fmt.Println(string(body))
		} else {
			fmt.Println(pretty.String())
		}
		fmt.Println()
		fmt.Println("✅  Stake deposit submitted. Your validator will enter the activation queue")
		fmt.Println("    once the transaction is included in a block (next epoch boundary).")
		return nil
	},
}

func init() {
	txSendCmd.Flags().String("to", "", "Recipient Aperod address")
	txSendCmd.Flags().Float64("amount", 0, "Amount in APRO")
	txSendCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
	txSendCmd.Flags().String("key-file", "", "Wallet keystore file")
	txCmd.AddCommand(txSendCmd)

	// validator partial-unstake
	validatorPartialUnstakeCmd.Flags().String("node", "http://localhost:8080", "Node admin REST URL")
	validatorPartialUnstakeCmd.Flags().String("pub-key", "", "Validator public key (64-hex)")
	validatorPartialUnstakeCmd.Flags().Float64("amount", 0, "Amount to withdraw in APRO")
	validatorCmd.AddCommand(validatorPartialUnstakeCmd)

	// validator stake — local signing, UTXO pre-flight check, broadcast
	validatorStakeCmd.Flags().String("node", "http://localhost:8080", "Node REST URL")
	validatorStakeCmd.Flags().String("priv-key", "", "Validator private key (64-hex Ed25519) — never sent to the node")
	validatorStakeCmd.Flags().String("utxo-txhash", "", "64-hex hash of the UTXO transaction to burn as stake proof")
	validatorStakeCmd.Flags().Uint32("utxo-idx", 0, "Output index within the UTXO transaction (default 0)")
	validatorStakeCmd.Flags().Float64("amount", 0, "Amount to stake in APRO (e.g. 100000)")
	validatorCmd.AddCommand(validatorStakeCmd)

	rootCmd.AddCommand(validatorCmd)
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
                result, err := rpcCall(rpc, "apr_getNodeInfo", nil)
                if err != nil {
                        return fmt.Errorf("chain info: %w", err)
                }
                var pretty bytes.Buffer
                if e := json.Indent(&pretty, result, "", "  "); e != nil {
                        fmt.Println(string(result))
                } else {
                        fmt.Println(pretty.String())
                }
                return nil
        },
}

// ChainExport is a JSON-serializable snapshot of the full chain.
type ChainExport struct {
        Version   string            `json:"version"`
        TipHeight uint64            `json:"tip_height"`
        Blocks    []json.RawMessage `json:"blocks"` // raw block JSON per height
}

var chainExportCmd = &cobra.Command{
        Use:   "export",
        Short: "Export the entire chain to a JSON file",
        RunE: func(cmd *cobra.Command, args []string) error {
                dataDir, _ := cmd.Flags().GetString("data-dir")
                outFile, _ := cmd.Flags().GetString("out")

                db, err := store.Open(dataDir + "/chain.db")
                if err != nil {
                        return fmt.Errorf("open db: %w", err)
                }
                defer db.Close()

                _, tipHeight, err := db.GetTip()
                if err != nil {
                        return fmt.Errorf("get tip: %w", err)
                }

                export := ChainExport{
                        Version:   "aperod/chain-export/v1",
                        TipHeight: tipHeight,
                        Blocks:    make([]json.RawMessage, 0, tipHeight+1),
                }

                for h := uint64(0); h <= tipHeight; h++ {
                        data, err := db.GetRawBlockByHeight(h)
                        if err != nil {
                                return fmt.Errorf("block %d: %w", h, err)
                        }
                        if data == nil {
                                return fmt.Errorf("missing block at height %d", h)
                        }
                        export.Blocks = append(export.Blocks, json.RawMessage(data))
                }

                out, err := json.MarshalIndent(export, "", "  ")
                if err != nil {
                        return fmt.Errorf("marshal: %w", err)
                }
                if err := os.WriteFile(outFile, out, 0644); err != nil {
                        return fmt.Errorf("write %s: %w", outFile, err)
                }
                fmt.Printf("Exported %d blocks to %s\n", len(export.Blocks), outFile)
                return nil
        },
}

var chainImportCmd = &cobra.Command{
        Use:   "import [file]",
        Short: "Import a chain export file into a LevelDB data directory",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
                inFile := args[0]
                dataDir, _ := cmd.Flags().GetString("data-dir")

                raw, err := os.ReadFile(inFile)
                if err != nil {
                        return fmt.Errorf("read %s: %w", inFile, err)
                }

                var export ChainExport
                if err := json.Unmarshal(raw, &export); err != nil {
                        return fmt.Errorf("parse export: %w", err)
                }
                if export.Version != "aperod/chain-export/v1" {
                        return fmt.Errorf("unknown export version: %s", export.Version)
                }
                if uint64(len(export.Blocks)) != export.TipHeight+1 {
                        return fmt.Errorf("block count mismatch: header says tip=%d but %d blocks present",
                                export.TipHeight, len(export.Blocks))
                }

                db, err := store.Open(dataDir + "/chain.db")
                if err != nil {
                        return fmt.Errorf("open db: %w", err)
                }
                defer db.Close()

                _, existingTip, _ := db.GetTip()
                if existingTip > 0 {
                        fmt.Printf("WARNING: data dir already has chain at height %d. Overwriting...\n", existingTip)
                }

                for h, blockData := range export.Blocks {
                        var hdr struct {
                                Header struct {
                                        Height uint64 `json:"Height"`
                                } `json:"Header"`
                        }
                        if err := json.Unmarshal(blockData, &hdr); err != nil {
                                return fmt.Errorf("parse block %d header: %w", h, err)
                        }
                        height := hdr.Header.Height
                        if height != uint64(h) {
                                return fmt.Errorf("block %d has wrong height field %d", h, height)
                        }

                        var placeholderHash crypto.Hash32
                        for i := 0; i < 8; i++ {
                                placeholderHash[i] = byte(height >> (uint(i) * 8))
                        }
                        if err := db.PutRawBlock(placeholderHash, height, blockData); err != nil {
                                return fmt.Errorf("store block %d: %w", h, err)
                        }
                }

                var tipHash crypto.Hash32
                for i := 0; i < 8; i++ {
                        tipHash[i] = byte(export.TipHeight >> (uint(i) * 8))
                }
                if err := db.PutTip(tipHash, export.TipHeight); err != nil {
                        return fmt.Errorf("put tip: %w", err)
                }

                fmt.Printf("Imported %d blocks (heights 0–%d) into %s\n",
                        len(export.Blocks), export.TipHeight, dataDir)
                fmt.Println("NOTE: Start the node once to recompute block hashes and rebuild the UTXO set.")
                return nil
        },
}

func init() {
        chainInfoCmd.Flags().String("rpc", "http://localhost:8545", "RPC endpoint")
        chainExportCmd.Flags().String("data-dir", "./data", "Node data directory (contains chain.db)")
        chainExportCmd.Flags().String("out", "chain-export.json", "Output file path")
        chainImportCmd.Flags().String("data-dir", "./data", "Node data directory (contains chain.db)")
        chainCmd.AddCommand(chainInfoCmd, chainExportCmd, chainImportCmd)
}
