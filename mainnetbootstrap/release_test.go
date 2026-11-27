package mainnetbootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestReleaseAuthenticatesRevisionAndPayload(t *testing.T) {
	root := t.TempDir()
	put := func(rel, value string) {
		t.Helper()
		f := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(f), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		"bin/aperod-node", "config/node.yaml", "config/genesis.yaml",
		"witnesses/bootstrap-signed.json", "witnesses/avm-signed.json",
		"witnesses/aperod-authority-fork.json", "witnesses/aperod-protocol-sale-opening.json", "witnesses/aperod-lpod-v3-witness.json",
		"state/mainnet/chain.db/CURRENT",
	} {
		put(path, path)
	}
	const profile = "network: mainnet\n"
	put("config/node.yaml", profile)
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	if _, err := InspectRelease(root, commit); err == nil {
		t.Fatal("release with missing LOCK accepted")
	}
	put("state/mainnet/chain.db/LOCK", "")
	put("state/mainnet/chain.db/MANIFEST-000001", "manifest")
	put("state/mainnet/chain.db/000002.ldb", "table")
	put("witnesses/registry-snapshot.json.gz", "registry")
	put("witnesses/registry-key-images.json", "key images")
	const startup = "state/mainnet/snapshot-v2-42.json.gz"
	put(startup, "native startup state")
	if _, err := InspectRelease(root, commit); err == nil {
		t.Fatal("startup snapshot without sidecar accepted")
	}
	put(startup+".sha256", "fixture checksum")
	put("state/mainnet/snapshot-v2-43.json.gz", "second native startup state")
	if _, err := InspectRelease(root, commit); err == nil {
		t.Fatal("multiple startup snapshots accepted")
	}
	if err := os.Remove(filepath.Join(root, "state/mainnet/snapshot-v2-43.json.gz")); err != nil {
		t.Fatal(err)
	}
	r, err := InspectRelease(root, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignRelease(r, priv); err == nil {
		t.Fatal("production signing accepted an unpinned key")
	}
	signed, err := signRelease(r, priv, pub.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.verify(root, commit, pub.Hex()); err != nil {
		t.Fatal(err)
	}
	put(startup+".sha256", "changed checksum")
	if err := signed.verify(root, commit, pub.Hex()); err == nil {
		t.Fatal("modified startup checksum accepted")
	}
	put(startup+".sha256", "fixture checksum")
	if err := signed.Verify(root, commit); err == nil {
		t.Fatal("production verification accepted an unpinned signer")
	}
	if err := signed.verify(root, strings.Repeat("b", 40), pub.Hex()); err == nil {
		t.Fatal("incorrect pinned revision accepted")
	}
	for _, path := range []string{"bin/aperod-node", "state/mainnet/chain.db/CURRENT"} {
		put(path, "changed")
		if err := signed.verify(root, commit, pub.Hex()); err == nil {
			t.Fatalf("tampering accepted: %s", path)
		}
		put(path, path)
	}
	for _, invalid := range []string{
		"network: testnet\n",
		"consensus:\n  non_validator: true\n",
		"network: mainnet\nconsensus:\n  non_validator: true\n  rehearsal_finality_v2_height: 999999999\n",
	} {
		put("config/node.yaml", invalid)
		if _, err := InspectRelease(root, commit); err == nil {
			t.Fatalf("unsuitable release network profile accepted: %s", invalid)
		}
	}
	put("config/node.yaml", profile+"log_level: debug\n")
	if err := signed.verify(root, commit, pub.Hex()); err == nil {
		t.Fatal("otherwise permitted modification to the signed configuration was accepted")
	}
	put("config/node.yaml", profile)
	committeeProfile := profile + "data_dir: state/mainnet\nconsensus:\n  finality_migration_file: " +
		ReleaseHistoricalCommitteeFile + "\n  finality_migration_trust_authorities: [" + HistoricalCommitteeOperatorPublicKeyHex + "]\n"
	put("config/node.yaml", committeeProfile)
	if _, err := InspectRelease(root, commit); err == nil || !strings.Contains(err.Error(), "inspect historical committee statement") {
		t.Fatalf("configured statement may not be omitted: %v", err)
	}
	draft, err := json.Marshal(HistoricalCommitteeDraft())
	if err != nil {
		t.Fatal(err)
	}
	put(releaseHistoricalCommitteePath, string(draft))
	if _, err := InspectRelease(root, commit); err == nil || !strings.Contains(err.Error(), "invalid pinned Mainnet operator attestation") {
		t.Fatalf("unsigned statement accepted: %v", err)
	}
	for _, invalid := range []string{
		profile,
		strings.Replace(committeeProfile, "data_dir: state/mainnet", "data_dir: ../outside", 1),
		strings.Replace(committeeProfile, ReleaseHistoricalCommitteeFile, "../outside.json", 1),
		strings.Replace(committeeProfile, HistoricalCommitteeOperatorPublicKeyHex, strings.Repeat("d", 64), 1),
	} {
		put("config/node.yaml", invalid)
		if _, err := InspectRelease(root, commit); err == nil || !strings.Contains(err.Error(), "bundle-local") {
			t.Fatalf("unbound committee profile accepted: %v", err)
		}
	}
	if err := os.Remove(filepath.Join(root, releaseHistoricalCommitteePath)); err != nil {
		t.Fatal(err)
	}
	put("config/node.yaml", profile)
	for _, path := range []string{
		"state/mainnet/validator.key", "state/mainnet/p2p_identity.key",
		"config/.env", "witnesses/database.dump", "state/mainnet/chain.db/extra.ldb",
	} {
		put(path, "unexpected")
		if _, err := InspectRelease(root, commit); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("unexpected payload file %s: %v", path, err)
		}
		if err := signed.verify(root, commit, pub.Hex()); err == nil {
			t.Fatalf("signed release accepted extra file: %s", path)
		}
		if err := os.Remove(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("config/node.yaml", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := signed.verify(root, commit, pub.Hex()); err == nil {
		t.Fatal("payload with an unexpected symlink accepted")
	}
}
