import tempfile
import unittest
from pathlib import Path

import publish_public_repo as publisher


class MainnetDocsTests(unittest.TestCase):
    def test_legacy_pinned_installer_is_not_presented_as_mainnet(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "deploy").mkdir()
            doc = root / "deploy/README-public.md"
            doc.write_text(
                "### Step 2 — Install the validator node\n\n"
                "```bash\nsudo env APEROD_NETWORK=testnet "
                "bash deploy/install-validator.sh\n```\n\n"
                "### Step 3 — Register your node\n\nKeep this section.\n"
            )
            publisher.harden_public_installer_docs(root)
            result = doc.read_text()
            self.assertIn("MAINNET.md", result)
            self.assertNotIn("APEROD_NETWORK=testnet", result)
            self.assertIn("Keep this section.", result)
            self.assertIn("not yet published", result)
            publisher.harden_public_installer_docs(root)
            self.assertEqual(result, doc.read_text())

    def test_legacy_full_node_instructions_are_readiness_checks_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "deploy").mkdir()
            doc = root / "deploy/README-public.md"
            doc.write_text(
                "### Install a full node from the pinned signed source commit\n\n"
                "```bash\nbash deploy/install-node.sh\n```\n\n"
                "### Next section\n\n"
                "./build/aperod-node --config config/testnet.yaml\n"
            )
            publisher.harden_public_installer_docs(root)
            result = doc.read_text()
            self.assertIn("--validate-config", result)
            self.assertNotIn("bash deploy/install-node.sh", result)
            self.assertNotIn("config/testnet.yaml", result)
            self.assertIn("### Next section", result)


if __name__ == "__main__":
    unittest.main()
