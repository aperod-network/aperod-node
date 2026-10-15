// Command ring-index-maintenance is an independently approved offline tool,
// never an automatically installed side effect of a node/source update.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const policyPath = "/etc/aperod/ring-index-maintenance/policy.json"
const toolPath = "/usr/local/libexec/aperod-ring-index-maintenance"

type unitPolicy struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files"`
}

type maintenancePolicy struct {
	Schema             int          `json:"schema"`
	ApprovalID         string       `json:"approval_id"`
	ToolSHA256         string       `json:"tool_sha256"`
	Operation          string       `json:"operation"`
	DataDir            string       `json:"data_dir"`
	DBPath             string       `json:"db_path"`
	BackupRoot         string       `json:"backup_root"`
	ConfigPath         string       `json:"config_path"`
	ConfigSHA256       string       `json:"config_sha256"`
	NodeBinary         string       `json:"node_binary"`
	IndexAwareBinaries []string     `json:"index_aware_binaries"`
	Clients            []unitPolicy `json:"clients"`
	Writers            []unitPolicy `json:"writers"`
	Legacy             unitPolicy   `json:"legacy"`
	LegacyBinary       string       `json:"legacy_binary"`
	LegacySHA256       string       `json:"legacy_sha256"`
	AllowColdPrepare   bool         `json:"allow_cold_prepare"`
}

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var approvalRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
var unitRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_@.-]{0,127}\.(service|socket|timer)$`)

func strictPolicy(r io.Reader) (maintenancePolicy, error) {
	var p maintenancePolicy
	decoder := json.NewDecoder(io.LimitReader(r, 128*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("policy trailing content: %v", err)
	}
	if p.Schema != 1 || !approvalRE.MatchString(p.ApprovalID) || !digestRE.MatchString(p.ToolSHA256) || !digestRE.MatchString(p.ConfigSHA256) || !digestRE.MatchString(p.LegacySHA256) {
		return p, fmt.Errorf("invalid approval schema or digest")
	}
	if p.Operation != "downgrade" && p.Operation != "import" && p.Operation != "repair" {
		return p, fmt.Errorf("unsupported operation")
	}
	for _, path := range []string{p.DataDir, p.DBPath, p.BackupRoot, p.ConfigPath, p.NodeBinary, p.LegacyBinary} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return p, fmt.Errorf("canonical absolute paths required")
		}
	}
	if !strings.HasPrefix(p.DBPath, p.DataDir+string(os.PathSeparator)) {
		return p, fmt.Errorf("database must be within full node data directory")
	}
	if overlaps(p.DataDir, p.BackupRoot) {
		return p, fmt.Errorf("backup and live data overlap")
	}
	if len(p.IndexAwareBinaries) == 0 || len(p.IndexAwareBinaries) > 64 {
		return p, fmt.Errorf("index-aware binary approval required")
	}
	for _, d := range p.IndexAwareBinaries {
		if !digestRE.MatchString(d) {
			return p, fmt.Errorf("invalid binary approval")
		}
	}
	if len(p.Clients) == 0 || len(p.Writers) == 0 || len(p.Clients)+len(p.Writers) > 32 {
		return p, fmt.Errorf("explicit client/writer unit sets required")
	}
	units := append(append(append([]unitPolicy{}, p.Clients...), p.Writers...), p.Legacy)
	names := map[string]bool{}
	for _, u := range units {
		if !unitRE.MatchString(u.Name) || names[u.Name] || len(u.Files) == 0 || len(u.Files) > 16 {
			return p, fmt.Errorf("invalid/duplicate unit or absent unit-file approval")
		}
		names[u.Name] = true
		for path, d := range u.Files {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || !digestRE.MatchString(d) {
				return p, fmt.Errorf("invalid unit-file approval")
			}
		}
	}
	if !strings.HasSuffix(p.Legacy.Name, ".service") {
		return p, fmt.Errorf("dedicated legacy service required")
	}
	return p, nil
}

func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func filepathCanonical(path string) (string, error) { return filepath.EvalSymlinks(path) }

// Exact protected file bytes, independently installed outside the checkout,
// authenticate tools and unit/config policies. No caller-supplied digest or
// source-generated self-manifest can substitute for this trust anchor.
func protectedPath(path string, owner uint32) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != owner || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("untrusted policy/tool path")
		}
		if current == "/" {
			return nil
		}
	}
}

func fileDigest(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 512*1024*1024 {
		return "", fmt.Errorf("regular bounded file required")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadApprovedPolicy() (maintenancePolicy, error) {
	return loadApprovedPolicyMode(true)
}

func loadApprovedPolicyMode(checkConfig bool) (maintenancePolicy, error) {
	var p maintenancePolicy
	if err := protectedPath(policyPath, 0); err != nil {
		return p, fmt.Errorf("separate privileged-tool approval unavailable: %w", err)
	}
	f, err := os.Open(policyPath)
	if err != nil {
		return p, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return p, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128*1024 {
		return p, fmt.Errorf("bounded regular policy required")
	}
	p, err = strictPolicy(f)
	if err != nil {
		return p, err
	}
	self, err := os.Executable()
	if err != nil {
		return p, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return p, err
	}
	approvedTool, err := filepath.EvalSymlinks(toolPath)
	if err != nil {
		return p, err
	}
	if self != approvedTool {
		return p, fmt.Errorf("only separately installed tool may execute")
	}
	if err := protectedPath(self, 0); err != nil {
		return p, err
	}
	image, err := os.Open("/proc/self/exe")
	if err != nil {
		return p, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, image)
	image.Close()
	if err != nil {
		return p, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if err != nil {
		return p, err
	}
	if digest != p.ToolSHA256 {
		return p, fmt.Errorf("privileged tool bytes not independently approved")
	}
	if !checkConfig {
		return p, nil
	}
	if err := protectedPath(p.ConfigPath, 0); err != nil {
		return p, err
	}
	digest, err = fileDigest(p.ConfigPath)
	if err != nil {
		return p, err
	}
	if digest != p.ConfigSHA256 {
		return p, fmt.Errorf("approved node configuration changed")
	}
	return p, nil
}

func approvedIndexAware(p maintenancePolicy, path string) error {
	digest, err := fileDigest(path)
	if err != nil {
		return err
	}
	for _, approved := range p.IndexAwareBinaries {
		if digest == approved {
			return nil
		}
	}
	return fmt.Errorf("legacy/unknown writer requires the approved offline fence procedure")
}
