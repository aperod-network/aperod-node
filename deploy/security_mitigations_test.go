package deploy_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestJoinRejectsRemotePlainHTTPBeforeTouchingNodeData(t *testing.T) {
	cmd := exec.Command("bash", "aperod-join.sh", "203.0.113.8:8545")
	cmd.Env = append(os.Environ(),
		"APEROD_NODE_YAML=/path/that/does/not/exist/node.yaml",
		"APEROD_NODE_CONFIG_SH=/path/that/does/not/exist/node-config.sh",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("remote plaintext sync unexpectedly succeeded")
	}
	if !strings.Contains(string(output), "Удалённая загрузка по HTTP запрещена") {
		t.Fatalf("remote HTTP rejection was not the first failure:\n%s", output)
	}
}

func TestJoinTunnelRequiresPinnedHostKey(t *testing.T) {
	script, err := os.ReadFile("aperod-join.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if !strings.Contains(text, "StrictHostKeyChecking=yes") {
		t.Fatal("SSH tunnel does not require a pre-pinned host key")
	}
	if strings.Contains(text, "StrictHostKeyChecking=accept-new") {
		t.Fatal("SSH tunnel still trusts an unverified first-use host key")
	}
}

func TestJoinValidatesStagedChainBeforeChangingNodeData(t *testing.T) {
	script, err := os.ReadFile("aperod-join.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"systemctl is-active --quiet aperod-node",
		"отказывается заменять существующую цепь",
		"unsupported archive member type",
		"--check-store --data-dir=",
		"--max-missing=0",
		"--check-snapshot --data-dir=",
		"--no-chaindb: существующие chain.db и snapshot не удаляются и не заменяются",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("join installer is missing staged-bootstrap safety check %q", required)
		}
	}
	if strings.Contains(text, `rm -rf "${DATA_DIR}/chain.db"`) ||
		strings.Contains(text, `rm -f "${DATA_DIR}"/snapshot-v2-*.json.gz`) ||
		strings.Contains(text, "unset-field consensus.non_validator") {
		t.Fatal("join installer still deletes existing chain/snapshot data or removes the safe-mode guard")
	}
	storeCheck := strings.Index(text, `"${NODE_BIN}" --check-store`)
	snapshotCheck := strings.Index(text, `"${NODE_BIN}" --check-snapshot`)
	publishDB := strings.Index(text, `mv -nT "${JOIN_STAGE}/chain.db" "${DATA_DIR}/chain.db"`)
	if storeCheck < 0 || snapshotCheck < 0 || publishDB < 0 ||
		storeCheck > publishDB || snapshotCheck > publishDB {
		t.Fatal("join installer promotes staged data before validating both the chain and matching snapshot")
	}
}

func TestInstallerWalletSetupNeverPersistsRawPrivateKey(t *testing.T) {
	script, err := os.ReadFile("install-node.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		`aperod wallet create --output "${WALLET_FILE}"`,
		`aperod wallet restore --output "${WALLET_FILE}"`,
		`[[ -s "${WALLET_FILE}" ]] || die`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("secure wallet setup missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"PRIVKEY",
		"wallet import",
		`echo -n "${PRIVKEY}"`,
		"aperod wallet address",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("unsafe or unsupported wallet setup remains: %q", forbidden)
		}
	}
}

func TestGoToolchainInstallersVerifyOfficialChecksumsBeforeExtraction(t *testing.T) {
	for _, path := range []string{"install-node.sh", "install-validator.sh"} {
		script, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(script)
		for _, required := range []string{
			`GO_VERSION="1.26.11"`,
			`9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec`,
			`6c2f29b458b5add0835f18b776f69c8881a6d88428f1484a6da55755e620757e`,
			`mktemp /tmp/aperod-go.XXXXXX.tar.gz`,
			`sha256sum --check --status`,
		} {
			if !strings.Contains(text, required) {
				t.Errorf("%s is missing verified Go installation step %q", path, required)
			}
		}
		if strings.Index(text, "sha256sum --check --status") > strings.Index(text, `tar -C /usr/local -xzf "$GO_ARCHIVE"`) {
			t.Errorf("%s extracts the Go archive before checking its checksum", path)
		}
	}
}

