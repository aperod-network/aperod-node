import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

import publish_public_repo as publisher
from publication_security import git, inspect


def fixture(root):
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
    (root / "go.mod").write_text("module example.invalid/test\n\ngo 1.25.0\n")
    subprocess.run(["git", "-C", str(root), "add", "."], check=True)
    subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)


class PublisherTests(unittest.TestCase):
    def test_candidate_symlink_cannot_overwrite_external_file(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            source, candidate = base / "source", base / "candidate"
            source.mkdir()
            candidate.mkdir()
            fixture(candidate)
            (source / "go.mod").write_text("module example.invalid/test\n")
            (source / "example.py").write_text("print('source')\n")
            external = base / "protected.txt"
            external.write_text("DO NOT MODIFY")
            (candidate / "example.py").symlink_to(external)
            subprocess.run(["git", "-C", str(candidate), "add", "."], check=True)
            publisher.prepare(source, candidate)
            self.assertEqual(external.read_text(), "DO NOT MODIFY")
            self.assertFalse((candidate / "example.py").is_symlink())

    def test_build_processes_do_not_receive_publication_credentials(self):
        with patch.dict(publisher.os.environ, {"PUBLIC_GITHUB_TOKEN": "TEST_ONLY",
                                              "SSH_PASSWORD": "TEST_ONLY"}), \
             patch.object(publisher.subprocess, "run") as child:
            publisher.run("go", "vet", "./...")
        env = child.call_args.kwargs["env"]
        self.assertNotIn("PUBLIC_GITHUB_TOKEN", env)
        self.assertNotIn("SSH_PASSWORD", env)

    def test_failed_required_check_never_merges(self):
        methods = []
        def request(method, url, data=None):
            methods.append(method)
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}}
            return {"check_runs": [{"name": "Whole-tree publication security",
                                   "status": "completed", "conclusion": "failure",
                                   "app": {"id": 15368}}]}
        with self.assertRaisesRegex(RuntimeError, "check failed"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request)
        self.assertNotIn("PUT", methods)

    def test_untrusted_status_provider_cannot_authorize_merge(self):
        methods = []
        def request(method, url, data=None):
            methods.append(method)
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}}
            return {"check_runs": [{"name": name, "status": "completed",
                                   "conclusion": "success", "app": {"id": 999}}
                                  for name in ("Whole-tree publication security", "go build & vet & test")]}
        with patch.object(publisher.time, "monotonic", side_effect=[0, 0, 2]), \
             patch.object(publisher.time, "sleep"):
            with self.assertRaisesRegex(RuntimeError, "did not complete"):
                publisher.await_merge({"number": 1, "html_url": "test"}, request, timeout=1)
        self.assertNotIn("PUT", methods)

    def test_merge_is_bound_to_reviewed_head(self):
        bodies = []
        def request(method, url, data=None):
            if method == "PUT":
                bodies.append(data)
                return {"merged": True}
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}}
            return {"check_runs": [{"name": name, "status": "completed",
                                   "conclusion": "success", "app": {"id": 15368}}
                                  for name in ("Whole-tree publication security", "go build & vet & test")]}
        publisher.await_merge({"number": 1, "html_url": "test"}, request)
        self.assertEqual(bodies[0]["sha"], "reviewed")
        self.assertNotIn("force", bodies[0])

    def test_policy_failure_prevents_every_network_request(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            (root / "credential.key").write_bytes(b"\xff" * 64)
            subprocess.run(["git", "-C", str(root), "add", "-f", "credential.key"], check=True)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with self.assertRaisesRegex(RuntimeError, "Publication policy"):
                publisher.publish(root, tree, "head", request)
            request.assert_not_called()

    def test_missing_scanner_prevents_every_network_request(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with patch.object(publisher.shutil, "which", return_value=None):
                with self.assertRaisesRegex(RuntimeError, "scanner missing"):
                    publisher.publish(root, tree, "head", request)
            request.assert_not_called()

    def test_deleted_historical_key_prevents_every_network_request(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            (root / "retired.key").write_bytes(b"\xff" * 64)
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "old key"], check=True)
            (root / "retired.key").unlink()
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with patch.object(publisher.shutil, "which", return_value="/scanner"):
                with self.assertRaisesRegex(RuntimeError, "Historical publication"):
                    publisher.publish(root, tree, "head", request)
            request.assert_not_called()

    def test_scanner_error_cannot_upload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with patch.object(publisher.shutil, "which", return_value="/scanner"), \
                 patch.object(publisher, "run", side_effect=subprocess.CalledProcessError(2, "scanner")):
                with self.assertRaises(subprocess.CalledProcessError):
                    publisher.publish(root, tree, "head", request)
            request.assert_not_called()

    def test_old_runtime_is_removed_only_from_disposable_candidate(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            source, candidate = base / "source", base / "candidate"
            source.mkdir()
            candidate.mkdir()
            fixture(candidate)
            (source / "go.mod").write_text("module example.invalid/test\n")
            runtime = source / "data/testnet"
            runtime.mkdir(parents=True)
            (runtime / "validator.key").write_bytes(b"\xff" * 64)
            (candidate / "data/testnet").mkdir(parents=True)
            (candidate / "data/testnet/validator.key").write_bytes(b"\xff" * 64)
            subprocess.run(["git", "-C", str(candidate), "add", "."], check=True)
            tree = publisher.prepare(source, candidate)
            self.assertEqual(inspect(candidate, tree)[1], [])
            self.assertTrue((runtime / "validator.key").exists())
            self.assertFalse((candidate / "data/testnet/validator.key").exists())

    def test_disguised_binary_and_hex_secret_are_refused(self):
        from publication_security import content_problem
        for contents in (b"\x7fELF" + b"A" * 70, b"a" * 128):
            self.assertIsNotNone(content_problem("documentation.txt", contents))


if __name__ == "__main__":
    unittest.main()