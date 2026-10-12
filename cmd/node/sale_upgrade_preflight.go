package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// The normal updater obtains a new completed logical copy via the private Unix
// socket while the service keeps running. An explicit copy is for offline
// verification only; it must include the same completed manifest.
func runSaleUpgradePreflight(cfg *config.Config, stage string, out io.Writer) error {
	if stage == "" {
		socket := filepath.Join(cfg.DataDir, backupSocketName)
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 20 * time.Minute}
		response, err := client.Post("http://localhost/checkpoint", "", nil)
		if err != nil {
			return fmt.Errorf("sale preflight: fresh consistent backup unavailable; service must remain running")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("sale preflight: backup refused (%d); service must remain running", response.StatusCode)
		}
		var reply checkpointResponse
		if err := decodeSalePreflightJSON(response.Body, &reply); err != nil {
			return err
		}
		root, err := filepath.EvalSymlinks(filepath.Join(cfg.DataDir, ".backup-staging"))
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(reply.Path)
		if err != nil || filepath.Dir(resolved) != root || !strings.HasPrefix(filepath.Base(resolved), "checkpoint-") {
			return fmt.Errorf("sale preflight: backup response outside owned checkpoint directory")
		}
		stage = resolved
		// Preserve refused copies as evidence. Successful copies are disposable.
		return verifySaleUpgradeStage(cfg, stage, &reply, out, true)
	}
	return verifySaleUpgradeStage(cfg, stage, nil, out, false)
}

func decodeSalePreflightJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("sale preflight: invalid backup receipt")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("sale preflight: trailing backup receipt")
	}
	return nil
}

func verifySaleUpgradeStage(cfg *config.Config, stage string, reply *checkpointResponse, out io.Writer, disposable bool) error {
	file, err := os.Open(filepath.Join(stage, "manifest.json"))
	if err != nil {
		return fmt.Errorf("sale preflight: completed backup manifest required")
	}
	var manifest checkpointManifest
	decodeErr := decodeSalePreflightJSON(file, &manifest)
	closeErr := file.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	hashRaw, err := hex.DecodeString(manifest.TipHash)
	if err != nil || len(hashRaw) != 32 || !manifest.Success ||
		manifest.TipHash != strings.ToLower(manifest.TipHash) {
		return fmt.Errorf("sale preflight: incomplete backup manifest")
	}
	if reply != nil && (reply.TipHash != manifest.TipHash || reply.TipHeight != manifest.TipHeight) {
		return fmt.Errorf("sale preflight: backup response and manifest disagree")
	}
	var hash crypto.Hash32
	copy(hash[:], hashRaw)
	report, err := store.CheckSaleUpgradeCopy(filepath.Join(stage, "chain.db"),
		filepath.Join(cfg.DataDir, "chain.db"), cfg.Consensus.SaleMigrationFile, hash, manifest.TipHeight)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(report); err != nil {
		return err
	}
	if disposable {
		if err := os.RemoveAll(stage); err != nil {
			return fmt.Errorf("sale preflight: verified temporary copy cleanup failed")
		}
	}
	return nil
}
