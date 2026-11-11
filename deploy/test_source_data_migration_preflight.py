#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import pwd
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("source-data-migration-preflight.py")
SPARSE = [
    "/artifacts/api-server/",
    "/blockchain/",
    "/deploy/",
    "/lib/api-zod/",
    "/lib/db/",
    "!/data/testnet/",
    "!/blockchain/data/testnet/",
]


class SourceDataMigrationPreflightTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory(prefix="aperod-source-migration-")
        self.root = Path(self.tmp.name)
        self.live = self.root / "legacy"
        self.stage = self.root / "source-stage"
        self.origin = self.root / "origin.git"
        self.config = self.root / "node.yaml"
        self.data = self.live / "data" / "testnet"
        self.target_parent = self.root / "var" / "lib" / "aperod"
        self.target = self.target_parent / "testnet"
        self.backup = self.root / "backup.tar"
        self.receipt = self.root / "verified-backup.json"
        self.systemctl = self.root / "systemctl"
        self.owner = pwd.getpwuid(os.getuid()).pw_name

        self.live.mkdir()
        self.data.mkdir(parents=True)
        (self.data / "chain.db").mkdir()
        (self.data / "chain.db" / "CURRENT").write_text("MANIFEST-000001\n")
        (self.data / "chain.db" / "MANIFEST-000001").write_text("VersionSet\n")
        self.config.write_text(f"data_dir: {self.data}\n")
        self.target_parent.mkdir(parents=True)

        seed = self.root / "seed"
        for rel in [
            "artifacts/api-server/src/index.ts",
            "blockchain/main.go",
            "deploy/update.sh",
            "lib/api-zod/index.ts",
            "lib/db/schema.ts",
            "data/testnet/chain.db/CURRENT",
            "blockchain/data/testnet/chain.db/CURRENT",
        ]:
            path = seed / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("runtime fixture\n" if "testnet" in rel else "source\n")
        subprocess.run(["git", "init", "--bare", str(self.origin)], check=True, capture_output=True)
        subprocess.run(["git", "init", "-b", "main", str(seed)], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(seed), "config", "user.email", "test@example.invalid"], check=True)
        subprocess.run(["git", "-C", str(seed), "config", "user.name", "Test"], check=True)
        subprocess.run(["git", "-C", str(seed), "add", "."], check=True)
        subprocess.run(["git", "-C", str(seed), "commit", "-m", "fixture"], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(seed), "remote", "add", "origin", str(self.origin)], check=True)
        subprocess.run(["git", "-C", str(seed), "push", "-u", "origin", "main"], check=True, capture_output=True)
        subprocess.run(
            ["git", "clone", "--no-checkout", "-b", "main", str(self.origin), str(self.stage)],
            check=True,
            capture_output=True,
        )
        subprocess.run(["git", "-C", str(self.stage), "sparse-checkout", "init", "--no-cone"], check=True)
        subprocess.run(
            ["git", "-C", str(self.stage), "sparse-checkout", "set", "--no-cone", "--stdin"],
            input="\n".join(SPARSE) + "\n",
            text=True,
            check=True,
            capture_output=True,
        )
        subprocess.run(["git", "-C", str(self.stage), "read-tree", "-mu", "HEAD"], check=True)
        self.commit = subprocess.check_output(
            ["git", "-C", str(self.stage), "rev-parse", "HEAD"], text=True
        ).strip()

        self.backup.write_bytes(b"verified archive fixture")
        self.archive_hash = hashlib.sha256(self.backup.read_bytes()).hexdigest()
        self.write_receipt()
        self.systemctl.write_text("#!/bin/sh\nprintf 'inactive\\n'\n")
        self.systemctl.chmod(0o755)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def write_receipt(self, **updates: object) -> None:
        receipt: dict[str, object] = {
            "schema": 1,
            "source_data_dir": str(self.data),
            "target_data_dir": str(self.target),
            "source_commit": self.commit,
            "archive_path": str(self.backup),
            "archive_sha256": self.archive_hash,
            "node_stopped": True,
            "isolated_restore_status": "passed",
            "isolated_restore_archive_sha256": self.archive_hash,
        }
        receipt.update(updates)
        self.receipt.write_text(json.dumps(receipt))

    def run_preflight(
        self, extra_env: dict[str, str] | None = None
    ) -> subprocess.CompletedProcess[str]:
        env = {
            **os.environ,
            "APEROD_MIGRATION_SOURCE_DIR": str(self.stage),
            "APEROD_MIGRATION_LIVE_SOURCE_DIR": str(self.live),
            "APEROD_MIGRATION_NODE_CONFIG": str(self.config),
            "APEROD_MIGRATION_TARGET_DATA_DIR": str(self.target),
            "APEROD_MIGRATION_BACKUP_RECEIPT": str(self.receipt),
            "APEROD_MIGRATION_EXPECTED_COMMIT": self.commit,
            "APEROD_MIGRATION_DATA_OWNER": self.owner,
            "PATH": str(self.root) + os.pathsep + os.environ.get("PATH", ""),
        }
        if extra_env:
            env.update(extra_env)
        (self.root / "systemctl").write_text(self.systemctl.read_text())
        (self.root / "systemctl").chmod(0o755)
        return subprocess.run(
            ["python3", str(SCRIPT)],
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_ready_is_read_only_and_does_not_create_target(self) -> None:
        before = (self.data / "chain.db" / "CURRENT").read_bytes()
        result = self.run_preflight()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("READY:", result.stdout)
        self.assertEqual((self.data / "chain.db" / "CURRENT").read_bytes(), before)
        self.assertFalse(self.target.exists())

    def test_wrong_source_commit_is_blocked(self) -> None:
        result = self.run_preflight_with("APEROD_MIGRATION_EXPECTED_COMMIT", "0" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match", result.stderr)

    def test_modified_backup_is_blocked(self) -> None:
        self.backup.write_bytes(b"changed after verification")
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHA-256", result.stderr)

    def test_active_node_is_blocked(self) -> None:
        self.systemctl.write_text("#!/bin/sh\nprintf 'active\\n'\n")
        self.systemctl.chmod(0o755)
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be confirmed inactive", result.stderr)

    def test_receipt_must_bind_the_exact_paths_and_restore_hash(self) -> None:
        self.write_receipt(target_data_dir=str(self.root / "wrong-target"))
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("target_data_dir", result.stderr)

    def test_sparse_stage_must_not_materialize_chain_data(self) -> None:
        forbidden = self.stage / "blockchain" / "data" / "testnet"
        forbidden.mkdir(parents=True)
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime data is materialized", result.stderr)

    def test_target_inside_source_checkout_is_blocked(self) -> None:
        result = self.run_preflight(
            {"APEROD_MIGRATION_TARGET_DATA_DIR": str(self.stage / "future-data")}
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be outside", result.stderr)

    def test_existing_target_is_never_overwritten(self) -> None:
        self.target.mkdir()
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already exists", result.stderr)

    def test_receipt_must_bind_reviewed_source_commit(self) -> None:
        self.write_receipt(source_commit="0" * 40)
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("source_commit", result.stderr)

    def test_private_key_permissions_are_preserved_fail_closed(self) -> None:
        key = self.data / "validator.key"
        key.write_bytes(b"test-only-key")
        key.chmod(0o644)
        result = self.run_preflight()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("broader than 0600", result.stderr)

    def run_preflight_with(self, key: str, value: str) -> subprocess.CompletedProcess[str]:
        return self.run_preflight({key: value})


if __name__ == "__main__":
    unittest.main()
