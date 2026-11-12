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

func TestPublicValidatorGuidesRequirePinnedInstallerSource(t *testing.T) {
	validators, err := os.ReadFile("VALIDATORS.md")
	if err != nil {
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
	for _, required := range []string{
		"APEROD_NODE_SOURCE_COMMIT",
		`[[ ! "$APEROD_NODE_SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]]`,
		"git checkout --detach",
		`git cat-file -e "${APEROD_NODE_SOURCE_COMMIT}^{commit}"`,
		"test \"$(git rev-parse HEAD)\" = \"$APEROD_NODE_SOURCE_COMMIT\"",
		"sudo env APEROD_NODE_SOURCE_COMMIT=",
	} {
		if !strings.Contains(string(validators), required) {
			t.Errorf("pinned installation procedure is missing %q", required)
		}
	}
	if !strings.Contains(string(investors), "VALIDATORS.md#setup-guide") {
		t.Fatal("investor quick-start does not link to the pinned validator setup")
	}
	if !strings.Contains(string(morningReport), "install -o root -g root -m 0755 deploy/aperod-morning-report.sh") {
		t.Fatal("morning-report installation does not use the checked-out source")
	}
	if !strings.Contains(string(systemCheck), "подписанного репозитория") {
		t.Fatal("system check recommends no trusted package source for rclone")
	}
}
