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