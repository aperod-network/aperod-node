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
import itertools
import json
import signal
from types import SimpleNamespace
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
    def __init__(self, root: Path, name="legacy-1", table=None):
        self.root = root
        self.state = root / "state"
        self.releases = root / "releases"
        self.release = self.releases / name
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
        (self.stopped / "chain.db" / "000001.ldb").write_bytes(
            b"historical leveldb" if table is None else table)
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
        self.proc.mkdir(exist_ok=True)
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


class HeadroomRetryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        import uuid
        # Atomic metadata writes also consume uuid4 for temporary filenames.
        ids = itertools.count(1)
        with mock.patch.object(retirement.uuid, "uuid4",
                               side_effect=lambda: uuid.UUID(int=next(ids))):
            self.big = RetirementFixture(root, "large", b"x" * 4096)
            self.small = RetirementFixture(root, "small", b"x" * 2048)
        self.big_id, self.small_id = (f.registration["id"] for f in (self.big, self.small))
        self.big_record = cleanup.read_json(self.big.record_path())
        self.small_record = cleanup.read_json(self.small.record_path())
        self.required = 4 * sum(t["bytes"] for t in self.big_record["targets"])
        self.free = self.required - 512
        self.calls = []
        self.real_delete = retirement._delete_manifest
        for target, name, value in (
            (retirement, "MIN_FREE_SPACE", 0),
            (retirement, "_settings", mock.Mock(return_value={"s3backup": {}})),
            (retirement, "_origin_unchanged", mock.Mock()),
            (retirement, "_proc_references", mock.Mock()),
            (retirement, "_confirm_archive_proof", mock.Mock()),
        ):
            patcher = mock.patch.object(target, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        for name, effect in (
            ("_archive_and_verify", self.archive),
            ("_delete_manifest", self.delete),
        ):
            patcher = mock.patch.object(retirement, name, side_effect=effect)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = mock.patch.object(retirement.shutil, "disk_usage",
                                   side_effect=lambda _path: SimpleNamespace(free=self.free))
        patcher.start()
        self.addCleanup(patcher.stop)

    def proof(self, record):
        return {
            "sha256": "a" * 64, "size": 99, "file_id": "immutable-" + record["id"],
            "bucket_id": "bucket", "bucket": "vault", "endpoint": "https://example",
            "provider_fingerprint": "b" * 64, "manifest_sha256": record["manifest_sha256"],
            "verified_at": 123,
            "object_name": "historical-retirement/" + record["id"] + "/archive.tar.gz.gpg",
        }

    def archive(self, record, stopped, targets, runtime, settings):
        self.calls.append(record["id"])
        retirement._ensure_staging_headroom(Path(runtime.state_dir), targets)
        return self.proof(record)

    def delete(self, target, runtime):
        removed = self.real_delete(target, runtime)
        self.free += removed
        return removed

    def scan(self):
        return retirement.scan_retirements(apply=True, runtime=self.big.runtime)

    def test_big_then_small_then_big_completes_in_one_scan(self):
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id, self.small_id, self.big_id])
        self.assertEqual(result["status"], "ok")
        self.assertEqual(len(result["retirements"]), 2)
        for fixture in (self.big, self.small):
            record = cleanup.read_json(fixture.record_path())
            self.assertEqual(record["status"], "completed")
            self.assertEqual(len(record["receipts"]), 3)
            self.assertFalse((fixture.stopped / "chain.db").exists())
            self.assertTrue((fixture.stopped / "p2p_identity.key").exists())
            self.assertTrue((fixture.release / "node.previous").exists())

    def test_no_deletion_means_no_retry(self):
        self.small.record_path().unlink()
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id])
        self.assertEqual(result["status"], "refused")
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_logical_removed_bytes_without_physical_headroom_do_not_retry(self):
        retirement._delete_manifest.side_effect = self.real_delete
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id, self.small_id])
        self.assertEqual(result["status"], "refused")
        self.assertEqual(cleanup.read_json(self.small.record_path())["status"], "completed")
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_non_typed_headroom_message_is_not_retried(self):
        def archive(record, *args):
            if record["id"] == self.big_id:
                self.calls.append(record["id"])
                raise cleanup.CleanupError("insufficient staging disk headroom for historical archive")
            return self.archive(record, *args)
        retirement._archive_and_verify.side_effect = archive
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id, self.small_id])
        self.assertEqual(result["status"], "refused")

    def permanent_failure(self, error):
        def archive(record, *args):
            if record["id"] == self.big_id:
                self.calls.append(record["id"])
                raise error
            return self.archive(record, *args)
        retirement._archive_and_verify.side_effect = archive
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id, self.small_id])
        self.assertEqual(result["status"], "refused")
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_network_failure_not_retried_after_other_copy_frees_space(self):
        self.permanent_failure(cleanup.CleanupError("historical archive upload failed"))

    def test_access_failure_not_retried_after_other_copy_frees_space(self):
        self.permanent_failure(PermissionError("access denied"))

    def test_two_deferred_copies_retry_smallest_requirement_first(self):
        root = self.big.root
        import uuid
        ids = itertools.count(100)
        with mock.patch.object(retirement.uuid, "uuid4",
                               side_effect=lambda: uuid.UUID(int=next(ids))):
            medium = RetirementFixture(root, "medium", b"x" * 3072)
        # Place the small successful seed last in the first pass; the deferred
        # records must then be reconsidered in ascending staging requirement.
        old = self.small.record_path()
        record = cleanup.read_json(old)
        record["id"] = str(uuid.UUID(int=200))
        cleanup.atomic_json(self.small.state / ("retirement-" + record["id"] + ".json"), record)
        old.unlink()
        self.small.registration["id"] = record["id"]
        self.small_id = record["id"]
        medium_id = medium.registration["id"]
        self.free = 4 * sum(t["bytes"] for t in cleanup.read_json(medium.record_path())["targets"]) - 512
        result = self.scan()
        self.assertEqual(result["status"], "ok")
        self.assertEqual(self.calls, [self.big_id, medium_id, self.small_id, medium_id, self.big_id])

    def test_new_process_reference_on_retry_keeps_copy(self):
        def references(_pid, path, *_args, **_kwargs):
            if self.small_id in self.calls and str(self.big.stopped) in str(path):
                raise cleanup.CleanupError("new process reference")
        retirement._proc_references.side_effect = references
        result = self.scan()
        self.assertEqual(result["status"], "refused")
        self.assertEqual(self.calls, [self.big_id, self.small_id])
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_external_disk_growth_does_not_bypass_reserve_or_loop(self):
        def archive(record, *args):
            if self.calls.count(self.big_id) == 1 and record["id"] == self.big_id:
                self.free = 0
            return self.archive(record, *args)
        retirement._archive_and_verify.side_effect = archive
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id, self.small_id, self.big_id])
        self.assertEqual(result["status"], "refused")
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_changed_copy_is_revalidated_before_retry_archive(self):
        def delete(target, runtime):
            removed = self.delete(target, runtime)
            if str(self.small.stopped) in target["path"]:
                (self.big.stopped / "chain.db/000001.ldb").write_bytes(b"mutated")
            return removed
        retirement._delete_manifest.side_effect = delete
        result = self.scan()
        self.assertEqual(self.calls, [self.big_id, self.small_id])
        self.assertEqual(result["status"], "refused")
        self.assertIn("changed", result["retirements"][0]["error"])
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_preexisting_archive_gets_full_readback_again_not_reuploaded(self):
        record = self.big_record
        record["archive"] = self.proof(record)
        record["status"] = "archived"
        cleanup.atomic_json(self.big.record_path(), record)
        reads = []
        def confirm(value, settings, runtime, *, readback=True):
            if value["id"] == self.big_id and readback:
                reads.append(value["archive"]["file_id"])
                retirement._ensure_staging_headroom(Path(runtime.state_dir), value["targets"])
        retirement._confirm_archive_proof.side_effect = confirm
        result = self.scan()
        self.assertEqual(result["status"], "ok")
        self.assertEqual(self.calls, [self.small_id])
        self.assertEqual(reads, ["immutable-" + self.big_id] * 2)
        self.assertEqual(cleanup.read_json(self.big.record_path())["archive"], record["archive"])

    def test_missing_immutable_version_on_retry_keeps_source_and_old_proof(self):
        record = self.big_record
        record["archive"] = self.proof(record)
        record["status"] = "archived"
        cleanup.atomic_json(self.big.record_path(), record)
        reads = []
        def confirm(value, settings, runtime, *, readback=True):
            if value["id"] == self.big_id and readback:
                reads.append(value["id"])
                if len(reads) == 1:
                    retirement._ensure_staging_headroom(Path(runtime.state_dir), value["targets"])
                raise cleanup.CleanupError("verified immutable B2 fileId is no longer present")
        retirement._confirm_archive_proof.side_effect = confirm
        result = self.scan()
        self.assertEqual(result["status"], "refused")
        self.assertEqual(reads, [self.big_id] * 2)
        self.assertTrue((self.big.stopped / "chain.db").exists())
        self.assertEqual(cleanup.read_json(self.big.record_path())["archive"], record["archive"])

    def test_durable_partial_deletion_can_free_space_without_retrying_its_error(self):
        def confirm(record, settings, runtime, *, readback=True):
            if record["id"] == self.small_id and record["receipts"]:
                raise cleanup.CleanupError("network failure after first durable deletion")
        retirement._confirm_archive_proof.side_effect = confirm
        result = self.scan()
        self.assertEqual(result["status"], "refused")
        self.assertEqual(self.calls, [self.big_id, self.small_id, self.big_id])
        self.assertEqual(cleanup.read_json(self.big.record_path())["status"], "completed")
        partial = cleanup.read_json(self.small.record_path())
        self.assertEqual(partial["status"], "archived")
        self.assertEqual(len(partial["receipts"]), 1)
        self.assertTrue((self.small.stopped / "snapshot-legacy.gz").exists())
        self.assertTrue((self.small.stopped / "p2p_identity.key").exists())

    def test_unreadable_free_space_does_not_hide_completed_receipts(self):
        retirement.shutil.disk_usage.side_effect = OSError("statvfs denied")
        retirement._archive_and_verify.side_effect = lambda r, *_args: self.proof(r)
        result = self.scan()
        self.assertEqual(result["status"], "ok")
        self.assertEqual(cleanup.read_json(self.small.record_path())["status"], "completed")


