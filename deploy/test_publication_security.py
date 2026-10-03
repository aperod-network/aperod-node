import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "publication_security", Path(__file__).with_name("publication_security.py")
)
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


class PublicationSecurityTests(unittest.TestCase):
    def test_forbidden_paths(self):
        for path in (
            "data/testnet/validator.key", "anything/secret.key", "keys/a.pem",
            ".env", ".env.production", "data/testnet/chain.db/CURRENT",
            "snapshots/a.gz", "aperod", "node", "cli", "x.sqlite",
            "node_modules/a.js", "../key", "/outside", "back\\slash",
        ):
            with self.subTest(path=path):
                self.assertIsNotNone(policy.path_problem(path))

    def test_safe_source_and_documented_environment_templates(self):
        for path in ("core/verify.go", "deploy/install-node.sh", ".env.example",
                     ".github/workflows/security.yml", "data/favicon.png"):
            self.assertIsNone(policy.path_problem(path))

    def test_symlink_and_submodule_are_refused(self):
        for mode in ("120000", "160000"):
            self.assertIsNotNone(policy.path_problem("source.go", mode))

    def test_disguised_binaries_and_raw_keys(self):
        for content in (b"\x7fELFbinary", b"MZbinary",
                        b"\xff" * 32, b"\xff" * 64):
            self.assertIsNotNone(policy.content_problem("innocent.txt", content))

    def test_private_pem_is_refused_without_echoing_contents(self):
        self.assertIsNotNone(policy.content_problem(
            "config.txt", b"-----BEGIN " + b"PRIVATE KEY-----\nTEST_ONLY\n"
        ))

    def test_whole_tree_includes_preexisting_tracked_runtime(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
            (root / "data/testnet").mkdir(parents=True)
            (root / "data/testnet/validator.key").write_bytes(b"\xff" * 64)
            (root / "source.go").write_text("package source\n")
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)
            checked, failures = policy.inspect(root)
            self.assertEqual(checked, 2)
            self.assertEqual(failures[0][0], "data/testnet/validator.key")
            (root / "data/testnet/validator.key").unlink()
            checked, failures = policy.inspect(root, worktree=True)
            self.assertEqual((checked, failures), (1, []))


if __name__ == "__main__":
    unittest.main()