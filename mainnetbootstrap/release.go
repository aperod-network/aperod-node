package mainnetbootstrap

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	chaincrypto "github.com/aperod/aperod/crypto"
)

const releaseSchema = "aperod-mainnet-release-v1"
const releaseDomain = "aperod/mainnet-release/v1\x00"

const ReleaseHistoricalCommitteeFile = "finality-committee-operator-v2.json"
const releaseHistoricalCommitteePath = "state/mainnet/" + ReleaseHistoricalCommitteeFile

var requiredReleaseFiles = []string{
	"bin/aperod-node", "config/node.yaml", "config/genesis.yaml",
	"witnesses/bootstrap-signed.json", "witnesses/avm-signed.json",
	"witnesses/aperod-authority-fork.json", "witnesses/aperod-protocol-sale-opening.json",
	"witnesses/aperod-lpod-v3-witness.json",
	"state/mainnet/chain.db/CURRENT",
}

var releaseDBFile = regexp.MustCompile(`^(CURRENT|CURRENT\.bak|LOCK|LOG(\.old)?|MANIFEST-[0-9]+|[0-9]+\.(ldb|sst|log))$`)
var releaseStartupSnapshot = regexp.MustCompile(`^snapshot-v2-[1-9][0-9]{0,18}\.json\.gz(\.sha256)?$`)

// Reject copied runtime trees and stray files before either signing or
// accepting a public bundle. These rules constrain paths, not file contents.
func checkReleaseLayout(root string) error {
	files := map[string]bool{
		"bin/aperod": true, "config/README.md": true,
		"witnesses/registry-snapshot.json.gz": true, "witnesses/registry-key-images.json": true,
		releaseHistoricalCommitteePath: true,
	}
	for _, path := range requiredReleaseFiles {
		files[path] = true
	}
	var startupSnapshot, startupChecksum string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			switch rel {
			case ".", "bin", "config", "witnesses", "state", "state/mainnet", "state/mainnet/chain.db":
				return nil
			}
		} else if entry.Type().IsRegular() && (files[rel] ||
			(filepath.ToSlash(filepath.Dir(rel)) == "state/mainnet/chain.db" && releaseDBFile.MatchString(entry.Name()))) {
			return nil
		} else if entry.Type().IsRegular() && filepath.ToSlash(filepath.Dir(rel)) == "state/mainnet" &&
			releaseStartupSnapshot.MatchString(entry.Name()) {
			if name, isChecksum := strings.CutSuffix(entry.Name(), ".sha256"); isChecksum && startupChecksum == "" {
				startupChecksum = name
				return nil
			} else if !isChecksum && startupSnapshot == "" {
				startupSnapshot = name
				return nil
			}
		}
		return fmt.Errorf("file or directory is not allowed in a Mainnet release payload: %s", rel)
	})
	if err != nil {
		return err
	}
	if startupSnapshot != startupChecksum {
		return errors.New("Mainnet release startup snapshot and SHA-256 sidecar must be a matched pair")
	}
	return nil
}

func checkReleaseNetworkProfile(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, "config", "node.yaml"))
	if err != nil {
		return err
	}
	var profile struct {
		Network   string `yaml:"network"`
		DataDir   string `yaml:"data_dir"`
		Consensus struct {
			RehearsalFinalityV2Height         uint64   `yaml:"rehearsal_finality_v2_height"`
			FinalityMigrationFile             string   `yaml:"finality_migration_file"`
			FinalityMigrationTrustAuthorities []string `yaml:"finality_migration_trust_authorities"`
		} `yaml:"consensus"`
	}
	if err := yaml.Unmarshal(raw, &profile); err != nil {
		return errors.New("invalid node.yaml in Mainnet release payload")
	}
	if profile.Network != "mainnet" {
		return errors.New("Mainnet release node.yaml must set network: mainnet")
	}
	if profile.Consensus.RehearsalFinalityV2Height != 0 {
		return errors.New("Mainnet releases cannot use rehearsal_finality_v2_height")
	}
	path := filepath.Join(root, filepath.FromSlash(releaseHistoricalCommitteePath))
	_, statErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect historical committee statement: %w", statErr)
	}
	migration := profile.Consensus.FinalityMigrationFile
	trust := profile.Consensus.FinalityMigrationTrustAuthorities
	if migration == "" && len(trust) == 0 && errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	if profile.DataDir != "state/mainnet" || migration != ReleaseHistoricalCommitteeFile ||
		len(trust) != 1 || trust[0] != HistoricalCommitteeOperatorPublicKeyHex {
		return errors.New("Mainnet release historical committee must configure the bundle-local data directory, statement and pinned operator")
	}
	if _, err := ReadReleaseHistoricalCommittee(root); err != nil {
		return err
	}
	return nil
}

