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
            ".local/source.txt", ".agents/memory.md", ".ssh/known_hosts",
            "artifacts/api-server/source.ts", "deploy/production_known_hosts",
            "p2p_whitelist.json", "backup.tgz", "operator.log",
            "deploy/sync-public-repo.sh",
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

    def test_standalone_encoded_key_material(self):
        import base64
        for size in (32, 64):
            self.assertIsNotNone(policy.content_problem("ordinary.txt", base64.b64encode(b"\xff" * size)))

    def test_history_still_refuses_deleted_key(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
            (root / "retired.key").write_bytes(b"\xff" * 64)
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "old"], check=True)
            (root / "retired.key").unlink()
            (root / "source.go").write_text("package source\n")
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "clean tip"], check=True)
            self.assertEqual(policy.inspect(root)[1], [])
            self.assertTrue(any(x[0] == "retired.key" for x in policy.inspect_history(root)[2]))

    def test_private_pem_is_refused_without_echoing_contents(self):
        self.assertIsNotNone(policy.content_problem(
            "config.txt", b"-----BEGIN " + b"PRIVATE KEY-----\nTEST_ONLY\n"
        ))

    def test_renamed_binary_is_refused_by_frozen_tree_scan(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
            (root / "aperod").write_bytes(b"\x7fELF" + b"safe-test-fixture")
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "binary fixture"], check=True)
            (root / "aperod").rename(root / "release-notes.txt")
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
            tree = subprocess.check_output(
                ["git", "-C", str(root), "write-tree"], text=True
            ).strip()
            checked, failures = policy.inspect(root, tree=tree)
            self.assertEqual(checked, 1)
            self.assertEqual(failures[0][0], "release-notes.txt")
            self.assertIn("disguised filename", failures[0][1])

    def test_private_repository_reference_is_detected_without_echoing_marker(self):
        marker = b"private-owner/" + b"private-source"
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
            (root / "README.md").write_bytes(
                b"Do not publish https://github.com/" + marker + b"\n"
            )
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)
            tree = subprocess.check_output(
                ["git", "-C", str(root), "write-tree"], text=True
            ).strip()
            failures = policy.inspect_private_references(root, tree, (marker,))
            self.assertEqual(failures, [("README.md", "private source repository reference")])
            self.assertNotIn(marker.decode(), repr(failures))

    def test_private_reference_scan_requires_a_marker(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            with self.assertRaisesRegex(ValueError, "identity unavailable"):
                policy.inspect_private_references(root, "HEAD", ())

    def test_missing_candidate_git_never_falls_back_to_parent(self):
        with tempfile.TemporaryDirectory() as temp:
            parent = Path(temp)
            subprocess.run(["git", "init", "-q", str(parent)], check=True)
            candidate = parent / "candidate"
            candidate.mkdir()
            with self.assertRaisesRegex(RuntimeError, "independent Git"):
                policy.git(candidate, "ls-files")

    def test_explicit_bare_repository_boundary_is_supported(self):
        with tempfile.TemporaryDirectory() as temp:
            bare = Path(temp) / "metadata.git"
            subprocess.run(["git", "init", "-q", "--bare", str(bare)], check=True)
            self.assertEqual(policy.git(bare, "rev-parse", "--is-bare-repository").strip(),
                             b"true")

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