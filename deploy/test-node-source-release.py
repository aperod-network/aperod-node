#!/usr/bin/env python3
"""Real independent Git histories; no network, credentials or system services."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

HELPER = Path(__file__).with_name("node-source-release.sh").resolve()


class NodeSourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="aperod-git-history-")
        self.root = Path(self.temp.name)
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.root),
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}
        self.old = self.repo("old", "old independent root")
        (self.old / "data" / "chain.db").mkdir(parents=True)
        for name in ["data/chain.db/CURRENT", "data/chain.db/MANIFEST-000001",
                     "data/chain.db/000001.ldb", "validator.key",
                     "snapshot.snapshot", "node.yaml", "p2p_bans.json"]:
            path = self.old / name
            path.write_text("synthetic runtime fixture: " + name)
            path.chmod(0o600)
        (self.old / "README").write_text("operator tracked local edit")
        self.before = self.manifest(self.old)
        self.new = self.repo("new", "new independent public root")
        self.new_root = self.git(self.new, "rev-parse", "HEAD").strip()
        (self.new / "README").write_text("new reviewed public revision")
        self.git(self.new, "add", ".")
        self.git(self.new, "commit", "-qm", "second new-public commit")
        self.pin = self.git(self.new, "rev-parse", "HEAD").strip()
        self.destination = self.root / "isolated"

    def tearDown(self):
        self.assertEqual(self.before, self.manifest(self.old), "old checkout/state changed")
        self.temp.cleanup()

    def git(self, repo, *args):
        return subprocess.check_output(
            ["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
             "-C", str(repo), *args], text=True, env=self.env, stderr=subprocess.DEVNULL)

    def repo(self, name, readme):
        path = self.root / name
        path.mkdir()
        subprocess.run(["git", "init", "-qb", "main", str(path)], env=self.env, check=True)
        (path / "README").write_text(readme)
        (path / "go.mod").write_text("module fixture.invalid/node\n\ngo 1.21\n")
        (path / "Makefile").write_text("build:\n\tCGO_ENABLED=0 go build -o build/aperod-node ./cmd/node\n")
        (path / ".gitignore").write_text("/build/\n")
        (path / "cmd/node").mkdir(parents=True)
        (path / "cmd/node/main.go").write_text(
            'package main\nimport "fmt"\nfunc main(){fmt.Println("independent-public-node")}\n')
        self.git(path, "add", ".")
        self.git(path, "commit", "-qm", readme)
        return path

    @staticmethod
    def manifest(path):
        return {str(p.relative_to(path)): (hashlib.sha256(p.read_bytes()).hexdigest(),
                                         p.stat().st_mode, p.stat().st_uid, p.stat().st_gid)
                for p in path.rglob("*") if p.is_file()}

    def shell(self, body, *args, env=None):
        return subprocess.run(["bash", "-c", 'source "$1"; shift; ' + body,
                               "fixture", str(HELPER), *map(str, args)],
                              env=env or self.env, capture_output=True, text=True)

    def fetch(self, origin=None, expected=None, pin=None, root=None, destination=None):
        return self.shell('if node_source_fetch_verified "$@"; then exit 0; else exit 41; fi',
                          origin or self.new, expected or self.new, pin or self.pin,
                          root or self.new_root, destination or self.destination)

    def test_new_history_fetch_preserves_old_clone_and_every_runtime_byte(self):
        self.assertNotEqual(self.git(self.old, "rev-list", "--max-parents=0", "HEAD").strip(),
                            self.new_root)
        result = self.fetch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git(self.destination, "rev-parse", "HEAD").strip(), self.pin)
        self.assertEqual((self.destination / "README").read_text(), "new reviewed public revision")
        self.assertEqual(self.git(self.destination, "status", "--porcelain"), "")
        # Full SHA pin, not merely the latest HEAD: reviewed ancestor remains valid.
        result = self.fetch(pin=self.new_root, destination=self.root / "pinned-ancestor")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git(self.root / "pinned-ancestor", "rev-parse", "HEAD").strip(),
                         self.new_root)

    def test_wrong_origin_rejected_before_destination_creation(self):
        self.assertNotEqual(self.fetch(origin=self.old).returncode, 0)
        self.assertFalse(self.destination.exists())

    def test_missing_commit_rejected_in_conditional_shell_context(self):
        self.assertNotEqual(self.fetch(pin="f" * 40).returncode, 0)

    def test_wrong_history_root_rejected(self):
        old_root = self.git(self.old, "rev-parse", "HEAD").strip()
        self.assertNotEqual(self.fetch(root=old_root).returncode, 0)

    def test_ambient_git_config_cannot_mutate_the_old_checkout(self):
        env = {**self.env, "GIT_CONFIG": str(self.old / ".git/config"),
               "GIT_COMMON_DIR": str(self.old / ".git"), "GIT_DIR": str(self.old / ".git"),
               "GIT_WORK_TREE": str(self.old), "GIT_SHALLOW_FILE": str(self.old / ".git/shallow")}
        result = self.shell('if node_source_fetch_verified "$@"; then exit 0; else exit 41; fi',
                            self.new, self.new, self.pin, self.new_root, self.destination, env=env)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_git_init_failure_does_not_fall_back_to_parent_repository(self):
        shim = self.root / "shim"
        shim.mkdir()
        real_git = shutil.which("git")
        wrapper = shim / "git"
        wrapper.write_text(f'#!/bin/bash\nfor a in "$@"; do [[ "$a" != init ]] || exit 71; done\nexec "{real_git}" "$@"\n')
        wrapper.chmod(0o755)
        env = {**self.env, "PATH": str(shim) + ":" + self.env["PATH"]}
        parent_config = (self.new / ".git/config").read_bytes()
        result = self.shell('if node_source_fetch_verified "$@"; then exit 0; else exit 41; fi',
                            self.new, self.new, self.pin, self.new_root, self.new / "child", env=env)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(parent_config, (self.new / ".git/config").read_bytes())

    def test_side_branch_commit_is_not_an_approved_main_ancestor(self):
        self.git(self.new, "checkout", "-qb", "unreviewed")
        (self.new / "README").write_text("unreviewed")
        self.git(self.new, "commit", "-qam", "side branch")
        pin = self.git(self.new, "rev-parse", "HEAD").strip()
        self.git(self.new, "checkout", "-q", "main")
        self.assertNotEqual(self.fetch(pin=pin).returncode, 0)

    def test_existing_checkout_and_symlink_ancestor_are_never_reused(self):
        self.assertNotEqual(self.fetch(destination=self.old).returncode, 0)
        linked = self.root / "linked"
        linked.symlink_to(self.old, target_is_directory=True)
        self.assertNotEqual(self.fetch(destination=linked / "new").returncode, 0)
        self.assertFalse((self.old / "new").exists())

    def test_partial_existing_state_is_not_treated_as_fresh_install(self):
        self.assertNotEqual(self.shell('node_source_fresh_state_guard "$1"',
                                     self.old / "data").returncode, 0)
        self.assertEqual(self.shell('node_source_fresh_state_guard "$1"',
                                   self.root / "absent-data").returncode, 0)
        empty = self.root / "empty-data"
        empty.mkdir()
        self.assertEqual(self.shell('node_source_fresh_state_guard "$1"', empty).returncode, 0)

    def test_baseline_approval_is_bound_to_both_artifact_identities(self):
        binary = self.root / "running-node"
        binary.write_text("synthetic baseline binary")
        env = {**self.env, "APEROD_NODE_SOURCE_COMMIT": self.pin,
               "APEROD_BASELINE_APPROVED_SOURCE": self.pin,
               "APEROD_BASELINE_APPROVED_BINARY_SHA256": hashlib.sha256(binary.read_bytes()).hexdigest()}
        self.assertEqual(self.shell('node_source_baseline_guard "$1"', binary, env=env).returncode, 0)
        env["APEROD_BASELINE_APPROVED_SOURCE"] = self.new_root
        self.assertNotEqual(self.shell('node_source_baseline_guard "$1"', binary, env=env).returncode, 0)
        env["APEROD_BASELINE_APPROVED_SOURCE"] = self.pin
        binary.write_text("changed live baseline")
        self.assertNotEqual(self.shell('node_source_baseline_guard "$1"', binary, env=env).returncode, 0)

    def test_real_static_fixture_build_after_verified_fetch(self):
        self.assertIsNotNone(shutil.which("go"), "Go fixture build is required, not silently skipped")
        self.assertEqual(self.fetch().returncode, 0)
        env = {**self.env, "CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOTELEMETRY": "off"}
        subprocess.run(["make", "CGO_ENABLED=0", "build"], cwd=self.destination, env=env,
                       check=True, capture_output=True)
        binary = self.destination / "build/aperod-node"
        info = subprocess.check_output(["go", "version", "-m", str(binary)], env=env, text=True)
        self.assertIn("CGO_ENABLED=0", info)
        self.assertIn("vcs.revision=" + self.pin, info)
        self.assertIn("vcs.modified=false", info)
        self.assertEqual(subprocess.check_output([str(binary)], text=True).strip(),
                         "independent-public-node")
        self.assertIsNotNone(shutil.which("readelf"))
        self.assertNotIn("INTERP", subprocess.check_output(["readelf", "-l", str(binary)], text=True))
        env["APEROD_NODE_SOURCE_COMMIT"] = self.pin
        checked = self.shell('node_source_candidate_guard "$1" "$2"', self.destination, binary, env=env)
        self.assertEqual(checked.returncode, 0, checked.stderr)
        (self.destination / "README").write_text("unapproved build overlay")
        self.assertNotEqual(self.shell('node_source_candidate_guard "$1" "$2"',
                                     self.destination, binary, env=env).returncode, 0)

    def test_fresh_validator_install_resolves_genesis_and_bootstrap_without_old_checkout(self):
        # Real pinned local source and real builds; privileged host commands are
        # explicit stubs. Every filesystem target is redirected under this job.
        (self.new / "config").mkdir()
        genesis = "network: synthetic-install-fixture\n"
        (self.new / "config/genesis-testnet.yaml").write_text(genesis)
        (self.new / "cmd/aperod").mkdir()
        (self.new / "cmd/aperod/main.go").write_text(
            'package main\nimport("fmt";"strings")\n'
            'func main(){fmt.Println("Private: "+strings.Repeat("1",64));'
            'fmt.Println("Public: "+strings.Repeat("2",64))}\n')
        (self.new / "Makefile").write_text(
            "deps:\n\ttrue\nbuild:\n\tgo build -o build/aperod-node ./cmd/node\n"
            "\tgo build -o build/aperod ./cmd/aperod\n")
        self.git(self.new, "add", ".")
        self.git(self.new, "commit", "-qm", "synthetic fresh-validator source")
        pin = self.git(self.new, "rev-parse", "HEAD").strip()
        deploy = HELPER.parent

        def sandbox(name, missing=None):
            base = self.root / name
            host = base / "host"
            bootstrap = base / "bootstrap"
            mocks = base / "mocks"
            for path in [bootstrap, mocks, host / "etc/systemd/system",
                         host / "usr/local/bin", host / "var/tmp", host / "opt/aperod"]:
                path.mkdir(parents=True, exist_ok=True)
            (host / "etc/os-release").write_text('NAME="Ubuntu"\n')
            files = ["install-validator.sh", "node-source-release.sh", "source-safe-guard.sh",
                     "ensure-dropin.sh", "gomemlimit-policy.sh", "aperod-node-watchdog.sh",
                     "aperod-node-watchdog.service", "aperod-node-watchdog.timer"]
            for name in files:
                if name == missing:
                    continue
                text = (deploy / name).read_text()
                for absolute in ["/usr/local", "/var/lib/aperod", "/opt/aperod", "/var/tmp", "/etc"]:
                    text = text.replace(absolute, str(host) + absolute)
                if name == "node-source-release.sh":
                    text = text.replace("https://github.com/aperod-network/aperod-node.git",
                                        str(self.new))
                    text = text.replace("4bfab534469dc61296910d968ab13cab27554a97", self.new_root)
                (bootstrap / name).write_text(text)
            stubs = {
                "id": 'printf "0\\n"\n',
                "apt-get": 'echo apt >> "$HOST/commands"\n',
                "useradd": "exit 0\n",
                "chown": "exit 0\n",
                "ufw": "exit 0\n",
                "curl": 'printf "203.0.113.10"\n',
                "sleep": "exit 0\n",
                "xxd": '''[[ "$*" == "-r -p" ]] || exit 81
python3 -c 'import sys; sys.stdout.buffer.write(bytes.fromhex(sys.stdin.read()))'
''',
                "systemctl": '''echo "$*" >> "$HOST/systemctl.calls"
if [[ "$*" == "start aperod-node" ]]; then
  test -s "$HOST/etc/aperod/genesis-testnet.yaml" || exit 31
  test -s "$HOST/etc/aperod/node.yaml" || exit 32
  test -x "$HOST/usr/local/bin/aperod-node" || exit 33
  test -s "$HOST/etc/systemd/system/aperod-node.service" || exit 34
  test -s "$HOST/etc/systemd/system/aperod-node.service.d/timeout.conf" || exit 35
  test -s "$HOST/etc/systemd/system/aperod-node.service.d/gomemlimit.conf" || exit 36
fi
exit 0
'''
            }
            for name, body in stubs.items():
                file = mocks / name
                file.write_text("#!/bin/bash\nset -e\n" + body)
                file.chmod(0o755)
            env = {**self.env, "PATH": str(mocks) + ":" + str(host / "usr/local/bin") + ":" + self.env["PATH"],
                   "HOST": str(host), "APEROD_NODE_SOURCE_COMMIT": pin,
                   "APEROD_REWARD_ADDRESS": "synthetic-fixture-address-" * 5,
                   "GOTOOLCHAIN": "local", "GOTELEMETRY": "off"}
            result = subprocess.run(["bash", str(bootstrap / "install-validator.sh")], env=env,
                                    text=True, capture_output=True, stdin=subprocess.DEVNULL,
                                    start_new_session=True, timeout=120)
            return host, result

        host, result = sandbox("fresh-success")
        self.assertEqual(result.returncode, 0, result.stdout[-2500:] + result.stderr[-2500:])
        self.assertEqual((host / "etc/aperod/genesis-testnet.yaml").read_text(), genesis)
        self.assertEqual(list((host / "opt/aperod").iterdir()), [])
        for name in ["aperod-node-watchdog.service", "aperod-node-watchdog.timer"]:
            self.assertTrue((host / "etc/systemd/system" / name).is_file())
        self.assertTrue((host / "usr/local/bin/aperod-node-watchdog.sh").is_file())
        calls = (host / "systemctl.calls").read_text()
        self.assertIn("start aperod-node", calls)
        self.assertIn("enable --now aperod-node-watchdog.timer", calls)
        self.assertEqual(list((host / "var/tmp").iterdir()), [], "owned source job leaked")
        broken, failed = sandbox("fresh-missing-bootstrap", missing="ensure-dropin.sh")
        self.assertNotEqual(failed.returncode, 0)
        self.assertFalse((broken / "commands").exists(), "missing bootstrap reached apt")
        self.assertFalse((broken / "etc/aperod/validator.key").exists())
        self.assertFalse((broken / "usr/local/bin/aperod-node").exists())
        (self.new / "config/genesis-testnet.yaml").unlink()
        self.git(self.new, "commit", "-qam", "synthetic candidate without genesis")
        pin = self.git(self.new, "rev-parse", "HEAD").strip()
        no_genesis, refused = sandbox("fresh-missing-genesis")
        self.assertNotEqual(refused.returncode, 0)
        self.assertFalse((no_genesis / "etc/aperod/validator.key").exists())
        self.assertFalse((no_genesis / "usr/local/bin/aperod-node").exists())
        self.assertEqual(list((no_genesis / "var/tmp").iterdir()), [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