func TestMonitoringEntrypointUsesExistingVerifiedBinaries(t *testing.T) {
	script, err := os.ReadFile("install-monitoring.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
if !strings.Contains(text, `exec bash "$SCRIPT_DIR/setup-monitoring-ovh.sh" "$@"`) {
t.Fatal("compatibility entrypoint must delegate only to the adjacent checked-out script")
}
setup, err := os.ReadFile("setup-monitoring-ovh.sh")
if err != nil {
t.Fatal(err)
}
text += "\n" + string(setup)
	for _, required := range []string{
"/usr/local/bin/prometheus --version",
"/usr/local/bin/prometheus-alertmanager --version",
"/usr/local/bin/prometheus-node-exporter --version",
"The verified monitoring binary is missing",
"Expected Prometheus 3.16.0",
"Expected Alertmanager 0.35.1",
"Expected node_exporter 1.13.1",
	} {
		if !strings.Contains(text, required) {
t.Errorf("monitoring setup is missing an existing-binary verification guard %q", required)
		}
	}
// Archives are no longer downloaded by this entrypoint. Keep the absence of
// extraction and mutable release URLs covered, not obsolete checksum strings.
for _, forbidden := range []string{
"tar -", "wget ", "/releases/download/", "/releases/latest/",
"curl |", "| bash", "| sudo bash",
	} {
if strings.Contains(text, forbidden) {
t.Errorf("monitoring setup reintroduced an unchecked archive or remote script: %q", forbidden)
		}
	}
	if strings.Contains(text, `-O /tmp/node_exporter.tar.gz`) ||
		strings.Contains(text, `-O /tmp/nginx_exporter.tar.gz`) {
		t.Fatal("monitoring installer still downloads root-owned archives to predictable /tmp paths")
	}
}

func TestPublicValidatorGuidesRequireVerifiedMainnetReadiness(t *testing.T) {
	validators, err := os.ReadFile("VALIDATORS.md")
	if err != nil {
		t.Fatal(err)
	}
	mainnet, err := os.ReadFile("MAINNET.md")
	if err != nil {
		t.Fatal(err)
	}
	publicReadme, err := os.ReadFile("README-public.md")
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	// The published tree may retain an older deploy/README-public.md while the
	// current public-facing copy is at the repository root. Prefer that root
	// copy when it contains the current mainnet safety notice.
	publicRootReadme, rootErr := os.ReadFile("../README.md")
	if rootErr == nil && strings.Contains(string(publicRootReadme), "current `install-validator.sh` is not a verified mainnet installer") {
		publicReadme = publicRootReadme
	} else if err != nil {
		t.Fatal(err)
	}
	investors, err := os.ReadFile("INVESTORS.md")
	if err != nil {
		t.Fatal(err)
	}
	morningReport, err := os.ReadFile("aperod-morning-report.sh")
	if err != nil {
		t.Fatal(err)
	}
	systemCheck, err := os.ReadFile("check-system.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(validators), string(investors)} {
		for _, unsafe := range []string{
			"raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash",
			"raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/uninstall-validator.sh | sudo bash",
		} {
			if strings.Contains(text, unsafe) {
				t.Errorf("public validator guide still pipes mutable-main script to root: %q", unsafe)
			}
		}
	}
	for _, text := range []string{string(morningReport), string(systemCheck)} {
		for _, unsafe := range []string{
			"https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/aperod-morning-report.sh",
			"curl https://rclone.org/install.sh | sudo bash",
		} {
			if strings.Contains(text, unsafe) {
				t.Errorf("public operator instructions still run mutable remote code as root: %q", unsafe)
			}
		}
	}
	validatorText := strings.Join(strings.Fields(string(validators)), " ")
	for _, required := range []string{
		"[mainnet onboarding](MAINNET.md)",
		"A verified self-service mainnet bootstrap release is not yet published.",
		"must not be used as a mainnet installer",
	} {
		if !strings.Contains(validatorText, required) {
			t.Errorf("validator guide is missing the mainnet readiness boundary %q", required)
		}
	}
	for _, forbidden := range []string{
		"APEROD_NETWORK=testnet",
		"sudo env APEROD_NETWORK=",
	} {
		if strings.Contains(validatorText, forbidden) {
			t.Errorf("validator guide still advertises a non-mainnet install command: %q", forbidden)
		}
	}
	for _, required := range []string{
		"existing Aperod mainnet",
		"verified, self-service",
		"canonical genesis",
		"consistent, hash-verified bootstrap snapshot",
		"on-chain stake",
		"active set",
		"source-pinning shell block is not a mainnet installer",
		"Preserve that directory and its contents",
		"child shell (`(...)`)",
	} {
		if !strings.Contains(string(mainnet), required) {
			t.Errorf("mainnet onboarding guide is missing required release or activation guidance %q", required)
		}
	}
	for _, required := range []string{
		"current `install-validator.sh` is not a verified mainnet installer",
		"does not make it one",
		"preserve it rather than deleting it",
		"child shell (`(...)`)",
	} {
		if !strings.Contains(string(publicReadme), required) {
			t.Errorf("public node guide is missing installation safety guidance %q", required)
		}
	}
	if !strings.Contains(string(investors), "VALIDATORS.md#setup-guide") {
		t.Fatal("investor quick-start does not link to the validator setup")
	}
	if !strings.Contains(string(morningReport), "install -o root -g root -m 0755 deploy/aperod-morning-report.sh") {
		t.Fatal("morning-report installation does not use the checked-out source")
	}
	if !strings.Contains(string(systemCheck), "подписанного репозитория") {
		t.Fatal("system check recommends no trusted package source for rclone")
	}
}