@unittest.skipUnless(hasattr(os, "fork"), "requires isolated POSIX workers")
class WorkerInterruptionTests(unittest.TestCase):
    """Real worker death/state reopening; external adapters are modeled here.

    deploy/retirement-interruption-lab.py separately exercises the same boundaries
    with genuine process checks, statvfs and immutable off-host archive readback.
    """

    setUp = HeadroomRetryTests.setUp
    proof = HeadroomRetryTests.proof
    archive = HeadroomRetryTests.archive
    delete = HeadroomRetryTests.delete

    def trace_path(self):
        return self.big.state / "worker-trace.json"

    def spawn(self, fault=None, guard=None):
        pid = os.fork()
        if pid == 0:
            events, deleted, reads = [], [], []
            real_atomic = retirement.atomic_json
            real_revalidate = retirement._revalidate_record

            def trace(**values):
                real_atomic(self.trace_path(), {
                    "pid": os.getpid(), "events": events, "deleted": deleted,
                    "readbacks": reads, **values,
                })

            def commit(path, record):
                real_atomic(path, record)
                if record.get("id") == self.small_id and (
                    fault == "between" and record.get("status") == "completed"
                    or fault == "partial" and len(record.get("receipts", [])) == 1
                ):
                    trace(fault=fault)
                    os.kill(os.getpid(), signal.SIGKILL)

            def revalidate(record, runtime):
                events.append("source")
                return real_revalidate(record, runtime)

            def origin(*_args, **_kwargs):
                events.append("origin")
                if guard == "origin":
                    raise cleanup.CleanupError("changed origin authority")

            def proc(*_args, **_kwargs):
                events.append("process")
                if guard == "process":
                    raise cleanup.CleanupError("source is referenced by a process")

            def confirm(record, _settings, runtime, *, readback=True):
                events.append("archive-version")
                if readback:
                    reads.append(record["archive"]["file_id"])
                    if guard == "archive":
                        raise cleanup.CleanupError("immutable archive version unavailable")
                    events.append("space")
                    retirement._ensure_staging_headroom(Path(runtime.state_dir), record["targets"])

            def delete(target, runtime):
                deleted.append(target["path"])
                return self.delete(target, runtime)

            # Only this unit fixture models physical capacity; the integration
            # companion uses an actual limited filesystem and unchanged reserve.
            for fixture in (self.big, self.small):
                record = cleanup.read_json(fixture.record_path())
                receipted = {r["path"] for r in record["receipts"]}
                self.free += sum(t["bytes"] for t in record["targets"] if t["path"] in receipted)
            if guard == "space":
                self.free = 0
            retirement._origin_unchanged.side_effect = origin
            retirement._proc_references.side_effect = proc
            retirement._confirm_archive_proof.side_effect = confirm
            retirement._delete_manifest.side_effect = delete
            try:
                with mock.patch.object(retirement, "atomic_json", side_effect=commit), \
                     mock.patch.object(retirement, "_revalidate_record", side_effect=revalidate), \
                     mock.patch.object(cleanup, "DEFAULT_RUNTIME", self.big.runtime), \
                     contextlib.redirect_stdout(io.StringIO()) as output:
                    code = cli.main(["retirement-scan", "--apply"])
                trace(cli_exit=code, result=json.loads(output.getvalue()))
                os._exit(code)
            except BaseException:
                os._exit(90)
        _, status = os.waitpid(pid, 0)
        trace = cleanup.read_json(self.trace_path())
        self.assertEqual(trace["pid"], pid)
        if fault:
            self.assertTrue(os.WIFSIGNALED(status))
            self.assertEqual(os.WTERMSIG(status), signal.SIGKILL)
            self.assertEqual(trace["fault"], fault)
        else:
            self.assertTrue(os.WIFEXITED(status))
            self.assertEqual(os.WEXITSTATUS(status), trace["cli_exit"])
        return trace

    def interrupt_between(self):
        record = cleanup.read_json(self.big.record_path())
        record.update(status="archived", archive=self.proof(record))
        cleanup.atomic_json(self.big.record_path(), record)
        first = self.spawn(fault="between")
        self.assertEqual(cleanup.read_json(self.small.record_path())["status"], "completed")
        self.assertEqual(cleanup.read_json(self.big.record_path()), record)
        return first

    def test_fresh_worker_restores_durable_proofs_not_in_memory_retry_list(self):
        first = self.interrupt_between()
        small = self.small.record_path().read_bytes()
        large = cleanup.read_json(self.big.record_path())
        second = self.spawn()
        self.assertNotEqual(first["pid"], second["pid"])
        self.assertEqual(second["cli_exit"], 0)
        self.assertEqual(self.small.record_path().read_bytes(), small)
        self.assertTrue(all(str(self.big.stopped) in path for path in second["deleted"]))
        self.assertIn(large["archive"]["file_id"], second["readbacks"])
        self.assertEqual(cleanup.read_json(self.big.record_path())["archive"], large["archive"])
        for check in ("source", "origin", "process", "space", "archive-version"):
            self.assertIn(check, second["events"])
        third = self.spawn()
        self.assertEqual(third["cli_exit"], 0)
        self.assertEqual(third["deleted"], [])
        self.assertEqual(third["readbacks"], [])

    def test_restarted_worker_rechecks_each_authorization_boundary(self):
        self.interrupt_between()
        baseline = [f.record_path().read_bytes() for f in (self.big, self.small)]
        for guard in ("origin", "process", "space", "archive"):
            with self.subTest(guard=guard):
                result = self.spawn(guard=guard)
                self.assertEqual(result["cli_exit"], 1)
                self.assertEqual(result["deleted"], [])
                self.assertEqual([f.record_path().read_bytes() for f in (self.big, self.small)], baseline)

    def test_changed_source_after_worker_death_is_retained(self):
        self.interrupt_between()
        (self.big.stopped / "chain.db/000001.ldb").write_bytes(b"changed fixture")
        result = self.spawn()
        self.assertEqual(result["cli_exit"], 1)
        self.assertEqual(result["deleted"], [])
        self.assertTrue((self.big.stopped / "chain.db").exists())

    def test_partial_receipt_survives_death_and_new_worker_readback(self):
        self.spawn(fault="partial")
        partial = cleanup.read_json(self.small.record_path())
        self.assertEqual(len(partial["receipts"]), 1)
        self.assertEqual(partial["status"], "archived")
        refused = self.spawn(guard="archive")
        self.assertEqual(refused["cli_exit"], 1)
        self.assertTrue(all(str(self.small.stopped) not in path for path in refused["deleted"]))
        self.assertEqual(cleanup.read_json(self.small.record_path()), partial)
        result = self.spawn()
        self.assertEqual(result["cli_exit"], 0)
        completed = cleanup.read_json(self.small.record_path())
        self.assertEqual(completed["archive"], partial["archive"])
        self.assertEqual(completed["receipts"][:1], partial["receipts"])
        self.assertNotIn(partial["receipts"][0]["path"], result["deleted"])
        self.assertIn(partial["archive"]["file_id"], result["readbacks"])

    def test_partial_missing_receipt_never_infers_deleted_target_success(self):
        self.spawn(fault="partial")
        partial = cleanup.read_json(self.small.record_path())
        partial["receipts"] = []
        cleanup.atomic_json(self.small.record_path(), partial)
        result = self.spawn()
        small_outcome = next(r for r in result["result"]["retirements"] if r["id"] == self.small_id)
        self.assertEqual(small_outcome["status"], "refused")
        self.assertIn("disappeared unexpectedly", small_outcome["error"])
        self.assertEqual(cleanup.read_json(self.small.record_path()), partial)
        self.assertTrue((self.small.stopped / "snapshot-legacy.gz").exists())


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