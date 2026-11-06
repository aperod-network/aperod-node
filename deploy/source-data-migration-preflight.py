#!/usr/bin/env python3
"""Read-only readiness gate for separating the production node source and data.

This tool deliberately does not stop services, copy files, change config, or
install a release. It verifies the evidence and path preconditions required
before a separately approved maintenance-window migration.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path


EXPECTED_SPARSE_PATTERNS = [
    "/artifacts/api-server/",
    "/blockchain/",
    "/deploy/",
    "/lib/api-zod/",
    "/lib/db/",
    "!/data/testnet/",
    "!/blockchain/data/testnet/",
]
RUNTIME_PATHS = (
    "data/testnet",
    "blockchain/data/testnet",
    "chain.db",
    "snapshots",
)


class PreflightError(Exception):
    pass


def env_path(name: str, default: str) -> Path:
    value = os.environ.get(name, default)
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts:
        raise PreflightError(f"{name} must be an absolute path without '..'")
    return path


def reject_symlink_components(path: Path, *, allow_missing_leaf: bool = False) -> None:
    current = Path("/")
    parts = path.parts[1:]
    for index, part in enumerate(parts):
        current /= part
        if current.is_symlink():
            raise PreflightError(f"path contains a symlink component: {current}")
        if not current.exists() and not (allow_missing_leaf and index == len(parts) - 1):
            raise PreflightError(f"path component does not exist: {current}")


def run(*args: str, cwd: Path | None = None) -> str:
    result = subprocess.run(
        args,
        cwd=cwd,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode:
        detail = result.stderr.strip() or "command failed"
        raise PreflightError(f"{' '.join(args)}: {detail}")
    return result.stdout.strip()


def canonical_existing(path: Path, label: str) -> Path:
    reject_symlink_components(path)
    try:
        result = path.resolve(strict=True)
    except OSError as exc:
        raise PreflightError(f"cannot resolve {label}: {exc}") from exc
    if not result.is_dir():
        raise PreflightError(f"{label} is not a directory: {result}")
    return result


def canonical_future(path: Path, label: str) -> Path:
    reject_symlink_components(path, allow_missing_leaf=True)
    if path.exists():
        raise PreflightError(f"{label} already exists; refusing to overwrite it: {path}")
    try:
        parent = path.parent.resolve(strict=True)
    except OSError as exc:
        raise PreflightError(f"{label} parent is not available: {exc}") from exc
    if not parent.is_dir():
        raise PreflightError(f"{label} parent is not a directory: {parent}")
    return parent / path.name


def require_outside(path: Path, roots: list[Path], label: str) -> None:
    for root in roots:
        if path == root or root in path.parents:
            raise PreflightError(f"{label} must be outside {root}: {path}")


def parse_data_dir(config_path: Path) -> Path:
    if config_path.is_symlink() or not config_path.is_file():
        raise PreflightError(f"node config is missing or symlinked: {config_path}")
    matches = re.findall(
        r"^[ \t]*data_dir[ \t]*:[ \t]*(.*?)[ \t]*(?:#.*)?$",
        config_path.read_text(encoding="utf-8"),
        flags=re.MULTILINE,
    )
    if len(matches) != 1:
        raise PreflightError("node config must contain exactly one data_dir entry")
    raw = matches[0].strip().strip("\"'")
    if not raw:
        raise PreflightError("node config has an empty data_dir")
    candidate = Path(raw)
    if ".." in candidate.parts:
        raise PreflightError("node config data_dir must not contain '..'")
    if not candidate.is_absolute():
        candidate = config_path.parent / candidate
    return candidate


def verify_source_checkout(source: Path, live_source: Path, expected_commit: str) -> Path:
    if not re.fullmatch(r"[0-9a-f]{40}", expected_commit):
        raise PreflightError("APEROD_MIGRATION_EXPECTED_COMMIT must be a full 40-character commit")
    reject_symlink_components(source)
    source_real = source.resolve(strict=True)
    live_real = live_source.resolve(strict=True)
    if source_real == live_real or live_real in source_real.parents or source_real in live_real.parents:
        raise PreflightError("staging and legacy live source must be separate, non-nested directories")
    git_root = Path(run("git", "-C", str(source_real), "rev-parse", "--show-toplevel")).resolve()
    if git_root != source_real:
        raise PreflightError("staging path is not the root of its own Git checkout")
    if run("git", "-C", str(source_real), "rev-parse", "HEAD") != expected_commit:
        raise PreflightError("staging HEAD does not match the explicitly reviewed commit")
    if run("git", "-C", str(source_real), "branch", "--show-current") != "main":
        raise PreflightError("staging must be checked out on main")
    run("git", "-C", str(source_real), "show-ref", "--verify", "refs/remotes/origin/main")
    run("git", "-C", str(source_real), "merge-base", "--is-ancestor", expected_commit, "origin/main")
    if run("git", "-C", str(source_real), "status", "--porcelain", "--untracked-files=all"):
        raise PreflightError("staging checkout is dirty")
    if run("git", "-C", str(source_real), "config", "--bool", "core.sparseCheckout") != "true":
        raise PreflightError("staging checkout is not sparse")
    patterns = run("git", "-C", str(source_real), "sparse-checkout", "list").splitlines()
    if patterns != EXPECTED_SPARSE_PATTERNS:
        raise PreflightError("staging sparse paths do not match the reviewed source allowlist")
    for rel in RUNTIME_PATHS:
        candidate = source_real / rel
        if candidate.exists() or candidate.is_symlink():
            raise PreflightError(f"runtime data is materialized inside staging: {candidate}")
    return source_real


def tree_size_and_owner(root: Path, expected_owner: str) -> int:
    total = 0
    for base, dirs, files in os.walk(root, followlinks=False):
        base_path = Path(base)
        for name in dirs + files:
            path = base_path / name
            if path.is_symlink():
                raise PreflightError(f"runtime data contains a symlink: {path}")
            try:
                info = path.stat(follow_symlinks=False)
                owner = __import__("pwd").getpwuid(info.st_uid).pw_name
            except (OSError, KeyError) as exc:
                raise PreflightError(f"cannot inspect runtime path {path}: {exc}") from exc
            if owner != expected_owner:
                raise PreflightError(f"runtime path has owner {owner}, expected {expected_owner}: {path}")
            if path.name in {"validator.key", "p2p_identity.key"} and info.st_mode & 0o077:
                raise PreflightError(f"private key permissions are broader than 0600: {path}")
            if path.is_file():
                total += info.st_size
            elif not path.is_dir():
                raise PreflightError(f"runtime data contains a non-regular entry: {path}")
    root_owner = __import__("pwd").getpwuid(root.stat().st_uid).pw_name
    if root_owner != expected_owner:
        raise PreflightError(f"runtime data root has owner {root_owner}, expected {expected_owner}")
    return total


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def verify_backup_receipt(
    receipt_path: Path,
    source_data: Path,
    target_data: Path,
    expected_commit: str,
    roots: list[Path],
) -> None:
    reject_symlink_components(receipt_path)
    if not receipt_path.is_file():
        raise PreflightError(f"verified backup receipt is missing: {receipt_path}")
    receipt_real = receipt_path.resolve(strict=True)
    require_outside(receipt_real, roots + [source_data, target_data], "backup receipt")
    try:
        receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise PreflightError(f"cannot read backup receipt: {exc}") from exc
    required = {
        "schema": 1,
        "source_data_dir": str(source_data),
        "target_data_dir": str(target_data),
        "source_commit": expected_commit,
        "node_stopped": True,
        "isolated_restore_status": "passed",
    }
    for key, expected in required.items():
        if receipt.get(key) != expected:
            raise PreflightError(f"backup receipt has missing or mismatched {key}")
    archive_raw = receipt.get("archive_path")
    archive_hash = receipt.get("archive_sha256")
    if not isinstance(archive_raw, str) or not Path(archive_raw).is_absolute():
        raise PreflightError("backup receipt archive_path must be absolute")
    if not isinstance(archive_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", archive_hash):
        raise PreflightError("backup receipt archive_sha256 is invalid")
    archive = Path(archive_raw)
    if ".." in archive.parts:
        raise PreflightError("backup receipt archive_path must not contain '..'")
    reject_symlink_components(archive)
    if not archive.is_file():
        raise PreflightError("backup archive does not exist")
    archive_real = archive.resolve(strict=True)
    require_outside(archive_real, roots + [source_data, target_data], "backup archive")
    if sha256_file(archive_real) != archive_hash:
        raise PreflightError("backup archive SHA-256 does not match the receipt")
    if receipt.get("isolated_restore_archive_sha256") != archive_hash:
        raise PreflightError("restore-test receipt is not bound to this backup archive")


def main() -> int:
    try:
        source_dir = env_path("APEROD_MIGRATION_SOURCE_DIR", "/opt/aperod-source-staging")
        live_source_dir = env_path("APEROD_MIGRATION_LIVE_SOURCE_DIR", "/opt/aperod")
        config_path = env_path("APEROD_MIGRATION_NODE_CONFIG", "/etc/aperod/node.yaml")
        target_raw = env_path("APEROD_MIGRATION_TARGET_DATA_DIR", "/var/lib/aperod/testnet")
        receipt_path = env_path(
            "APEROD_MIGRATION_BACKUP_RECEIPT",
            "/var/lib/aperod/source-data-migration/verified-backup.json",
        )
        expected_commit = os.environ.get("APEROD_MIGRATION_EXPECTED_COMMIT", "")
        expected_owner = os.environ.get("APEROD_MIGRATION_DATA_OWNER", "aperod")
        if not expected_owner:
            raise PreflightError("APEROD_MIGRATION_DATA_OWNER cannot be empty")

        source_real = verify_source_checkout(source_dir, live_source_dir, expected_commit)
        live_real = canonical_existing(live_source_dir, "legacy live source")
        source_data = canonical_existing(parse_data_dir(config_path), "configured node data")
        target_data = canonical_future(target_raw, "external data target")
        require_outside(target_data, [source_real, live_real], "external data target")
        if source_data == target_data:
            raise PreflightError("source and target data paths are identical")

        if run("systemctl", "show", "--property=ActiveState", "--value", "aperod-node") != "inactive":
            raise PreflightError("aperod-node must be confirmed inactive during the maintenance window")

        data_bytes = tree_size_and_owner(source_data, expected_owner)
        target_parent_owner = __import__("pwd").getpwuid(target_data.parent.stat().st_uid).pw_name
        if target_parent_owner != expected_owner:
            raise PreflightError(
                f"external target parent has owner {target_parent_owner}, expected {expected_owner}"
            )
        target_parent_mode = target_data.parent.stat().st_mode
        if target_parent_mode & 0o022:
            raise PreflightError("external target parent is group- or world-writable")
        required_bytes = data_bytes + max(512 * 1024 * 1024, data_bytes // 10)
        free_bytes = shutil.disk_usage(target_data.parent).free
        if free_bytes < required_bytes:
            raise PreflightError(
                f"insufficient target disk space: {free_bytes} free, at least {required_bytes} required"
            )

        verify_backup_receipt(
            receipt_path,
            source_data,
            target_data,
            expected_commit,
            [source_real, live_real],
        )
        print("READY: source/data path checks passed; no files or services were changed.")
        print(f"  source commit: {expected_commit}")
        print(f"  current data:  {source_data} ({data_bytes} bytes)")
        print(f"  future data:   {target_data} (must remain absent until the approved copy step)")
        print(f"  owner:         {expected_owner}")
        print(f"  target free:   {free_bytes} bytes")
        print("  backup receipt: checksum and isolated-restore claim match")
        print("  IMPORTANT: receipt fields are operator evidence, not a cryptographic signature.")
        return 0
    except (PreflightError, OSError, subprocess.SubprocessError) as exc:
        print(f"BLOCKED: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
