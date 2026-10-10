#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Privilege boundary regressions: temporary paths only, no installed payload execution."""
import hashlib
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

DEPLOY = Path(__file__).resolve().parent
GOOD = b"#!/bin/bash\necho approved-tool\n"


class ToolTrust(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo = self.root / "checkout"
        (self.repo / "deploy").mkdir(parents=True)
        self.source = self.repo / "deploy/aperod_backup.sh"
        self.source.write_bytes(GOOD)
        self.installed = self.root / "installed"
        self.installed.write_bytes(GOOD)
        self.installed.chmod(0o700)
        self.marker = self.root / "payload-ran"
        self.bad = f"#!/bin/bash\necho owned > '{self.marker}'\n".encode()
        self.approval = hashlib.sha256(GOOD).hexdigest()
        self.git("init", "-q")
        self.git("add", ".")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "commit", "-qm", "reviewed fixture")

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], text=True).strip()

    def sync(self, approval=None, source=None, installed=None, env=None):
        result = subprocess.run(
            ["bash", "-c", 'source "$1"; _sync_backup_script "$2" "$3" "${4:-}"',
             "fixture", str(DEPLOY / "sync-backup-script.sh"),
             str(installed or self.installed), str(source or self.source), approval or ""],
            env=env, capture_output=True, text=True)
        return result

    def unchanged(self):
        self.assertEqual(self.installed.read_bytes(), GOOD)
        self.assertEqual(self.installed.stat().st_mode & 0o777, 0o700)
        self.assertFalse(self.marker.exists())
        self.assertFalse(list(self.root.glob(".aperod_backup_sync.*")))

    def test_dirty_and_clean_unapproved_commits(self):
        for clean in (False, True):
            with self.subTest(clean=clean):
                self.source.write_bytes(self.bad)
                if clean:
                    self.git("add", ".")
                    self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                             "commit", "-qm", "unapproved local change")
                    self.assertEqual(self.git("status", "--porcelain"), "")
                    guard = subprocess.run(
                        ["bash", "-c", 'source "$1"; source_checkout_guard "$2" "" /dev/null',
                         "fixture", str(DEPLOY / "source-safe-guard.sh"), str(self.repo)])
                    self.assertEqual(guard.returncode, 0)
                for approval in (None, self.approval):
                    self.assertNotEqual(self.sync(approval).returncode, 0)
                    self.unchanged()

    def test_self_manifest_and_node_pin_do_not_authorize_tools(self):
        self.source.write_bytes(self.bad)
        (self.repo / "manifest.json").write_text(hashlib.sha256(self.bad).hexdigest())
        env = dict(os.environ, APEROD_NODE_SOURCE_COMMIT=self.git("rev-parse", "HEAD"),
                   APEROD_TOOL_APPROVED_SHA256=hashlib.sha256(self.bad).hexdigest())
        self.assertNotEqual(self.sync(env=env).returncode, 0)
        self.unchanged()

    def test_source_and_parent_symlinks(self):
        payload = self.root / "payload"
        payload.write_bytes(self.bad)
        self.source.unlink()
        self.source.symlink_to(payload)
        self.assertNotEqual(self.sync(hashlib.sha256(self.bad).hexdigest()).returncode, 0)
        self.unchanged()
        directory = self.root / "linked"
        directory.symlink_to(self.repo, target_is_directory=True)
        self.assertNotEqual(self.sync(self.approval, directory / "deploy/aperod_backup.sh").returncode, 0)
        self.unchanged()

    def test_destination_symlink(self):
        link = self.root / "destination-link"
        link.symlink_to(self.installed)
        self.assertNotEqual(self.sync(self.approval, installed=link).returncode, 0)
        self.assertTrue(link.is_symlink())
        self.unchanged()

    def test_staged_mutation_is_refused(self):
        self.source.write_bytes(b"#!/bin/bash\necho next-approved-tool\n")
        approval = hashlib.sha256(self.source.read_bytes()).hexdigest()
        shim = self.root / "bin"
        shim.mkdir()
        cp = shim / "cp"
        cp.write_text(f'#!/bin/bash\n/bin/cp "$@"\nprintf "%s" "{self.bad.decode()}" > "${{@: -1}}"\n')
        cp.chmod(0o700)
        env = dict(os.environ, PATH=f"{shim}:{os.environ['PATH']}")
        self.assertNotEqual(self.sync(approval, env=env).returncode, 0)
        self.unchanged()

    def test_independently_approved_copy_never_executes_tool(self):
        self.source.write_bytes(self.bad)
        result = self.sync(hashlib.sha256(self.bad).hexdigest())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.installed.read_bytes(), self.bad)
        self.assertFalse(self.marker.exists())

    def test_updaters_have_no_automatic_tool_loader_or_copy(self):
        for name in ("update-node.sh", "update-api.sh", "update-validator.sh"):
            text = (DEPLOY / name).read_text()
            commands = "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))
            self.assertNotRegex(commands, r"source[^\n]*sync-backup-script")
            self.assertNotRegex(commands, r"\b_sync_backup_script\b")
        # Execute the real node updater's complete former tool-sync section.
        # A poisoned old helper would run here in the vulnerable version.
        helper = self.repo / "deploy/sync-backup-script.sh"
        helper.write_bytes(self.bad)
        text = (DEPLOY / "update-node.sh").read_text()
        section = text.split("# Step 1b:", 1)[1].split("# Step 1c:", 1)[0]
        section = "# Step 1b:" + section
        result = subprocess.run(["bash", "-eu", "-c", section],
                                env=dict(os.environ, DEPLOY_DIR=str(self.repo / "deploy"),
                                         BLOCKCHAIN_DIR=str(self.repo), APEROD_DIR=str(self.repo)),
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.unchanged()


if __name__ == "__main__":
    unittest.main(verbosity=2)
