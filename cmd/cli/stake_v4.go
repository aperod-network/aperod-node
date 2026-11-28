package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/internal/checkedio"
	"github.com/aperod/aperod/wallet"
)

type stakeV4Source struct {
	TxHash     string                `json:"tx_hash"`
	OutIdx     *uint32               `json:"out_idx"`
	Exists     bool                  `json:"exists"`
	OneTimePub string                `json:"one_time_pub_hex"`
	TxPub      string                `json:"tx_pub_key_hex"`
	Commitment string                `json:"amount_commit_hex"`
	EncAmount  string                `json:"enc_amount_hex"`
	Version    core.TxVersion        `json:"tx_version"`
	Binding    *crypto.AmountBinding `json:"amount_binding"`
}

func parseStakeHex(text string, dst []byte, name string) error {
	if len(text) != hex.EncodedLen(len(dst)) {
		return fmt.Errorf("%s must be %d hex characters", name, hex.EncodedLen(len(dst)))
	}
	if _, err := hex.Decode(dst, []byte(text)); err != nil {
		return fmt.Errorf("%s is not valid hex", name)
	}
	return nil
}

func buildWalletStakeV4(source stakeV4Source, ref crypto.Hash32, index uint32, amount uint64, keys *crypto.WalletKeyPair, signer crypto.ValidatorPrivKey) (core.Transaction, crypto.KeyImage, error) {
	var tx core.Transaction
	var image crypto.KeyImage
	if !source.Exists || source.OutIdx == nil || *source.OutIdx != index || !strings.EqualFold(source.TxHash, hex.EncodeToString(ref[:])) {
		return tx, image, fmt.Errorf("node did not return the requested active output")
	}
	var oneTimePub, txPub crypto.Point32
	var commit crypto.Commitment
	var enc [8]byte
	for _, field := range []struct {
		value string
		dest  []byte
		name  string
	}{
		{source.OneTimePub, oneTimePub[:], "one-time public key"},
		{source.TxPub, txPub[:], "output transaction public key"},
		{source.Commitment, commit[:], "output commitment"},
		{source.EncAmount, enc[:], "encrypted amount"},
	} {
		if err := parseStakeHex(field.value, field.dest, field.name); err != nil {
			return tx, image, err
		}
	}
	if txPub == (crypto.Point32{}) {
		return tx, image, fmt.Errorf("this command only accepts ordinary stealth payment outputs, not mint outputs")
	}
	hs, err := crypto.ScanForOutput(keys.View.Private, keys.Spend.Public, txPub, oneTimePub)
	if err != nil || hs == nil {
		return tx, image, fmt.Errorf("output does not belong to the selected wallet")
	}
	defer crypto.ZeroBytes(hs[:])
	secret, err := crypto.AddScalars(*hs, keys.Spend.Private)
	if err != nil {
		return tx, image, fmt.Errorf("derive one-time key: %w", err)
	}
	defer crypto.ZeroBytes(secret[:])
	var walletAmount uint64
	isBound := source.Version == core.TxVersionAmountBinding || source.Version == core.TxVersionAVMAmountBinding
	switch {
	case isBound || source.Binding != nil:
		if source.Binding == nil || enc != ([8]byte{}) {
			return tx, image, fmt.Errorf("invalid bound output metadata")
		}
		valid, err := crypto.VerifyAmountBinding(commit, oneTimePub, source.Binding)
		if err != nil || !valid {
			return tx, image, fmt.Errorf("invalid output amount proof")
		}
		walletAmount, err = crypto.DecryptAmountBinding(source.Binding, secret)
		if err != nil {
			return tx, image, fmt.Errorf("cannot decrypt bound output amount")
		}
	default:
		walletAmount = core.DecryptAmount(enc, hs)
	}
	if amount == 0 || amount != walletAmount {
		return tx, image, fmt.Errorf("stake must equal the entire verified output amount")
	}
	blind, err := crypto.DeterministicPaymentBlind(*hs, amount)
	if err != nil {
		return tx, image, fmt.Errorf("derive payment blind: %w", err)
	}
	defer crypto.ZeroBytes(blind[:])
	opened, err := crypto.Commit(amount, blind)
	if err != nil || opened != commit {
		return tx, image, fmt.Errorf("output commitment cannot be opened with the wallet's payment blind; change outputs may not be supported")
	}
	pub := signer.Public()
	action := core.StakeDeposit
	image, proof, err := crypto.ProveKeyImage(secret, oneTimePub, core.StakeKeyImageProofContext(action, pub, amount, ref, index, blind))
	if err != nil {
		return tx, image, fmt.Errorf("prove ownership of collateral: %w", err)
	}
	sig, err := signer.Sign(core.StakeSignMsgV2(action, pub, amount, ref, index))
	if err != nil {
		return tx, image, fmt.Errorf("sign stake: %w", err)
	}
	extra, err := core.EncodeStakeExtraV4(action, pub, amount, sig, ref, index, blind, image, proof)
	if err != nil {
		return tx, image, fmt.Errorf("encode stake: %w", err)
	}
	return core.Transaction{Version: core.TxVersionStake, Extra: extra}, image, nil
}

func stakeNodeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.Port() == "" {
		return "", fmt.Errorf("node must be a loopback HTTP URL with an explicit port")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("node must use a literal loopback IP, such as 127.0.0.1")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func stakeNodeJSON(ctx context.Context, client *http.Client, endpoint, method, path, key string, payload []byte, status int, dst any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if method == http.MethodPost {
		req.Header.Set("X-API-Key", key)
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("local node request failed: %w", err)
	}
	defer checkedio.LogClose(slog.Default(), res.Body, "local node response body")
	if res.StatusCode != status {
		return fmt.Errorf("local node rejected %s (HTTP %d)", method, res.StatusCode)
	}
	const maxResponse = 256 << 10
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return fmt.Errorf("invalid or oversized response from local node")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("invalid JSON response from local node")
	}
	return nil
}

type stakeV4NodeStatus struct {
	Height         *uint64 `json:"height"`
	Syncing        *bool   `json:"syncing"`
	UTXORebuilding *bool   `json:"utxo_rebuilding"`
	Activation     *uint64 `json:"stake_key_image_v4_activation_height"`
}

func (s stakeV4NodeStatus) check() error {
	if s.Height == nil || s.Syncing == nil || s.UTXORebuilding == nil ||
		*s.Height == 0 || *s.Syncing || *s.UTXORebuilding {
		return fmt.Errorf("local node has not confirmed chain and UTXO readiness")
	}
	if s.Activation == nil || *s.Activation == 0 {
		return fmt.Errorf("local node does not report a configured v4 stake activation")
	}
	if *s.Activation-1 > *s.Height {
		return fmt.Errorf("v4 stake activates at height %d; local chain is at %d", *s.Activation, *s.Height)
	}
	return nil
}

func runStakeV4(cmd *cobra.Command, _ []string) error {
	flags := cmd.Flags()
	node, _ := flags.GetString("node")
	node, err := stakeNodeURL(node)
	if err != nil {
		return err
	}
	genesis, _ := flags.GetString("genesis-hash")
	refText, _ := flags.GetString("utxo-txhash")
	index, _ := flags.GetUint32("utxo-idx")
	amount, _ := flags.GetUint64("amount-napr")
	submit, _ := flags.GetBool("submit")
	key := os.Getenv("APEROD_API_KEY")
	if submit && key == "" {
		return fmt.Errorf("set the local node's APEROD_API_KEY before submitting")
	}
	var genesisHash, ref crypto.Hash32
	if err := parseStakeHex(genesis, genesisHash[:], "trusted genesis hash"); err != nil {
		return err
	}
	if err := parseStakeHex(refText, ref[:], "output transaction hash"); err != nil {
		return err
	}
	if amount == 0 {
		return fmt.Errorf("amount-napr must be a positive integer")
	}
	client := &http.Client{
		Timeout:   12 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var genesisBlock struct{ Hash string }
	get := func(path string, dst any) error {
		return stakeNodeJSON(cmd.Context(), client, node, http.MethodGet, path, "", nil, http.StatusOK, dst)
	}
	if err := get("/api/v1/blocks/0", &genesisBlock); err != nil {
		return fmt.Errorf("verify node genesis: %w", err)
	}
	if !strings.EqualFold(genesisBlock.Hash, genesis) {
		return fmt.Errorf("local node genesis differs from the trusted genesis hash")
	}
	checkLocalStake := func() error {
		var status stakeV4NodeStatus
		if err := get("/api/v1/status", &status); err != nil {
			return fmt.Errorf("check local stake readiness: %w", err)
		}
		return status.check()
	}
	if err := checkLocalStake(); err != nil {
		return err
	}
	var source stakeV4Source
	if err := get("/api/v1/utxo/"+hex.EncodeToString(ref[:])+"/"+strconv.FormatUint(uint64(index), 10), &source); err != nil {
		return fmt.Errorf("fetch collateral: %w", err)
	}
	keystore, _ := flags.GetString("keystore")
	validator, _ := flags.GetString("validator-key-file")
	walletKeys, err := unlockStakeWallet(keystore)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(walletKeys.Spend.Private[:])
	defer crypto.ZeroBytes(walletKeys.View.Private[:])
	signer, err := loadStakeSigner(validator)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(signer.Bytes())
	tx, image, err := buildWalletStakeV4(source, ref, index, amount, walletKeys, signer)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(tx.Extra)
	var spent struct {
		Value *bool `json:"is_spent"`
	}
	if err := get("/api/v1/keyimage/"+hex.EncodeToString(image[:])+"/is-spent", &spent); err != nil {
		return fmt.Errorf("check collateral spentness: %w", err)
	}
	if spent.Value == nil || *spent.Value {
		return fmt.Errorf("collateral is spent or the node did not verify its spentness")
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Collateral: %x:%d, %d nAPRO; validator: %s\n", ref, index, amount, signer.Public().Hex()); err != nil {
		return fmt.Errorf("write stake preflight output; no deposit submitted: %w", err)
	}
	if !submit {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Preflight only. Local v4 activation checked; network-wide activation and inclusion are not verified.")
		return err
	}
	// The operator may have spent time unlocking the wallet. Recheck before
	// sending any signed stake payload to the node.
	if err := checkLocalStake(); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"tx_extra_hex": hex.EncodeToString(tx.Extra)})
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(body)
	var result struct {
		Hash   string `json:"tx_hash"`
		Status string `json:"status"`
	}
	if err := stakeNodeJSON(cmd.Context(), client, node, http.MethodPost, "/api/v1/stake", key, body, http.StatusCreated, &result); err != nil {
		return fmt.Errorf("deposit not confirmed; check the local node before retrying: %w", err)
	}
	if result.Status != "pending" || !strings.EqualFold(result.Hash, fmt.Sprintf("%x", tx.Hash())) {
		return fmt.Errorf("deposit response did not confirm the expected pending transaction")
	}
	reportPendingStake(cmd.OutOrStdout(), slog.Default(), result.Hash)
	return nil
}

