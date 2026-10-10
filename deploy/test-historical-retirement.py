#!/usr/bin/env python3
"""Focused safety tests for explicitly registered historical retirement."""

from __future__ import annotations

import hashlib
import contextlib
import io
import os
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

DEPLOY = Path(__file__).resolve().parent
sys.path.insert(0, str(DEPLOY))

import rollout_cleanup as cleanup  # noqa: E402
from rollout_cleanup import retirement  # noqa: E402
from rollout_cleanup import cli  # noqa: E402


class CLIRetirementTests(unittest.TestCase):
    def test_refused_scan_cannot_report_service_success(self):
        for result, expected in [({"status": "refused"}, 1), ({"refused": True}, 1),
                                 ({"status": "ok"}, 0)]:
            with mock.patch.object(cli, "_dispatch", return_value=result), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(cli.main(["retirement-scan", "--dry-run"]), expected)


class RetirementFixture:
    def __init__(self, root: Path):
        self.root = root
        self.state = root / "state"
        self.releases = root / "releases"
        self.release = self.releases / "legacy-1"
        self.stopped = self.release / "rollback" / "stopped-data"
        self.data = root / "live-data"
        self.config = root / "node.conf"
        self.settings = root / "settings.json"
        for path in (self.state, self.releases, self.release, self.stopped,
                     self.data, self.data / "chain.db", self.stopped / "chain.db"):
            path.mkdir(parents=True, exist_ok=True)
        for path in (self.state, self.rollback, self.stopped, self.stopped / "chain.db",
                     self.data, self.data / "chain.db"):
            path.chmod(0o700)
        (self.stopped / "chain.db" / "CURRENT").write_text("MANIFEST-000001\n")
        (self.stopped / "chain.db" / "MANIFEST-000001").write_bytes(b"db manifest")
        (self.stopped / "chain.db" / "000001.ldb").write_bytes(b"historical leveldb")
        (self.stopped / "snapshot-legacy.gz").write_bytes(b"snapshot bytes")
        (self.stopped / "snapshot-legacy.gz.sha256").write_text("a" * 64)
        (self.stopped / "p2p_identity.key").write_bytes(b"preserve secret")
        (self.stopped / ".backup-staging").mkdir(mode=0o700)
        (self.release / "node.previous").write_bytes(b"old binary")
        self.config.write_text("fixture-config")
        self.settings.write_text("{}")
        self.origin = {
            "data_dir": str(self.data), "chain_db": cleanup._identity(self.data / "chain.db", allow_dir=True),
            "config": str(self.config), "config_identity": cleanup._identity(self.config),
            "config_sha256": cleanup._sha_file(self.config), "service": "aperod-node",
            "pid": 101, "start_ticks": 1, "live_exe": "/bin/true",
            "height": 5, "hash": "a" * 64, "genesis_hash": "b" * 64,
            "api_url": "http://127.0.0.1:8545",
        }
        self.runtime = cleanup.Runtime(
            state_dir=self.state, releases_dir=self.releases, settings_file=self.settings,
            is_root=lambda: True, root_owned=lambda _info: True,
            clock=lambda: 100, proc_root=root / "proc",
        )
        self.proc = self.runtime.proc_root
        self.proc.mkdir()
        self.registration = self.register()

    @property
    def rollback(self):
        return self.release / "rollback"

    def register(self):
        with mock.patch.object(retirement, "_capture_origin", return_value=self.origin), \
                mock.patch.object(retirement, "_origin_unchanged"), \
                mock.patch.object(retirement, "_proc_references"):
            return retirement.register(
                str(self.release), str(self.data), str(self.config), "aperod-node",
                self.origin["api_url"], runtime=self.runtime,
            )

    def record_path(self):
        return self.state / ("retirement-" + self.registration["id"] + ".json")


class ArchiveValidationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.content = b"archive fixture"
        self.members = [{
            "path": "chain.db", "type": "dir", "size": 0, "sha256": None,
        }, {
            "path": "chain.db/000001.ldb", "type": "file",
            "size": len(self.content), "sha256": hashlib.sha256(self.content).hexdigest(),
        }]

    def write_tar(self, name, *, data=None, symlink=False, extra=False):
        path = self.root / name
        with tarfile.open(path, "w:gz") as archive:
            directory = tarfile.TarInfo("chain.db")
            directory.type = tarfile.DIRTYPE
            archive.addfile(directory)
            file_info = tarfile.TarInfo("chain.db/000001.ldb")
            file_info.size = len(self.content)
            if symlink:
                file_info.type = tarfile.SYMTYPE
                file_info.linkname = "outside"
                archive.addfile(file_info)
            else:
                import io
                archive.addfile(file_info, io.BytesIO(self.content if data is None else data))
            if extra:
                extra_info = tarfile.TarInfo("chain.db/unexpected")
                extra_info.size = 1
                archive.addfile(extra_info, io.BytesIO(b"x"))
        return path

    def test_valid_archive_streams_and_verifies_every_member(self):
        archive = self.write_tar("valid.tar.gz")
        retirement._validate_archive(archive, self.members)

    def test_truncated_unexpected_symlink_and_corrupt_content_are_refused(self):
        valid = self.write_tar("truncated.tar.gz")
        raw = valid.read_bytes()
        valid.write_bytes(raw[:len(raw) // 2])
        with self.assertRaises(cleanup.CleanupError):
            retirement._validate_archive(valid, self.members)
        with self.assertRaises(cleanup.CleanupError):
            retirement._validate_archive(self.write_tar("link.tar.gz", symlink=True), self.members)
        with self.assertRaises(cleanup.CleanupError):
            retirement._validate_archive(
                self.write_tar("corrupt.tar.gz", data=b"x" * len(self.content)), self.members,
            )
        with self.assertRaises(cleanup.CleanupError):
            retirement._validate_archive(self.write_tar("extra.tar.gz", extra=True), self.members)


class HistoricalRegistrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.fixture = RetirementFixture(Path(self.temp.name))

    def test_registration_is_distinct_and_selects_only_chain_and_top_level_snapshots(self):
        f = self.fixture
        record = cleanup.read_json(f.record_path())
        self.assertEqual(record["type"], "historical-retirement")
        self.assertEqual(record["origin"], f.origin)
        self.assertTrue(f.record_path().name.startswith("retirement-"))
        targets = record["targets"]
        self.assertEqual({Path(item["path"]).name for item in targets},
                         {"chain.db", "snapshot-legacy.gz", "snapshot-legacy.gz.sha256"})
        self.assertNotIn("p2p_identity.key", repr(record["targets"]))
        self.assertNotIn(".backup-staging", repr(record["targets"]))

    def test_current_bak_is_only_allowed_in_the_historical_db_scope(self):
        f = self.fixture
        path = f.stopped / "chain.db"
        (path / "CURRENT.bak").write_bytes(b"MANIFEST-000001\n")
        with self.assertRaises(cleanup.CleanupError):
            cleanup._manifest(path, "stopped-copy", f.runtime)
        manifest, _ = cleanup._manifest(path, "stopped-copy", f.runtime, allow_current_bak=True)
        self.assertIn("CURRENT.bak", {item["path"] for item in manifest})
        (path / "unexpected.bak").write_bytes(b"unexpected")
        with self.assertRaises(cleanup.CleanupError):
            cleanup._manifest(path, "stopped-copy", f.runtime, allow_current_bak=True)
        self.assertTrue((f.stopped / "p2p_identity.key").exists())
        self.assertTrue((f.release / "node.previous").exists())

    def test_dry_run_never_uploads_or_deletes_and_keeps_keys(self):
        f = self.fixture
        with mock.patch.object(retirement, "_origin_unchanged"), \
                mock.patch.object(retirement, "_proc_references"), \
                mock.patch.object(retirement.subprocess, "run") as run, \
                mock.patch.object(retirement, "_archive_and_verify") as archive:
            result = retirement.scan_retirements(runtime=f.runtime)
        self.assertEqual(result["retirements"][0]["status"], "ready")
        run.assert_not_called()
        archive.assert_not_called()
        self.assertTrue((f.stopped / "chain.db" / "000001.ldb").exists())
        self.assertTrue((f.stopped / "p2p_identity.key").exists())

    def test_changed_source_and_process_reference_refuse_retirement(self):
        f = self.fixture
        changed = f.stopped / "chain.db" / "000001.ldb"
        changed.write_bytes(b"mutated")
        with mock.patch.object(retirement, "_origin_unchanged"), \
                mock.patch.object(retirement, "_proc_references"):
            result = retirement.scan_retirements(runtime=f.runtime)
        self.assertEqual(result["retirements"][0]["status"], "refused")
        self.assertIn("changed", result["retirements"][0]["error"])
        changed.write_bytes(b"historical leveldb")
        with mock.patch.object(retirement, "_origin_unchanged"), \
                mock.patch.object(retirement, "_proc_references",
                                  side_effect=cleanup.CleanupError("process reference")):
            result = retirement.scan_retirements(runtime=f.runtime)
        self.assertEqual(result["retirements"][0]["status"], "refused")
        self.assertTrue((f.stopped / "p2p_identity.key").exists())

    def test_completed_record_is_idempotent(self):
        f = self.fixture
        record = cleanup.read_json(f.record_path())
        record["status"] = "completed"
        cleanup.atomic_json(f.record_path(), record)
        result = retirement.scan_retirements(apply=True, runtime=f.runtime)
        self.assertEqual(result["retirements"], [{
            "id": f.registration["id"], "status": "completed", "removed_bytes": 0,
        }])

    def test_successful_apply_deletes_only_registered_targets(self):
        f = self.fixture
        proof = {
            "sha256": "a" * 64, "size": 99, "file_id": "immutable",
            "bucket_id": "bucket", "bucket": "vault", "endpoint": "https://example",
            "provider_fingerprint": "b" * 64,
            "manifest_sha256": cleanup.read_json(f.record_path())["manifest_sha256"],
            "verified_at": 123, "object_name":
                "historical-retirement/" + f.registration["id"] + "/archive.tar.gz.gpg",
        }
        with mock.patch.object(retirement, "_settings", return_value={"s3backup": {}}), \
                mock.patch.object(retirement, "_origin_unchanged"), \
                mock.patch.object(retirement, "_proc_references"), \
                mock.patch.object(retirement, "_archive_and_verify", return_value=proof), \
                mock.patch.object(retirement, "_confirm_archive_proof"):
            result = retirement.scan_retirements(apply=True, runtime=f.runtime)
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["retirements"][0]["status"], "completed")
        self.assertFalse((f.stopped / "chain.db").exists())
        self.assertFalse((f.stopped / "snapshot-legacy.gz").exists())
        self.assertFalse((f.stopped / "snapshot-legacy.gz.sha256").exists())
        self.assertTrue((f.stopped / "p2p_identity.key").exists())
        self.assertTrue((f.stopped / ".backup-staging").is_dir())
        self.assertTrue((f.release / "node.previous").exists())

    def test_registration_refuses_process_references_before_archiving(self):
        f = self.fixture
        (f.stopped / "chain.db" / "CURRENT").write_text("MANIFEST-000001\n")
        original = f.record_path()
        original.unlink()
        with mock.patch.object(retirement, "_capture_origin", return_value=f.origin), \
                mock.patch.object(retirement, "_origin_unchanged"), \
                mock.patch.object(retirement, "_proc_references",
                                  side_effect=cleanup.CleanupError("reference present")), \
                self.assertRaisesRegex(cleanup.CleanupError, "reference present"):
            retirement.register(str(f.release), str(f.data), str(f.config), "aperod-node",
                                f.origin["api_url"], runtime=f.runtime)
        self.assertFalse(list(f.state.glob("retirement-*.json")))

    def test_closed_parent_symlink_and_mount_are_rejected(self):
        f = self.fixture
        stopped = f.stopped
        stopped.rename(stopped.with_name("stopped-data-real"))
        stopped.symlink_to(stopped.with_name("stopped-data-real"), target_is_directory=True)
        with self.assertRaises(cleanup.CleanupError):
            retirement._closed_historical_path(f.release, f.runtime)
        stopped.unlink()
        stopped.with_name("stopped-data-real").rename(stopped)
        with mock.patch.object(retirement, "_assert_not_mountpoint",
                               side_effect=cleanup.CleanupError("mount")):
            with self.assertRaisesRegex(cleanup.CleanupError, "mount"):
                retirement._closed_historical_path(f.release, f.runtime)


class B2VersionTests(unittest.TestCase):
    def test_by_id_download_uses_account_authorization_token(self):
        settings = {"s3backup": {
            "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "vault",
            "accessKeyId": "access", "secretAccessKey": "secret",
        }}
        auth = {
            "apiUrl": "https://api.backblazeb2.com", "downloadUrl": "https://f004.backblazeb2.com",
            "authorizationToken": "account-token", "accountId": "account-1",
        }
        bucket = {"buckets": [{
            "bucketName": "vault", "bucketId": "bucket-1", "lifecycleRules": [],
        }]}
        with mock.patch.object(retirement._RetirementB2Provider, "_request",
                               side_effect=[auth, bucket]) as request:
            url, token = retirement._RetirementB2Provider._authorized_download(
                settings, "bucket-1", "historical-retirement/id/archive.tar.gz.gpg",
            )
        self.assertEqual(url, "https://f004.backblazeb2.com")
        self.assertEqual(token, "account-token")
        self.assertEqual(request.call_count, 2)
        self.assertEqual(request.call_args_list[1].args[2], {
            "accountId": "account-1", "bucketName": "vault",
        })

    def test_exact_file_id_and_size_are_required(self):
        object_name = "historical-retirement/id/archive.tar.gz.gpg"

        class Provider:
            def __init__(self, rows):
                self.rows = rows
                self.requested = None

            def versions(self, _settings, requested):
                self.requested = requested
                return self.rows

        row = {"fileName": object_name, "fileId": "immutable-file-id",
               "action": "upload", "size": 37}
        provider = Provider([row])
        selected = retirement._remote_verify({}, object_name, 37, "a" * 64, provider)
        self.assertEqual(selected["fileId"], "immutable-file-id")
        self.assertEqual(provider.requested, object_name)
        with self.assertRaises(cleanup.CleanupError):
            retirement._remote_verify({}, object_name, 38, "a" * 64, Provider([row]))
        with self.assertRaises(cleanup.CleanupError):
            retirement._remote_verify({}, object_name, 37, "a" * 64, Provider([
                dict(row, fileName=object_name + ".neighbor")
            ]))

    def test_archive_proof_requires_exact_b2_file_id_and_encrypted_hash(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = RetirementFixture(Path(tmp))
            record = cleanup.read_json(fixture.record_path())
            ciphertext = b"verified encrypted archive"
            proof = {
                "object_name": "historical-retirement/" + record["id"] + "/archive.tar.gz.gpg",
                "file_id": "exact-version", "bucket_id": "bucket-1", "bucket": "vault",
                "size": len(ciphertext), "sha256": hashlib.sha256(ciphertext).hexdigest(),
                "endpoint": "https://b2.example", "provider_fingerprint": "d" * 64,
                "manifest_sha256": retirement._manifest_hash(
                    retirement._archive_manifest(record["targets"])),
                "verified_at": 123,
            }
            record["archive"] = proof

            class Provider:
                rows = [{
                    "fileName": proof["object_name"], "fileId": "exact-version",
                    "bucketId": "bucket-1", "action": "upload", "size": len(ciphertext),
                }]

                def versions(self, _settings, name):
                    self.asserted_name = name
                    return self.rows

            provider = Provider()
            with mock.patch.object(retirement, "_lifecycle_and_identity", return_value={
                "bucket_id": "bucket-1", "bucket": "vault", "endpoint": "https://b2.example",
            }), mock.patch.dict(os.environ, {"APEROD_BACKUP_PASSWORD": "fixture"}), mock.patch.object(
                retirement._RetirementB2Provider, "fingerprint", return_value="d" * 64,
            ), mock.patch.object(retirement, "_ensure_staging_headroom"), mock.patch.object(
                retirement._RetirementB2Provider, "download_file_id",
                side_effect=lambda _s, _b, _n, _i, output, _z: output.write_bytes(ciphertext),
            ), mock.patch.object(retirement, "_run_gpg_decrypt"), mock.patch.object(
                retirement, "_validate_archive"
            ):
                retirement._confirm_archive_proof(record, {}, fixture.runtime, provider)
                provider.rows = [dict(provider.rows[0], fileId="different-version")]
                with self.assertRaises(cleanup.CleanupError):
                    retirement._confirm_archive_proof(record, {}, fixture.runtime, provider)
                provider.rows = [dict(provider.rows[0], fileId="exact-version")]
                with mock.patch.object(retirement, "_sha_file", return_value="0" * 64):
                    with self.assertRaisesRegex(cleanup.CleanupError, "hash or size mismatch"):
                        retirement._confirm_archive_proof(record, {}, fixture.runtime, provider)


if __name__ == "__main__":
    unittest.main(verbosity=2)