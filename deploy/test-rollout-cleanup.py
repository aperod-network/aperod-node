#!/usr/bin/env python3
"""Filesystem and unit tests for the rollout-cleanup safety boundary."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import stat
import sys
import tempfile
import unittest
from contextlib import nullcontext
from pathlib import Path
from types import SimpleNamespace
from unittest import mock


DEPLOY = Path(__file__).resolve().parent
sys.path.insert(0, str(DEPLOY))
import rollout_cleanup as cleanup  # noqa: E402
from rollout_cleanup import cli  # noqa: E402
from rollout_cleanup import runtime as cleanup_runtime  # noqa: E402


def root_stat(real):
    fields = {name: getattr(real, name) for name in dir(real) if name.startswith("st_")}
    fields["st_uid"] = 0
    return SimpleNamespace(**fields)


class StrictJSONTests(unittest.TestCase):
    def test_duplicate_keys_are_refused_at_every_object_level(self):
        for raw in (b'{"x":1,"x":2}', b'{"x":{"a":1,"a":2}}'):
            with self.subTest(raw=raw), self.assertRaises(cleanup.CleanupError):
                cleanup.strict_json_bytes(raw)

    def test_bounded_utf8_and_top_level_json(self):
        with mock.patch.object(cleanup_runtime, "MAX_JSON", 8):
            with self.assertRaisesRegex(cleanup.CleanupError, "size limit"):
                cleanup.strict_json_bytes(b'{"long":true}')
        with self.assertRaises(cleanup.CleanupError):
            cleanup.strict_json_bytes(b"\xff")
        self.assertEqual(cleanup.strict_json_bytes(b'{"ok":true}'), {"ok": True})

    def test_symlink_json_is_not_followed(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            actual = base / "real.json"
            actual.write_text("{}")
            link = base / "link.json"
            link.symlink_to(actual)
            with self.assertRaises(cleanup.CleanupError):
                cleanup.read_json(link)

    def test_atomic_json_uses_mode_600_and_replaces_whole_document(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "state.json"
            cleanup.atomic_json(path, {"schema": 1, "state": "ok"})
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(cleanup.read_json(path), {"schema": 1, "state": "ok"})
            self.assertEqual({p.name for p in path.parent.iterdir()}, {"state.json"})


class ArtifactFilesystemTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "stopped-copy"
        self.root.mkdir(mode=0o700)

    def owner_patched_lstat(self):
        original = os.lstat

        def lstat(path, *args, **kwargs):
            return root_stat(original(path, *args, **kwargs))
        return mock.patch.object(cleanup.os, "lstat", side_effect=lstat)

    def test_stopped_copy_accepts_leveldb_and_snapshots_and_accounts_bytes(self):
        (self.root / "CURRENT").write_bytes(b"MANIFEST-000001\n")
        (self.root / "MANIFEST-000001").write_bytes(b"version")
        (self.root / "000002.ldb").write_bytes(b"records")
        (self.root / "snapshot-20260101.gz").write_bytes(b"compressed")
        (self.root / "snapshot-20260101.sha256").write_text("a" * 64)
        with self.owner_patched_lstat():
            manifest, size = cleanup._manifest(self.root, "stopped-copy")
        self.assertEqual(size, sum(p.stat().st_size for p in self.root.iterdir()))
        self.assertEqual({entry["path"] for entry in manifest},
                         {"CURRENT", "MANIFEST-000001", "000002.ldb",
                          "snapshot-20260101.gz", "snapshot-20260101.sha256"})
        self.assertTrue(all(entry["sha256"] for entry in manifest))

    def test_stopped_copy_rejects_unmarked_or_unapproved_children(self):
        (self.root / "node.key").write_text("never delete")
        with self.owner_patched_lstat(), self.assertRaises(cleanup.CleanupError):
            cleanup._manifest(self.root, "stopped-copy")
        (self.root / "node.key").unlink()
        (self.root / "mystery.bin").write_bytes(b"not a leveldb file")
        with self.owner_patched_lstat(), self.assertRaises(cleanup.CleanupError):
            cleanup._manifest(self.root, "stopped-copy")

    def test_stopped_copy_rejects_symlinks_and_hardlinks(self):
        external = Path(self.temp.name) / "outside"
        external.write_bytes(b"outside")
        (self.root / "CURRENT").symlink_to(external)
        with self.owner_patched_lstat(), self.assertRaises(cleanup.CleanupError):
            cleanup._manifest(self.root, "stopped-copy")
        (self.root / "CURRENT").unlink()
        source = self.root / "000001.ldb"
        source.write_bytes(b"hardlinked")
        os.link(source, self.root / "000002.ldb")
        with self.owner_patched_lstat(), self.assertRaisesRegex(cleanup.CleanupError, "hard link"):
            cleanup._manifest(self.root, "stopped-copy")

    def test_protected_directories_are_refused_even_when_empty(self):
        (self.root / "keys").mkdir()
        with self.owner_patched_lstat(), self.assertRaisesRegex(cleanup.CleanupError, "protected"):
            cleanup._manifest(self.root, "stopped-copy")

    def test_nested_mountpoint_is_refused(self):
        nested = self.root / "segments"
        nested.mkdir()
        (nested / "000001.ldb").write_bytes(b"segment")
        original = cleanup._assert_not_mountpoint

        def reject_nested(path):
            if Path(path) == nested:
                raise cleanup.CleanupError("artifact contains a mount point")
            original(path)

        with self.owner_patched_lstat(), mock.patch.object(
            cleanup, "_assert_not_mountpoint", side_effect=reject_nested
        ), self.assertRaisesRegex(cleanup.CleanupError, "mount point"):
            cleanup._manifest(self.root, "stopped-copy")

    def test_candidate_captures_hash_and_refuses_symlink(self):
        candidate = Path(self.temp.name) / "node.candidate"
        payload = b"binary candidate"
        candidate.write_bytes(payload)
        with self.owner_patched_lstat():
            manifest, size = cleanup._manifest(candidate, "candidate")
        self.assertEqual(size, len(payload))
        self.assertEqual(manifest[0]["sha256"], hashlib.sha256(payload).hexdigest())
        candidate.unlink()
        candidate.symlink_to(Path(self.temp.name) / "outside")
        with self.owner_patched_lstat(), self.assertRaises(cleanup.CleanupError):
            cleanup._manifest(candidate, "candidate")

    def test_descriptor_delete_of_candidate_preserves_neighbors(self):
        candidate = Path(self.temp.name) / "node.candidate"
        candidate.write_bytes(b"candidate bytes")
        neighbor = Path(self.temp.name) / "node.previous"
        neighbor.write_bytes(b"keep")
        item = {
            "kind": "candidate", "path": str(candidate), "bytes": candidate.stat().st_size,
            "manifest": [{"path": ".", "sha256": hashlib.sha256(candidate.read_bytes()).hexdigest()}],
        }
        with mock.patch.object(cleanup, "_verify_open_file"):
            self.assertEqual(cleanup._delete_manifest(item), len(b"candidate bytes"))
        self.assertFalse(candidate.exists())
        self.assertEqual(neighbor.read_bytes(), b"keep")

    def test_revalidation_detects_inode_replacement(self):
        job = {"release": str(self.root.parent),
               "release_identity": cleanup._identity(self.root.parent, allow_dir=True)}
        candidate = Path(self.temp.name) / "node.candidate"
        candidate.write_bytes(b"before")
        item = {
            "kind": "candidate", "path": str(candidate),
            "identity": cleanup._identity(candidate), "root_mode": stat.S_IMODE(candidate.stat().st_mode),
            "manifest": [], "bytes": 6,
        }
        old = Path(self.temp.name) / "old"
        candidate.rename(old)
        candidate.write_bytes(b"before")
        with mock.patch.object(cleanup, "_release_path", return_value=self.root.parent):
            with self.assertRaisesRegex(cleanup.CleanupError, "inode changed"):
                cleanup._revalidate_artifact(item, job)


class APIAndProviderTests(unittest.TestCase):
    def test_live_http_endpoint_validation_without_api_callback_bypass(self):
        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b'{"ok":true}'
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch("urllib.request.build_opener", return_value=opener):
            for path in ("/api/v1/status", "/api/v1/blocks/0", "/api/v1/blocks/123"):
                self.assertEqual(cleanup._http_json("http://127.0.0.1:8545" + path), {"ok": True})
            self.assertEqual(opener.open.call_count, 3)
            for url in (
                "http://example.com:8545/api/v1/status",
                "https://127.0.0.1:8545/api/v1/status",
                "http://127.0.0.1:8545/admin",
                "http://user:password@127.0.0.1:8545/api/v1/status",
                "http://127.0.0.1:8545/api/v1/blocks/../admin",
                "http://127.0.0.1:8545/api/v1/status?redirect=1",
            ):
                with self.subTest(url=url), self.assertRaises(cleanup.CleanupError):
                    cleanup._http_json(url)
            self.assertEqual(opener.open.call_count, 3)

    def test_known_status_and_block_response_schemas(self):
        status = {"ok": True, "height": 41, "syncing": False, "utxo_rebuilding": False}
        with mock.patch.object(cleanup, "_http_json", return_value=status) as mocked:
            self.assertEqual(cleanup._ready_height("http://127.0.0.1:8545"), 41)
            mocked.assert_called_once_with("http://127.0.0.1:8545/api/v1/status", None)
        block_hash = "a" * 64
        with mock.patch.object(cleanup, "_http_json", return_value={
            "height": 0, "hash": block_hash,
        }):
            self.assertEqual(cleanup._block("http://127.0.0.1:8545", 0), (0, block_hash))

    def test_ready_rejects_unrecognized_or_not_ready_responses(self):
        for response in (
            {"status": "ok", "height": 1},
            {"ok": True, "height": 1, "syncing": True, "utxo_rebuilding": False},
            {"ok": True, "height": 1, "syncing": False, "utxo_rebuilding": True},
            {"ok": True, "height": 1, "syncing": False},
            [],
        ):
            with self.subTest(response=response), mock.patch.object(
                cleanup, "_http_json", return_value=response
            ), self.assertRaises(cleanup.CleanupError):
                cleanup._ready_height("http://127.0.0.1:8545")

    def test_b2_endpoint_is_https_and_host_validated(self):
        valid = {"s3backup": {
            "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "archive",
            "accessKeyId": "id", "secretAccessKey": "secret",
        }}
        self.assertEqual(cleanup.NativeB2Provider._configuration(valid)[1], "archive")
        for endpoint in ("http://s3.us-west-004.backblazeb2.com", "https://evilbackblazeb2.com"):
            value = {"s3backup": dict(valid["s3backup"], endpoint=endpoint)}
            with self.subTest(endpoint=endpoint), self.assertRaises(cleanup.CleanupError):
                cleanup.NativeB2Provider._configuration(value)

    def test_provider_fingerprint_changes_when_secret_changes(self):
        complete_a = {"s3backup": {
            "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "archive",
            "accessKeyId": "id", "secretAccessKey": "first",
        }}
        complete_b = dict(complete_a, s3backup=dict(complete_a["s3backup"], secretAccessKey="second"))
        self.assertNotEqual(
            cleanup.DEFAULT_PROVIDER.fingerprint(complete_a),
            cleanup.DEFAULT_PROVIDER.fingerprint(complete_b),
        )

    def test_native_b2_version_rows_require_content_length_and_normalize(self):
        provider = cleanup.NativeB2Provider()
        settings = {"s3backup": {
            "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "archive",
            "accessKeyId": "id", "secretAccessKey": "secret",
        }}
        response = {"files": [{
            "fileId": "immutable-id", "fileName": "archive.tar.gz", "bucketId": "bucket-id",
            "action": "upload", "uploadTimestamp": 42, "contentLength": 123,
        }]}
        with mock.patch.object(provider, "_authenticate", return_value=(
            "https://s3.us-west-004.backblazeb2.com", "archive", "id", "secret",
            "token", "bucket-id", "https://api.backblazeb2.com",
        )), mock.patch.object(provider, "_request", return_value=response):
            rows = provider.versions(settings, "archive.tar.gz")
        self.assertEqual(rows[0]["size"], 123)
        self.assertEqual(rows[0]["uploadTimestamp"], 42)
        response["files"][0]["size"] = 123
        del response["files"][0]["contentLength"]
        with mock.patch.object(provider, "_authenticate", return_value=(
            "https://s3.us-west-004.backblazeb2.com", "archive", "id", "secret",
            "token", "bucket-id", "https://api.backblazeb2.com",
        )), mock.patch.object(provider, "_request", return_value=response), self.assertRaises(cleanup.CleanupError):
            provider.versions(settings, "archive.tar.gz")

    def test_proof_schema_is_exact_and_rejects_duplicate_or_invalid_anchors(self):
        valid = {
            "schema": 1, "genesis_hash": "a" * 64, "tip_height": 12, "tip_hash": "b" * 64,
            "anchors": [{"id": "00000000-0000-0000-0000-000000000001",
                         "height": 11, "hash": "c" * 64}],
        }
        cleanup._validate_proof(valid)
        with self.assertRaises(cleanup.CleanupError):
            cleanup._validate_proof(dict(valid, extra=True))
        invalid_anchor = dict(valid, anchors=[dict(valid["anchors"][0], height=12)])
        with self.assertRaises(cleanup.CleanupError):
            cleanup._validate_proof(invalid_anchor)

    def test_native_b2_listing_is_required_for_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            job_id = "00000000-0000-0000-0000-000000000000"
            job = {
                "completed_at": 1,
                "id": job_id,
                "completion_height": 3,
                "origin": {"genesis_hash": "g", "chain_db": {}, "config_sha256": "c",
                           "height": 1, "hash": "a" * 64},
            }
            receipt = {
                "schema": 1, "verified_at": int(__import__("time").time()), "backup_height": 4,
                "genesis_hash": "g", "source": {}, "config_sha256": "c",
                "anchors": [{"id": job_id, "height": 1, "hash": "a" * 64}],
                "archive_sha256": "d" * 64, "archive_size": 10,
                "provider_fingerprint": "fp",
                "b2": {"file_id": "id", "file_name": "archive.tar.gz",
                       "remote_object": "archive.tar.gz", "bucket_id": "bucket-id",
                       "bucket": "archive", "endpoint": "https://s3.us-west-004.backblazeb2.com",
                       "file_size": 10, "upload_timestamp": 12},
            }
            class EmptyProvider:
                def identity(self, settings):
                    return {"endpoint": receipt["b2"]["endpoint"], "bucket": "archive",
                            "bucket_id": "bucket-id"}
                def fingerprint(self, settings):
                    return "fp"
                def versions(self, settings, object_name):
                    return []
            with mock.patch.object(cleanup, "_safe_external_json", return_value=receipt), mock.patch.object(
                cleanup, "read_json", return_value={"s3backup": {}}
            ):
                with self.assertRaisesRegex(cleanup.CleanupError, "no longer present"):
                    cleanup._receipt(Path(tmp) / "settings.json", job, {}, provider=EmptyProvider())


class B2PaginationTests(unittest.TestCase):
    def test_versions_use_prefix_cursor_and_exact_filter_matching_objects(self):
        provider = cleanup.NativeB2Provider()
        settings = {"s3backup": {
            "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "archive",
            "accessKeyId": "id", "secretAccessKey": "secret",
        }}

        def upload(file_id, file_name, timestamp, size):
            return {
                "fileId": file_id, "fileName": file_name, "bucketId": "bucket-id",
                "action": "upload", "uploadTimestamp": timestamp, "contentLength": size,
            }

        pages = [
            {
                "files": [
                    upload("new-id", "archive.tar.gz", 200, 123),
                    # Prefix matches, but this is a distinct neighboring object.
                    {"fileName": "archive.tar.gz.previous"},
                ],
                "nextFileName": "archive.tar.gz.previous", "nextFileId": "neighbor-id",
            },
            {
                "files": [
                    # A similarly named object must not be normalized as the target.
                    {"fileName": "archive.tar.gz.older"},
                    upload("old-id", "archive.tar.gz", 100, 111),
                ],
                "nextFileName": None, "nextFileId": None,
            },
        ]
        with mock.patch.object(provider, "_authenticate", return_value=(
            "https://s3.us-west-004.backblazeb2.com", "archive", "id", "secret",
            "token", "bucket-id", "https://api.backblazeb2.com",
        )), mock.patch.object(provider, "_request", side_effect=pages) as request:
            rows = provider.versions(settings, "archive.tar.gz")

        self.assertEqual([row["fileId"] for row in rows], ["new-id", "old-id"])
        self.assertEqual([row["fileName"] for row in rows], ["archive.tar.gz"] * 2)
        first_payload = request.call_args_list[0].args[2]
        second_payload = request.call_args_list[1].args[2]
        self.assertEqual(first_payload["prefix"], "archive.tar.gz")
        self.assertNotIn("fileName", first_payload)
        self.assertEqual(second_payload["prefix"], "archive.tar.gz")
        self.assertEqual(second_payload["startFileName"], "archive.tar.gz.previous")
        self.assertEqual(second_payload["startFileId"], "neighbor-id")

    def test_matching_prefix_rows_still_require_strict_version_metadata(self):
        provider = cleanup.NativeB2Provider()
        settings = {"s3backup": {
            "endpoint": "https://s3.us-west-004.backblazeb2.com", "bucket": "archive",
            "accessKeyId": "id", "secretAccessKey": "secret",
        }}
        malformed = {"files": [{
            "fileId": "target-id", "fileName": "archive.tar.gz",
            "bucketId": "bucket-id", "action": "upload", "uploadTimestamp": 10,
            "size": 100,
        }]}
        with mock.patch.object(provider, "_authenticate", return_value=(
            "https://s3.us-west-004.backblazeb2.com", "archive", "id", "secret",
            "token", "bucket-id", "https://api.backblazeb2.com",
        )), mock.patch.object(provider, "_request", return_value=malformed), self.assertRaisesRegex(
            cleanup.CleanupError, "contentLength"
        ):
            provider.versions(settings, "archive.tar.gz")


class _FixtureProvider:
    def __init__(self):
        self.rows = [{
            "fileId": "immutable-b2-file-id", "fileName": "backup.tar.gz",
            "bucketId": "fixture-bucket-id", "action": "upload",
            "uploadTimestamp": 500, "size": 5000,
        }]

    def identity(self, settings):
        return {
            "endpoint": "https://s3.us-west-004.backblazeb2.com",
            "bucket": "fixture-bucket", "access_key_id": "fixture-access",
            "bucket_id": "fixture-bucket-id",
        }

    def fingerprint(self, settings):
        return "fixture-provider-fingerprint"

    def versions(self, settings, object_name):
        return [dict(row, fileName=object_name) for row in self.rows]


class LifecycleFixture:
    def __init__(self, root: Path):
        self.root = root
        self.state = root / "state"
        self.releases = root / "releases"
        self.release = self.releases / "r1"
        self.data = root / "data"
        self.config = root / "integration-settings.json"
        self.settings = root / "backup-settings.json"
        self.proc_root = root / "proc"
        self.installed = root / "aperod-node"
        self.api = "http://127.0.0.1:8545"
        self.current_pid = [101]
        self.ticks = {101: 111, 202: 222, 303: 333, 404: 444}
        self.height = [100]
        self.genesis = ["0" * 64]
        self.origin_hashes = {}
        self.time = [200000]
        for path in (self.state, self.releases, self.release, self.data, self.proc_root):
            path.mkdir(parents=True, exist_ok=True)
        (self.data / "chain.db").mkdir()
        self.config.write_text('{"network":"fixture"}')
        self.installed.write_bytes(b"installed executable bytes")
        self.installed.chmod(0o700)
        self.settings.write_text(json.dumps({"s3backup": {"fixture": True}}))
        self.neighbor = self.release / "node.previous"
        self.neighbor.write_bytes(b"keep this neighboring release artifact")
        self.stopped = self.release / "stopped-copy"
        self.stopped.mkdir(mode=0o700)
        (self.stopped / "CURRENT").write_text("MANIFEST-000001\n")
        (self.stopped / "MANIFEST-000001").write_text("leveldb manifest")
        (self.stopped / "000001.ldb").write_bytes(b"old leveldb bytes")
        self.candidate = self.release / "node.candidate"
        self.candidate.write_bytes(b"new candidate executable")
        self.add_process(101)
        self.runtime = cleanup.Runtime(
            state_dir=self.state, releases_dir=self.releases, settings_file=self.settings,
            installed_binary=self.installed, proc_root=self.proc_root,
            clock=self.clock, sleep=lambda _seconds: None, completion_polls=3,
            completion_interval=0, is_root=lambda: True, root_owned=lambda _info: True,
            service_pid=lambda _service: self.current_pid[0],
            process_start_ticks=lambda pid: self.ticks[pid],
            process_exe=lambda _pid: self.installed,
            live_exe_sha256=lambda _pid: hashlib.sha256(self.installed.read_bytes()).hexdigest(),
            api_json=self.api_json, verify_process_binding=lambda _pid, _data, _config: None,
        )
        self.provider = _FixtureProvider()
        self.job_id = None
        self.context_path = root / "backup-context.json"
        self.anchors_path = root / "anchors.json"
        self.proof_path = root / "proof.json"

    def clock(self):
        self.time[0] += 10
        return self.time[0]

    @staticmethod
    def block_hash(height):
        return f"{height:064x}"

    def api_json(self, url):
        from urllib.parse import urlsplit
        path = urlsplit(url).path
        if path == "/api/v1/status":
            return {
                "ok": True, "height": self.height[0], "syncing": False,
                "utxo_rebuilding": False,
            }
        prefix = "/api/v1/blocks/"
        if path.startswith(prefix):
            height = int(path[len(prefix):])
            return {"height": height, "hash": self.genesis[0] if height == 0
                    else self.origin_hashes.get(height, self.block_hash(height))}
        raise AssertionError("unexpected API path " + path)

    def add_process(self, pid, *, cwd=None, exe=None, cmdline=None, fds=(),
                    comm="fixture", stat_flags=0):
        proc = self.proc_root / str(pid)
        proc.mkdir(parents=True, exist_ok=True)
        cwd = cwd or self.root
        exe = exe or self.installed
        (proc / "cwd").symlink_to(cwd)
        (proc / "exe").symlink_to(exe)
        (proc / "root").symlink_to("/")
        (proc / "fd").mkdir(exist_ok=True)
        for descriptor, target in fds:
            (proc / "fd" / str(descriptor)).symlink_to(target)
        arguments = ["aperod-node", "--config", str(self.config)] if cmdline is None else cmdline
        (proc / "cmdline").write_bytes(
            b"\0".join(value.encode() for value in arguments) + (b"\0" if arguments else b"")
        )
        stat_fields = ["S"] + ["0"] * 5 + [str(stat_flags)] + ["0"] * 12 + [str(self.ticks[pid])]
        (proc / "stat").write_text(f"{pid} ({comm}) " + " ".join(stat_fields))

    def begin_and_mark(self):
        result = cleanup.begin(
            str(self.release), str(self.data), str(self.config), "aperod-node", self.api,
            runtime=self.runtime,
        )
        self.job_id = result["id"]
        cleanup.mark(self.job_id, str(self.stopped), "stopped-copy", runtime=self.runtime)
        cleanup.mark(self.job_id, str(self.candidate), "candidate", runtime=self.runtime)
        return self.job_id

    def restart_and_complete(self):
        self.current_pid[0] = 202
        self.add_process(202)
        self.height[0] = 105
        return cleanup.complete(
            self.job_id, hashlib.sha256(self.installed.read_bytes()).hexdigest(),
            runtime=self.runtime,
        )

    def backup_pin_and_publish(self):
        cleanup.backup_begin(
            str(self.data), str(self.config), "aperod-node", self.api,
            str(self.anchors_path), str(self.context_path), runtime=self.runtime,
        )
        cleanup.backup_pin(
            str(self.context_path), str(self.settings), "backup.tar.gz",
            runtime=self.runtime, provider=self.provider,
        )
        anchors = cleanup.read_json(self.anchors_path)
        proof = {
            "schema": 1, "genesis_hash": self.genesis[0], "tip_height": 110,
            "tip_hash": self.block_hash(110), "anchors": anchors["anchors"],
        }
        self.proof_path.write_text(json.dumps(proof))
        return cleanup.backup_publish(
            str(self.context_path), str(self.proof_path), str(self.settings), "backup.tar.gz",
            "d" * 64, 5000, 110, self.block_hash(110),
            runtime=self.runtime, provider=self.provider,
        )

    def complete_lifecycle(self):
        self.begin_and_mark()
        self.restart_and_complete()
        self.backup_pin_and_publish()


class ProcKernelThreadTests(unittest.TestCase):
    def test_real_proc_stat_pf_kthread_allows_empty_cmdline_without_bracketed_comm(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = LifecycleFixture(Path(tmp))
            fixture.add_process(
                404, cmdline=[], comm="kworker/0:1", stat_flags=cleanup.PF_KTHREAD,
            )
            cleanup._proc_references(
                101, fixture.candidate, fixture.config, runtime=fixture.runtime,
            )

    def test_empty_userspace_cmdline_is_not_inferred_to_be_kernel_thread_from_comm(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = LifecycleFixture(Path(tmp))
            fixture.add_process(404, cmdline=[], comm="[lookalike]", stat_flags=0)
            with self.assertRaisesRegex(cleanup.CleanupError, "unexpectedly empty"):
                cleanup._proc_references(
                    101, fixture.candidate, fixture.config, runtime=fixture.runtime,
                )


class RelativeProcessReferenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.fixture = LifecycleFixture(Path(self.temp.name))

    def test_relative_data_dir_argv_from_release_cwd_blocks_stopped_copy(self):
        fixture = self.fixture
        fixture.add_process(
            303, cwd=fixture.release,
            cmdline=["recovery", "--data-dir=stopped-copy"],
        )
        with self.assertRaisesRegex(cleanup.CleanupError, "command line references cleanup artifact"):
            cleanup._proc_references(
                101, fixture.stopped, fixture.config, runtime=fixture.runtime,
            )

    def test_relative_data_dir_argv_symlink_alias_blocks_stopped_copy(self):
        fixture = self.fixture
        alias = fixture.release / "copy-alias"
        alias.symlink_to(fixture.stopped, target_is_directory=True)
        fixture.add_process(
            303, cwd=fixture.release,
            cmdline=["recovery", "--data-dir=copy-alias"],
        )
        with self.assertRaisesRegex(cleanup.CleanupError, "command line references cleanup artifact"):
            cleanup._proc_references(
                101, fixture.stopped, fixture.config, runtime=fixture.runtime,
            )

    def test_relative_data_dir_inside_relative_config_blocks_stopped_copy(self):
        fixture = self.fixture
        restore_config = fixture.release / "restore.yaml"
        restore_config.write_text("data_dir: ./stopped-copy\n")
        fixture.add_process(
            303, cwd=fixture.release,
            cmdline=["recovery", "--config=./restore.yaml"],
        )
        with self.assertRaisesRegex(cleanup.CleanupError, "config references cleanup artifact"):
            cleanup._proc_references(
                101, fixture.stopped, fixture.config, runtime=fixture.runtime,
            )

    def test_config_path_values_resolve_from_config_directory_as_well_as_cwd(self):
        fixture = self.fixture
        config_dir = fixture.release / "conf"
        config_dir.mkdir()
        (config_dir / "restore.yaml").write_text("data_dir: ../stopped-copy\n")
        fixture.add_process(
            303, cwd=fixture.release,
            cmdline=["recovery", "--config=./conf/restore.yaml"],
        )
        with self.assertRaisesRegex(cleanup.CleanupError, "config references cleanup artifact"):
            cleanup._proc_references(
                101, fixture.stopped, fixture.config, runtime=fixture.runtime,
            )


class RolloutLifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.fixture = LifecycleFixture(Path(self.temp.name))

    def test_restart_backup_pin_publish_dry_run_apply_and_neighbor_preservation(self):
        fixture = self.fixture
        fixture.complete_lifecycle()
        record = cleanup._safe_record(fixture.state / (fixture.job_id + ".json"), fixture.runtime)
        self.assertNotEqual(record["origin"]["pid"], record["runtime_pid"])
        dry = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        self.assertTrue(all(row["eligible"] and not row["removed"] for row in dry["artifacts"]))
        self.assertTrue(fixture.candidate.exists())
        self.assertTrue(fixture.stopped.exists())
        self.assertTrue(fixture.neighbor.exists())
        applied = cleanup.scan(True, runtime=fixture.runtime, provider=fixture.provider)
        self.assertTrue(all(row["eligible"] and row["removed"] for row in applied["artifacts"]))
        self.assertFalse(fixture.candidate.exists())
        self.assertFalse(fixture.stopped.exists())
        self.assertEqual(fixture.neighbor.read_bytes(), b"keep this neighboring release artifact")
        second = cleanup.scan(True, runtime=fixture.runtime, provider=fixture.provider)
        self.assertEqual(second["artifacts"], [])
        next_anchors = fixture.root / "next-anchors.json"
        next_context = fixture.root / "next-context.json"
        cleanup.backup_begin(
            str(fixture.data), str(fixture.config), "aperod-node", fixture.api,
            str(next_anchors), str(next_context), runtime=fixture.runtime,
        )
        self.assertEqual(cleanup.read_json(next_anchors)["anchors"], [])

    def test_missing_stale_and_malformed_receipts_refuse_cleanup(self):
        fixture = self.fixture
        fixture.begin_and_mark()
        fixture.restart_and_complete()
        missing = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        self.assertFalse(any(row["eligible"] for row in missing["artifacts"]))
        self.assertIn("verified-generation", missing["artifacts"][0]["reasons"][0])
        fixture.backup_pin_and_publish()
        receipt_path = fixture.settings.parent / "verified-generation.json"
        receipt = cleanup.read_json(receipt_path)
        receipt["verified_at"] = 1
        cleanup.atomic_json(receipt_path, receipt)
        stale = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        self.assertFalse(any(row["eligible"] for row in stale["artifacts"]))
        self.assertIn("stale", stale["artifacts"][0]["reasons"][0])
        receipt_path.write_text('{"schema":1,"schema":1}')
        receipt_path.chmod(0o600)
        malformed = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        self.assertFalse(any(row["eligible"] for row in malformed["artifacts"]))
        self.assertTrue(fixture.candidate.exists())

    def test_receipt_genesis_anchor_and_source_mismatches_refuse_cleanup(self):
        for field, value, reason in (
            ("genesis_hash", "f" * 64, "source anchor"),
            ("source", {"device": 99, "inode": 100}, "source anchor"),
            ("anchors", [], "omits this rollout anchor"),
        ):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as temp:
                fixture = LifecycleFixture(Path(temp))
                fixture.complete_lifecycle()
                receipt_path = fixture.settings.parent / "verified-generation.json"
                receipt = cleanup.read_json(receipt_path)
                receipt[field] = value
                cleanup.atomic_json(receipt_path, receipt)
                report = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
                self.assertFalse(any(row["eligible"] for row in report["artifacts"]))
                self.assertTrue(any(reason in msg for row in report["artifacts"] for msg in row["reasons"]))

    def test_missing_immutable_remote_version_refuses_cleanup(self):
        fixture = self.fixture
        fixture.complete_lifecycle()
        fixture.provider.rows = []
        report = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        self.assertFalse(any(row["eligible"] for row in report["artifacts"]))
        self.assertTrue(any("immutable B2" in msg for row in report["artifacts"] for msg in row["reasons"]))

    def test_b2_head_change_between_publish_checks_refuses_receipt(self):
        fixture = self.fixture
        fixture.begin_and_mark()
        fixture.restart_and_complete()

        class ChangingProvider(_FixtureProvider):
            def __init__(self):
                super().__init__()
                self.calls = 0

            def versions(self, settings, object_name):
                self.calls += 1
                if self.calls >= 3:
                    return [{
                        "fileId": "replacement-file-id", "fileName": object_name,
                        "bucketId": "fixture-bucket-id", "action": "upload",
                        "uploadTimestamp": 501, "size": 5000,
                    }]
                return super().versions(settings, object_name)

        fixture.provider = ChangingProvider()
        with self.assertRaisesRegex(cleanup.CleanupError, "head changed"):
            fixture.backup_pin_and_publish()
        self.assertFalse((fixture.settings.parent / "verified-generation.json").exists())

    def test_backup_begin_fails_closed_for_unavailable_pending_origin_anchor(self):
        fixture = self.fixture
        fixture.begin_and_mark()
        fixture.restart_and_complete()
        fixture.origin_hashes[100] = "f" * 64
        with self.assertRaisesRegex(cleanup.CleanupError, "unavailable or noncanonical"):
            cleanup.backup_begin(
                str(fixture.data), str(fixture.config), "aperod-node", fixture.api,
                str(fixture.anchors_path), str(fixture.context_path), runtime=fixture.runtime,
            )

    def test_incomplete_jobs_are_not_added_to_backup_anchors(self):
        fixture = self.fixture
        fixture.begin_and_mark()
        cleanup.backup_begin(
            str(fixture.data), str(fixture.config), "aperod-node", fixture.api,
            str(fixture.anchors_path), str(fixture.context_path), runtime=fixture.runtime,
        )
        self.assertEqual(cleanup.read_json(fixture.anchors_path)["anchors"], [])

    def test_other_host_process_descriptor_reference_refuses_cleanup(self):
        fixture = self.fixture
        fixture.complete_lifecycle()
        fixture.add_process(303, fds=((9, fixture.candidate),))
        report = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        candidate = next(row for row in report["artifacts"] if row["path"] == str(fixture.candidate))
        self.assertFalse(candidate["eligible"])
        self.assertTrue(any("host process references" in msg for msg in candidate["reasons"]))

    def test_other_process_config_reference_and_unreadable_proc_entry_fail_closed(self):
        fixture = self.fixture
        fixture.complete_lifecycle()
        fixture.add_process(
            303, cmdline=["worker", "--config", str(fixture.candidate)],
        )
        report = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        candidate = next(row for row in report["artifacts"] if row["path"] == str(fixture.candidate))
        self.assertFalse(candidate["eligible"])
        self.assertTrue(any("command line references" in msg for msg in candidate["reasons"]))

        shutil.rmtree(fixture.proc_root / "303")
        fixture.add_process(404)
        original_readlink = os.readlink

        def unreadable(path, *args, **kwargs):
            if Path(path) == fixture.proc_root / "404" / "cwd":
                raise PermissionError("fixture proc access denied")
            return original_readlink(path, *args, **kwargs)

        with mock.patch.object(cleanup.os, "readlink", side_effect=unreadable):
            report = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
        candidate = next(row for row in report["artifacts"] if row["path"] == str(fixture.candidate))
        self.assertFalse(candidate["eligible"])
        self.assertTrue(any("completely inspect" in msg for msg in candidate["reasons"]))

    def test_pid_drift_during_completion_is_refused(self):
        fixture = self.fixture
        fixture.begin_and_mark()
        fixture.current_pid[0] = 202
        fixture.add_process(202)
        fixture.height[0] = 105
        identities = iter((202, 303))
        fixture.runtime.service_pid = lambda _service: next(identities)
        with self.assertRaisesRegex(cleanup.CleanupError, "changed during completion"):
            cleanup.complete(fixture.job_id, hashlib.sha256(fixture.installed.read_bytes()).hexdigest(),
                             runtime=fixture.runtime)

    def test_backup_capture_publish_and_scan_reject_pid_or_start_tick_drift(self):
        for operation, identity in (
            ("capture", "pid"), ("capture", "ticks"),
            ("publish", "pid"), ("publish", "ticks"),
            ("scan", "pid"), ("scan", "ticks"),
        ):
            with self.subTest(operation=operation, identity=identity), tempfile.TemporaryDirectory() as temp:
                fixture = LifecycleFixture(Path(temp))
                fixture.complete_lifecycle()
                if operation == "capture":
                    values = iter((202, 303)) if identity == "pid" else iter((222, 333))
                    if identity == "pid":
                        fixture.runtime.service_pid = lambda _service: next(values)
                    else:
                        fixture.runtime.process_start_ticks = lambda _pid: next(values)
                    with self.assertRaises(cleanup.CleanupError):
                        cleanup.backup_begin(
                            str(fixture.data), str(fixture.config), "aperod-node", fixture.api,
                            str(fixture.root / "capture-anchors.json"),
                            str(fixture.root / "capture-context.json"), runtime=fixture.runtime,
                        )
                elif operation == "publish":
                    values = iter((202, 303)) if identity == "pid" else iter((222, 333))
                    if identity == "pid":
                        fixture.runtime.service_pid = lambda _service: next(values)
                    else:
                        fixture.runtime.process_start_ticks = lambda _pid: next(values)
                    with self.assertRaises(cleanup.CleanupError):
                        cleanup.backup_publish(
                            str(fixture.context_path), str(fixture.proof_path), str(fixture.settings),
                            "backup.tar.gz", "d" * 64, 5000, 110, fixture.block_hash(110),
                            runtime=fixture.runtime, provider=fixture.provider,
                        )
                else:
                    if identity == "pid":
                        calls = [0]

                        def service_pid(_service):
                            calls[0] += 1
                            return 202 if calls[0] <= 5 else 303

                        fixture.runtime.service_pid = service_pid
                    else:
                        calls = [0]

                        def start_ticks(pid):
                            if pid != 202:
                                return fixture.ticks[pid]
                            calls[0] += 1
                            return 222 if calls[0] <= 9 else 333

                        fixture.runtime.process_start_ticks = start_ticks
                    report = cleanup.scan(runtime=fixture.runtime, provider=fixture.provider)
                    self.assertFalse(any(row["eligible"] for row in report["artifacts"]))

    def test_config_genesis_and_anchor_changes_refuse_completion(self):
        for mutation, message in (
            ("config", "config identity or content changed"),
            ("genesis", "genesis changed"),
            ("anchor", "origin anchor changed"),
        ):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temp:
                fixture = LifecycleFixture(Path(temp))
                fixture.begin_and_mark()
                fixture.current_pid[0] = 202
                fixture.add_process(202)
                fixture.height[0] = 105
                if mutation == "config":
                    fixture.config.write_text('{"network":"changed"}')
                elif mutation == "genesis":
                    fixture.genesis[0] = "f" * 64
                else:
                    fixture.origin_hashes[100] = "f" * 64
                with self.assertRaisesRegex(cleanup.CleanupError, message):
                    cleanup.complete(
                        fixture.job_id, hashlib.sha256(fixture.installed.read_bytes()).hexdigest(),
                        runtime=fixture.runtime,
                    )


class CLIContractTests(unittest.TestCase):
    def test_parser_has_all_requested_commands(self):
        command_args = {
            "begin": ["--release", "/r", "--data-dir", "/d", "--config", "/c",
                      "--service", "aperod-node", "--api-url", "http://127.0.0.1:8545"],
            "mark": ["--id", "00000000-0000-0000-0000-000000000000",
                     "--artifact", "/r/node.candidate", "--kind", "candidate"],
            "complete": ["--id", "00000000-0000-0000-0000-000000000000",
                         "--expected-binary-sha256", "a" * 64],
            "scan": [],
            "backup-begin": ["--data-dir", "/d", "--config", "/c", "--service", "aperod-node",
                             "--api-url", "http://127.0.0.1:8545", "--anchors-output", "/a",
                             "--context-output", "/cxt"],
            "backup-pin": ["--context", "/cxt", "--settings", "/settings",
                           "--remote-object", "archive.tar.gz"],
            "backup-publish": ["--context", "/cxt", "--proof", "/proof", "--settings", "/settings",
                               "--remote-object", "archive.tar.gz", "--archive-sha256", "a" * 64,
                               "--archive-size", "1", "--tip-height", "2", "--tip-hash", "b" * 64],
            "backup-abort": ["--context", "/cxt"],
        }
        for command, values in command_args.items():
            with self.subTest(command=command):
                args = cli._parser().parse_args([command, *values])
                self.assertIsNotNone(args)

    def test_state_enumeration_uses_explicit_names(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "not-state.txt").write_text("preserve")
            (root / "00000000-0000-0000-0000-000000000000.json").write_text("{}")
            paths = cleanup._state_json_paths(root)
            self.assertEqual([path.name for path in paths],
                             ["00000000-0000-0000-0000-000000000000.json"])

    def test_incomplete_job_is_never_a_scan_candidate(self):
        job_id = "00000000-0000-0000-0000-000000000001"
        path = Path(job_id + ".json")
        with mock.patch.object(cleanup, "state_lock", return_value=nullcontext()), mock.patch.object(
            cleanup, "_state_json_paths", return_value=[path]
        ), mock.patch.object(cleanup, "_safe_record", return_value={
            "schema": 1, "id": job_id, "status": "begun", "marks": [],
        }):
            report = cleanup.scan()
        self.assertEqual(report["artifacts"], [])
        self.assertEqual(report["bytes"], 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)