// reportPendingStake must not convert an accepted submission into a retryable
// failure just because terminal output failed. The diagnostic retains its hash.
func reportPendingStake(out io.Writer, log *slog.Logger, hash string) {
	if _, err := fmt.Fprintf(out, "Pending: %s. Verify canonical inclusion and active-set selection separately.\n", hash); err != nil {
		log.Warn("stake transaction is pending but terminal output failed; do not resubmit",
			"tx_hash", hash, "err", err)
	}
}

func unlockStakeWallet(path string) (_ *crypto.WalletKeyPair, readCloseErr error) {
	// #nosec G304 -- Explicit local keystore selected by the CLI operator; its contents are separately decrypted and validated.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer checkedio.CloseReadResult(file, &readCloseErr)
	ks, err := wallet.UnmarshalKeystoreReader(file)
	if err != nil {
		return nil, err
	}
	password, err := readPassword("Keystore password: ")
	if err != nil {
		return nil, err
	}
	phrase, err := wallet.DecryptMnemonic(ks, password)
	if err != nil {
		return nil, fmt.Errorf("could not unlock keystore: %w", err)
	}
	var account, index uint32
	if ks.AccountIndex != nil {
		account = *ks.AccountIndex
	}
	if ks.AddressIndex != nil {
		index = *ks.AddressIndex
	}
	dk, err := wallet.DeriveFromMnemonic(phrase, "", account, index)
	if err != nil {
		return nil, err
	}
	addr := crypto.AddressFromKeys(crypto.MainnetByte, dk.Keys)
	if err := ks.VerifyDerivation(account, index, string(addr)); err != nil {
		crypto.ZeroBytes(dk.Keys.View.Private[:])
		crypto.ZeroBytes(dk.Keys.Spend.Private[:])
		return nil, fmt.Errorf("not a locally verified Mainnet wallet: %w", err)
	}
	return dk.Keys, nil
}

func loadStakeSigner(path string) (_ crypto.ValidatorPrivKey, readCloseErr error) {
	// #nosec G304 -- Explicit local collateral input selected by the CLI operator.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer checkedio.CloseReadResult(file, &readCloseErr)
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("validator key must be a private regular file (mode 0600 or stricter)")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(raw)
	if len(raw) != ed25519.SeedSize && len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("validator key must contain a raw Ed25519 seed or private key")
	}
	signer, err := crypto.ValidatorPrivKeyFromBytes(raw[:ed25519.SeedSize])
	if err != nil {
		return nil, err
	}
	if len(raw) == ed25519.PrivateKeySize && !bytes.Equal(raw[ed25519.SeedSize:], signer.Public()) {
		crypto.ZeroBytes(signer.Bytes())
		return nil, fmt.Errorf("validator key public half does not match its seed")
	}
	return signer, nil
}

func init() {
	cmd := &cobra.Command{
		Use:   "stake-v4",
		Short: "Check or submit a Mainnet stake deposit from an owned payment output",
		Args:  cobra.NoArgs,
		RunE:  runStakeV4,
	}
	cmd.Flags().String("node", "http://127.0.0.1:8545", "Local node REST address (literal loopback IP)")
	cmd.Flags().String("genesis-hash", "", "Trusted genesis hash from the independently verified Mainnet bootstrap")
	cmd.Flags().String("keystore", "", "Local encrypted Mainnet CLI wallet keystore")
	cmd.Flags().String("validator-key-file", "", "Private raw validator key file")
	cmd.Flags().String("utxo-txhash", "", "Owned payment output transaction hash")
	cmd.Flags().Uint32("utxo-idx", 0, "Owned payment output index")
	cmd.Flags().Uint64("amount-napr", 0, "Entire output amount in nAPRO (integer)")
	cmd.Flags().Bool("submit", false, "Submit the signed deposit; default only checks the collateral")
	for _, name := range []string{"genesis-hash", "keystore", "validator-key-file", "utxo-txhash", "amount-napr"} {
		_ = cmd.MarkFlagRequired(name)
	}
	validatorCmd.AddCommand(cmd)
}
