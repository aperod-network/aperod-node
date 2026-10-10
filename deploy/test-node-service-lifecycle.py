#!/usr/bin/env python3
"""Host-service evidence in the disposable systemd guest; no production I/O."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.request

ROOT = Path("/fixture")
DEPLOY = Path("/reviewed-deploy")
BINARY = Path("/usr/local/bin/aperod-node")
OLD = Path("/opt/aperod/blockchain")
MODE = sys.argv[1]
assert os.geteuid() == 0 and Path("/run/systemd/system").is_dir()
assert os.environ.get("container") == "docker", "Must run in the isolated guest"
ROOT.mkdir()
CONTROL = ROOT / "build-control"
CONTROL.mkdir()
env = {**os.environ, "HOME": "/root", "CGO_ENABLED": "0", "GOTOOLCHAIN": "local",
       "SKIP_PEER_CHECK": "1", "HEALTH_MAX_ATTEMPTS": "4", "HEALTH_WAIT_SECS": "1"}
for key in list(env):
    if key.startswith("GIT_") or any(word in key for word in ("TOKEN", "PASSWORD", "SECRET")):
        env.pop(key)


def run(*args, ok=True, input=None, custom=None):
    result = subprocess.run(list(map(str, args)), env=custom or env, input=input,
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            timeout=450)
    if ok and result.returncode:
        raise AssertionError(f"{args}: exit {result.returncode}\n{result.stdout}")
    return result


def git(path, *args):
    return run("git", "-c", "user.name=Lifecycle fixture", "-c",
               "user.email=lifecycle@example.invalid", "-C", path, *args).stdout.strip()


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def manifest(path):
    return {str(p.relative_to(path)): [digest(p), p.stat().st_mode, p.stat().st_uid,
                                    p.stat().st_gid]
            for p in path.rglob("*") if p.is_file()}


def proof(label):
    print("PASS:", label, flush=True)


def source(name):
    path = ROOT / name
    (path / "cmd/node").mkdir(parents=True)
    (path / "cmd/aperod").mkdir(parents=True)
    (path / "cmd/store").mkdir(parents=True)
    (path / "config").mkdir()
    (path / "config/genesis-testnet.yaml").write_text("chain_id: synthetic-lifecycle\n")
    for filename, command in (("service.go", "node"), ("cli.go", "aperod"), ("store.go", "store")):
        shutil.copyfile(DEPLOY / "fixtures/node-lifecycle" / filename, path / f"cmd/{command}/main.go")
    (path / "go.mod").write_text("module fixture.invalid/node\n\ngo 1.21\n\n"
                               "require github.com/syndtr/goleveldb v1.0.0\n")
    (path / "Makefile").write_text(
        "deps:\n\tgo mod download\nbuild:\n\tmkdir -p build\n"
        "\tgo build -o build/aperod-node ./cmd/node\n"
        "\tgo build -o build/aperod ./cmd/aperod\n"
        "\tgo build -o build/store ./cmd/store\n")
    (path / ".gitignore").write_text("/build/\n")
    run("go", "-C", path, "mod", "tidy")
    run("git", "init", "-qb", "main", path)
    git(path, "add", ".")
    git(path, "commit", "-qm", "Independent synthetic root " + name)
    return path


new = source("public")
root_sha = git(new, "rev-parse", "HEAD")
node_source = new / "cmd/node/main.go"
node_source.write_text(node_source.read_text().replace("fixture-old", "fixture-new"))
git(new, "add", ".")
git(new, "commit", "-qm", "Reviewed synthetic candidate")
pin = git(new, "rev-parse", "HEAD")
node_source.write_text(node_source.read_text().replace("const unhealthy = false", "const unhealthy = true"))
git(new, "add", ".")
git(new, "commit", "-qm", "Synthetic health-failure candidate")
bad_pin = git(new, "rev-parse", "HEAD")
assert root_sha != pin != bad_pin
node_source.write_text(node_source.read_text().replace("const unhealthy = true", "const unhealthy = false"))
makefile = new / "Makefile"
makefile.write_text(makefile.read_text() + "\ttouch /fixture/build-control/build-ready\n"
                    "\twhile [ ! -f /fixture/build-control/build-release ]; do sleep 0.1; done\n")
git(new, "add", ".")
git(new, "commit", "-qm", "Synthetic approval-race build barrier")
race_pin = git(new, "rev-parse", "HEAD")
# Modify trusted fixture identities in a COPIED bootstrap helper only. Every
# production origin/root/pin/baseline/provenance guard is otherwise executed.
bootstrap = ROOT / "bootstrap"
shutil.copytree(DEPLOY, bootstrap)
helper = bootstrap / "node-source-release.sh"
text = helper.read_text()
assert text.count('APEROD_PUBLIC_NODE_ORIGIN="https://github.com/aperod-network/aperod-node.git"') == 1
assert text.count('APEROD_PUBLIC_NODE_ROOT="4bfab534469dc61296910d968ab13cab27554a97"') == 1
helper.write_text(text.replace("https://github.com/aperod-network/aperod-node.git", str(new), 1)
                 .replace("4bfab534469dc61296910d968ab13cab27554a97", root_sha, 1))
stubs = ROOT / "package-and-ip-stubs"
stubs.mkdir()
# No git/go/make/readelf/systemctl/cp/sudo stubs: those are real in this guest.
for name, body in {
    "apt-get": "exit 0\n", "ufw": "exit 0\n",
    "curl": 'case "$*" in *localhost*|*127.0.0.1*) exec /usr/bin/curl "$@";; '
            '*ifconfig*|*ipify*|*icanhazip*) echo 127.0.0.9;; *) exit 91;; esac\n',
}.items():
    p = stubs / name
    p.write_text("#!/bin/bash\n" + body)
    p.chmod(0o755)
env["PATH"] = str(stubs) + ":" + env["PATH"]
env["APEROD_NODE_SOURCE_COMMIT"] = pin
env["APEROD_REWARD_ADDRESS"] = "apro" + "A" * 99
other_job = Path("/var/tmp/aperod-node-source.KEEPKEEP")
other_job.mkdir()
(other_job / "retain").write_text("another owner's source job")
others = manifest(other_job)


def cleaned():
    assert manifest(other_job) == others
    assert set(Path("/var/tmp").glob("aperod-node-source.*")) == {other_job}
    proof("only owned source jobs cleaned; unrelated source job unchanged")


def health():
    for _ in range(60):
        try:
            with urllib.request.urlopen("http://127.0.0.1:8545/api/v1/status", timeout=1) as response:
                return json.load(response)
        except Exception:
            time.sleep(0.2)
    raise AssertionError("real service did not become healthy")


def service_proof(version):
    # Type=simple acknowledges the fork before exec. Wait for the mapped
    # executable AND expected response, rather than sampling systemd-executor.
    for _ in range(60):
        pid = int(run("systemctl", "show", "aperod-node", "-p", "MainPID", "--value").stdout)
        if pid > 1 and Path(f"/proc/{pid}/exe").exists() and digest(Path(f"/proc/{pid}/exe")) == digest(BINARY):
            if health()["version"] == version:
                break
        time.sleep(0.1)
    else:
        raise AssertionError("real service failed executable/version convergence")
    assert run("systemctl", "is-active", "aperod-node").stdout.strip() == "active"
    before = health()["height"]
    time.sleep(0.3)
    assert health()["height"] > before
    proof("real systemd MainPID/executable hash, HTTP health and advancing height")


install_script = "install-validator.sh" if MODE == "validator" else "install-node.sh"
arguments = [] if MODE == "validator" else ["--primary-ip", "127.0.0.2"]
result = run("bash", bootstrap / install_script, *arguments, input="1\n")
print(result.stdout)
service_proof("fixture-new")
run("chown", "aperod:aperod", CONTROL)
cleaned()
assert Path("/etc/aperod/node.yaml").is_file()
assert Path("/etc/systemd/system/aperod-node.service.d/timeout.conf").is_file()
assert Path("/etc/systemd/system/aperod-node.service.d/gomemlimit.conf").is_file()
backup = Path("/usr/local/bin/aperod_backup.sh")
if MODE != "validator":
    assert backup.is_file() and (backup.stat().st_mode & 0o777) == 0o700
    assert backup.read_bytes() == (bootstrap / "aperod_backup.sh").read_bytes()
proof("fresh install, configuration, drop-ins, account and backup tool")
for unit in ("aperod-node-watchdog.timer", "aperod-sched-restart.timer", "aperod-mem-watchdog.timer"):
    run("systemctl", "stop", unit, ok=False)
if MODE in ("install", "validator"):
    before = digest(BINARY)
    denied = run("bash", bootstrap / install_script, *arguments, input="1\n", ok=False)
    assert denied.returncode and digest(BINARY) == before
    cleaned()
    service_proof("fixture-new")
    proof("fresh installer refuses reinstallation over live state")
    raise SystemExit(0)

# The runtime checkout is an INDEPENDENT old history, with local changes and
# hooks. Never use it as the build source or mutate its Git metadata.
old_repo = source("old")
run("go", "-C", old_repo, "build", "-o", ROOT / "old-binary", "./cmd/node")
run("systemctl", "stop", "aperod-node")
shutil.copy2(ROOT / "old-binary", BINARY)
run("systemctl", "start", "aperod-node")
service_proof("fixture-old")
shutil.copytree(old_repo, OLD, dirs_exist_ok=True)
assert git(OLD, "rev-list", "--max-parents=0", "HEAD") != root_sha
(OLD / "local-operator-note").write_text("retain local edits")
hook = OLD / ".git/hooks/post-checkout"
hook.write_text("#!/bin/sh\ntouch /fixture/forbidden-hook\n")
hook.chmod(0o755)
data = Path("/var/lib/aperod/data")
data.mkdir(parents=True, exist_ok=True)
run("go", "-C", old_repo, "build", "-o", ROOT / "store", "./cmd/store")
run(ROOT / "store", data / "chain.db")
snapshots = data / "snapshots"
snapshots.mkdir()
(snapshots / "fixture.snapshot").write_bytes(b"synthetic snapshot retained\x00\xff")
key = data / "validator.key"
key.write_bytes(bytes([0x33]) * 32)
key.chmod(0o600)
run("chown", "-R", "aperod:aperod", data)
config = Path("/etc/aperod/node.yaml")
config.write_text("data_dir: /var/lib/aperod/data\nconsensus:\n"
                  "  validator_key: /var/lib/aperod/data/validator.key\n")
protected = {str(p): manifest(p) if p.is_dir() else [digest(p), p.stat().st_mode,
              p.stat().st_uid, p.stat().st_gid] for p in (OLD, data, config)}


def unchanged():
    for name, original in protected.items():
        path = Path(name)
        after = manifest(path) if path.is_dir() else [digest(path), path.stat().st_mode,
                path.stat().st_uid, path.stat().st_gid]
        assert after == original, "retained runtime bytes/modes/ownership changed: " + name
    assert not (ROOT / "forbidden-hook").exists()
    cleaned()
    proof("old Git/history/hooks, config/key, real LevelDB and snapshots unchanged")


def approved(selected):
    return {**env, "APEROD_NODE_SOURCE_COMMIT": selected,
            "APEROD_BASELINE_APPROVED_SOURCE": selected,
            "APEROD_BASELINE_APPROVED_BINARY_SHA256": digest(BINARY)}


script = bootstrap / ("upgrade-node.sh" if MODE == "upgrade" else "update-node.sh")
before = digest(BINARY)
pid = run("systemctl", "show", "aperod-node", "-p", "MainPID", "--value").stdout
wrong = approved(pin)
wrong["APEROD_BASELINE_APPROVED_BINARY_SHA256"] = "0" * 64
result = run("bash", script, ok=False, custom=wrong)
assert result.returncode and digest(BINARY) == before
assert run("systemctl", "show", "aperod-node", "-p", "MainPID", "--value").stdout == pid
unchanged()
proof("changed running-binary approval refused without stopping service")
# Mutate the actual installed inode, not just the approval string. The mapped
# process remains on its old inode and must never be stopped by a failed guard.
saved = ROOT / "previous-binary"
shutil.copy2(BINARY, saved)
changed = ROOT / "changed-binary"
shutil.copy2(BINARY, changed)
with changed.open("ab") as output:
    output.write(b"synthetic approval invalidation")
approval = approved(pin)
os.replace(changed, BINARY)
result = run("bash", script, ok=False, custom=approval)
assert result.returncode and digest(BINARY) != before
assert run("systemctl", "show", "aperod-node", "-p", "MainPID", "--value").stdout == pid
os.replace(saved, BINARY)
unchanged()
proof("actual installed binary hash change refused before source acquisition")
# Also invalidate the same approval AFTER source selection, during the real
# build; the pre-stop recheck must catch the change without stopping the node.
approval = approved(race_pin)
race_log = ROOT / "approval-race.log"
with race_log.open("w") as output:
    process = subprocess.Popen(["bash", str(script)], env=approval,
                               stdout=output, stderr=subprocess.STDOUT)
    try:
        for _ in range(1600):
            if (CONTROL / "build-ready").exists():
                break
            assert process.poll() is None, race_log.read_text()
            time.sleep(0.1)
        else:
            raise AssertionError("real build did not reach the approval barrier")
        shutil.copy2(BINARY, saved)
        shutil.copy2(BINARY, changed)
        with changed.open("ab") as file:
            file.write(b"synthetic during-build invalidation")
        os.replace(changed, BINARY)
        (CONTROL / "build-release").touch()
        assert process.wait(timeout=45) != 0
        assert "approval changed during the build" in race_log.read_text()
        assert run("systemctl", "show", "aperod-node", "-p", "MainPID", "--value").stdout == pid
        os.replace(saved, BINARY)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
unchanged()
proof("during-build binary hash change refused before systemd stop")
result = run("bash", script, custom=approved(pin))
print(result.stdout)
service_proof("fixture-new")
unchanged()
proof("successful full existing-node update with explicit source/binary approval")
before = digest(BINARY)
result = run("bash", script, ok=False, custom=approved(bad_pin))
print(result.stdout)
assert result.returncode and digest(BINARY) == before
service_proof("fixture-new")
unchanged()
proof("health-failure rollback restored exact previous binary and healthy service")
# Validate real storage contents AFTER the byte comparison, since reopening a
# LevelDB legitimately updates its operational LOG, not its stored records.
run(ROOT / "store", data / "chain.db", "verify")
proof("real LevelDB reopening retains all original records")
print("SCENARIO_SUMMARY: lifecycle-" + MODE + " complete; no skipped scenarios")
