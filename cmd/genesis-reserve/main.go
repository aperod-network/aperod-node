// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
	"github.com/aperod/aperod/wallet"
)

func writeExclusive(path string, value interface{}) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if err = json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	action := flag.String("action", "", "init-wallet, rekey-wallet or attest")
	path := flag.String("wallet", "", "encrypted treasury keystore path")
	output := flag.String("output", "", "new exclusive witness/keystore path")
	signer := flag.String("validator-key", "", "0600 raw validator signing key")
	genesis := flag.String("genesis", "", "canonical genesis hash")
	snapshot := flag.String("snapshot", "", "canonical v3 nominal snapshot root")
	height := flag.Uint64("height", 0, "exact future activation height")
	database := flag.String("database", "", "offline canonical chain DB; fills exact activation/genesis/snapshot")
	address := flag.String("liquidity-address", "", "public treasury address")
	flag.Parse()
	var offlineDB *store.DB
	password := os.Getenv("SESSION_SECRET")
	if *action == "init-wallet" || *action == "rekey-wallet" || *action == "normalize-wallet" {
		if len(password) < 32 || *path == "" {
			return fmt.Errorf("protected keystore password/path unavailable")
		}
		if *action == "init-wallet" {
			mnemonic, err := wallet.GenerateMnemonic(256)
			if err != nil {
				return err
			}
			keys, err := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
			if err != nil {
				return err
			}
			address := crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys)
			keystore, err := wallet.EncryptMnemonicWithDerivation(mnemonic, password, string(address), 0, 0)
			if err != nil {
				return err
			}
			if err = writeExclusive(*path, keystore); err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"address": string(address), "custody": "encrypted_operator_keystore"})
		}
		raw, err := os.ReadFile(*path)
		if err != nil {
			return fmt.Errorf("protected keystore unavailable")
		}
		var keystore wallet.KeystoreV1
		if err = json.Unmarshal(raw, &keystore); err != nil {
			return err
		}
		mnemonic, err := wallet.DecryptMnemonic(&keystore, password)
		if err != nil {
			return fmt.Errorf("protected keystore authentication failed")
		}
		keys, err := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
		if *action == "normalize-wallet" {
			if err != nil || *output == "" || (string(keys.Address) != keystore.Address && string(crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys)) != keystore.Address) {
				return fmt.Errorf("wallet normalization integrity check failed")
			}
			keystore.Address = string(crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys))
			if err := writeExclusive(*output, &keystore); err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"address": keystore.Address})
		}
		if err != nil || string(crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys)) != keystore.Address {
			return fmt.Errorf("keystore address integrity failed")
		}
		newPassword := os.Getenv("TREASURY_EXPORT_PASSWORD")
		if len(newPassword) < 12 {
			return fmt.Errorf("export password must have at least 12 characters")
		}
		rekeyed, err := wallet.EncryptMnemonicWithDerivation(mnemonic, newPassword, keystore.Address, 0, 0)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(rekeyed)
	}
	if *action == "attest" && *database != "" {
		db, err := store.Open(*database)
		if err != nil {
			return err
		}
		offlineDB = db
		defer db.Close()
		_, tip, err := db.GetTip()
		if err != nil {
			return err
		}
		cp, err := db.LoadLPoDCheckpoint()
		if err != nil || cp == nil || cp.Allocation == nil || cp.Allocation.Version != 3 || cp.GenesisVesting != nil || cp.State.LastHeight != tip {
			return fmt.Errorf("canonical v3 prerequisite absent or annual reserve already active")
		}
		*height = tip + 1
		*genesis = hex.EncodeToString(cp.Allocation.Genesis[:])
		*snapshot = hex.EncodeToString(cp.Allocation.SnapshotRoot[:])
	}
	if *action != "attest" || *height == 0 || *output == "" {
		return fmt.Errorf("explicit action/future activation/output required")
	}
	g, err := hex.DecodeString(*genesis)
	if err != nil || len(g) != 32 {
		return fmt.Errorf("invalid genesis hash")
	}
	s, err := hex.DecodeString(*snapshot)
	if err != nil || len(s) != 32 {
		return fmt.Errorf("invalid nominal snapshot root")
	}
	info, err := os.Lstat(*signer)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("signing key must be a 0600 regular file")
	}
	fd, err := syscall.Open(*signer, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("cannot securely open signing key")
	}
	file := os.NewFile(uintptr(fd), *signer)
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil || (len(raw) != 32 && len(raw) != 64) {
		return fmt.Errorf("invalid signing key length")
	}
	defer crypto.ZeroBytes(raw)
	if len(raw) == 32 {
		raw = ed25519.NewKeyFromSeed(raw)
		defer crypto.ZeroBytes(raw)
	}
	key, err := crypto.ValidatorPrivKeyFromBytes(raw)
	if err != nil {
		return err
	}
	proof := &store.GenesisVestingFunding{Version: 2, Height: *height,
		Recipient: store.GenesisVestingRecipient, Reserve: store.GenesisVestingReserve,
		OtherAllocations: store.GenesisNominalTotal - store.GenesisVestingReserve - store.GenesisLiquidityAmount,
		Trust:            store.GenesisVestingTrust, LiquidityRecipient: crypto.Address(*address),
		LiquidityAmount: store.GenesisLiquidityAmount, LiquidityTrust: store.GenesisLiquidityTrust}
	copy(proof.Genesis[:], g)
	copy(proof.Snapshot[:], s)
	if _, _, _, err = crypto.DecodeAddress(proof.LiquidityRecipient); err != nil {
		return fmt.Errorf("invalid public treasury address")
	}
	signature, err := key.Sign(proof.Message())
	if err != nil {
		return err
	}
	proof.Attestations = []store.LPoDAttestation{{Validator: key.Public(), Signature: signature}}
	if offlineDB != nil {
		if err := offlineDB.CheckGenesisVestingConfig(*height, proof); err != nil {
			return err
		}
	}
	return writeExclusive(*output, proof)
}