// ReleaseStatement authenticates a frozen bundle. Its signed manifest must
// live outside the payload directory to avoid a self-referential digest.
type ReleaseStatement struct {
	Schema        string `json:"schema"`
	ChainID       string `json:"chain_id"`
	SourceCommit  string `json:"source_commit"`
	PayloadSHA256 string `json:"payload_tree_sha256"`
}

type SignedRelease struct {
	Release   ReleaseStatement `json:"release"`
	Signature string           `json:"signature"`
}

func releaseHash(r ReleaseStatement) (chaincrypto.Hash32, error) {
	if r.Schema != releaseSchema || r.ChainID != ChainID ||
		!releaseHex(r.SourceCommit, 20) || !releaseHex(r.PayloadSHA256, 32) {
		return chaincrypto.Hash32{}, errors.New("invalid Mainnet release statement")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return chaincrypto.Hash32{}, err
	}
	return chaincrypto.HashBytes([]byte(releaseDomain), raw), nil
}

func releaseHex(value string, size int) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == size && hex.EncodeToString(data) == value
}

// InspectRelease measures the full payload (including the bootstrap bytes).
// Its caller must stage and verify chain state separately before signing.
func InspectRelease(root, revision string) (ReleaseStatement, error) {
	if err := checkReleaseLayout(root); err != nil {
		return ReleaseStatement{}, err
	}
	if err := checkReleaseNetworkProfile(root); err != nil {
		return ReleaseStatement{}, err
	}
	digest, err := SnapshotTreeSHA256(root)
	if err != nil {
		return ReleaseStatement{}, err
	}
	for _, rel := range requiredReleaseFiles {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return ReleaseStatement{}, fmt.Errorf("release payload is missing a nonempty regular file: %s", rel)
		}
	}
	// goleveldb can create a missing LOCK even for a read-only open.
	lock, err := os.Lstat(filepath.Join(root, "state", "mainnet", "chain.db", "LOCK"))
	if err != nil || !lock.Mode().IsRegular() {
		return ReleaseStatement{}, errors.New("closed release snapshot must already contain a regular LOCK file")
	}
	r := ReleaseStatement{Schema: releaseSchema, ChainID: ChainID, SourceCommit: revision, PayloadSHA256: digest}
	_, err = releaseHash(r)
	return r, err
}

func SignRelease(r ReleaseStatement, key signer) (SignedRelease, error) {
	return signRelease(r, key, BootstrapSignerPublicKeyHex)
}

func signRelease(r ReleaseStatement, key signer, authority string) (SignedRelease, error) {
	if key == nil || key.Public().Hex() != authority {
		return SignedRelease{}, errors.New("release signing key does not match the pinned Mainnet operator")
	}
	hash, err := releaseHash(r)
	if err != nil {
		return SignedRelease{}, err
	}
	sig, err := key.Sign(hash)
	return SignedRelease{Release: r, Signature: hex.EncodeToString(sig)}, err
}

func (s SignedRelease) Verify(root, expectedRevision string) error {
	return s.verify(root, expectedRevision, BootstrapSignerPublicKeyHex)
}

func (s SignedRelease) verify(root, expectedRevision, authority string) error {
	hash, err := releaseHash(s.Release)
	if err != nil {
		return err
	}
	if s.Release.SourceCommit != expectedRevision {
		return errors.New("release source revision does not match the expected commit")
	}
	pubBytes, err := hex.DecodeString(authority)
	if err != nil {
		return err
	}
	pub, err := chaincrypto.ValidatorPubKeyFromBytes(pubBytes)
	if err != nil {
		return err
	}
	sig, _ := hex.DecodeString(s.Signature)
	if !releaseHex(s.Signature, 64) || !pub.Verify(hash, sig) {
		return errors.New("invalid Mainnet operator release signature")
	}
	measured, err := InspectRelease(root, expectedRevision)
	if err != nil {
		return err
	}
	if measured != s.Release {
		return errors.New("release payload differs from the signed release")
	}
	return nil
}
