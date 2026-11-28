import tempfile
import unittest
from pathlib import Path

import publish_public_repo as publisher


class MainnetDocsTests(unittest.TestCase):
    def test_maintenance_uses_verified_mainnet_paths(self):
        maintenance = Path(__file__).with_name("MAINTENANCE.md").read_text()
        self.assertNotIn("/data/testnet", maintenance)
        self.assertNotIn("aperod-node-sched-restart.timer", maintenance)
        self.assertIn("MAINNET_DATA_DIR:?", maintenance)
        self.assertIn("verified, closed", maintenance)

    def test_mainnet_onboarding_does_not_mistake_a_signed_checkpoint_for_an_installer(self):
        mainnet = Path(__file__).with_name("MAINNET.md").read_text()
        readme = Path(__file__).with_name("README-public.md").read_text()
        self.assertIn("48f2668a598b9fe9111bed594ce7fcf646bdb15385eac91542188dd5a82aea70", mainnet)
        self.assertIn("056476ad6cf64f8c47a7ec0f0033128cc7759d2bdfd699a319d44d6146a94654", mainnet)
        self.assertIn("operator_1_of_1", mainnet)
        self.assertIn("on-chain stake locking", mainnet)
        self.assertRegex(mainnet, r"no\s+supported command")
        self.assertRegex(readme, r"No\s+OS/architecture is currently verified")
        self.assertNotIn("Supported platforms: **Ubuntu 22.04", readme)

    def test_mainnet_docs_record_live_api_genesis_and_ancestry_mismatch(self):
        mainnet = Path(__file__).with_name("MAINNET.md").read_text()
        verifier = Path(__file__).with_name("verify_public_mainnet.py").read_text()
        self.assertIn("c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f", mainnet)
        self.assertIn("da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1", mainnet)
        self.assertIn("ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb", mainnet)
        self.assertIn("61457b6a59a321588cd501365ab4d3bbbe4ab9ed7e03c326cdeda86adcb28809", mainnet)
        self.assertIn("1782932857", mainnet)
        self.assertIn("4,076,315", mainnet)
        self.assertIn("8bf6b47c87e075007527993d3ea1c8f45424c1f382cb43eab8ab7f5c19cb650f", mainnet)
        attestation = Path(__file__).resolve().parents[1] / "mainnetbootstrap" / "attestation.go"
        attestation_source = attestation.read_text()
        self.assertIn("dedicated off-chain 1-of-1 operator key", attestation_source)
        self.assertIn(
            'CanonicalMainnetGenesisHeaderHashHex = "c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f"',
            attestation_source,
        )
        self.assertIn(
            'CanonicalMainnetGenesisIndexHashHex  = "da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1"',
            attestation_source,
        )
        self.assertIn("operator_1_of_1", mainnet)
        self.assertIn("not validator-committee quorum evidence", mainnet.replace("\n", " "))
        self.assertIn("python3 deploy/verify_public_mainnet.py", mainnet)
        self.assertIn("operator-signed bootstrap statement", mainnet)
        self.assertIn("AVM checkpoint signature", mainnet)
        self.assertIn("height-scoped replacement signing key", " ".join(mainnet.split()))
        self.assertIn("persisted node key", " ".join(mainnet.split()))
        self.assertIn("/ip4/127.0.0.1/tcp/0", mainnet)
        normalized_mainnet = " ".join(mainnet.split())
        plain_mainnet = " ".join(mainnet.replace("`", "").split())
        self.assertIn("checkpoint block signature does not verify", plain_mainnet)
        self.assertIn("`17,563` signatures under replacement signer `eb9650…`", normalized_mainnet)
        self.assertIn("removing it from historical verification would make this chain fail validation", normalized_mainnet)
        self.assertIn("discrepancy remains unresolved", normalized_mainnet)
        self.assertIn("finality quorum as unverified", normalized_mainnet)
        self.assertIn("historical_committee_authenticated=false", mainnet)
        self.assertIn("798,755", mainnet)
        self.assertIn("2 minutes 22 seconds", normalized_mainnet)
        self.assertIn("6 minutes 21 seconds", normalized_mainnet)
        self.assertIn("4,094,720", mainnet)
        self.assertIn("4,094,730", mainnet)
        self.assertIn("4,095,578", mainnet)
        self.assertIn("4,095,577", mainnet)
        self.assertIn(
            "3e6579833d018e732a38bfe87f53fd80ff6a360dcc5f4540187b785a4fe23756",
            mainnet,
        )
        self.assertIn("Cloudflare reported `DYNAMIC`", mainnet)
        self.assertIn("active bundle lacks the", mainnet)
        self.assertIn("4,095,914", mainnet)
        self.assertIn("4,095,953", mainnet)
        self.assertIn("needed to catch up by 39 blocks", " ".join(mainnet.split()))
        self.assertIn("3/3 checks passed", mainnet)
        self.assertIn("4,096,227", mainnet)
        self.assertIn("4,096,270", mainnet)
        self.assertIn("does not verify burn totals", " ".join(mainnet.split()))
        self.assertIn("43 blocks behind the node", Path(__file__).with_name("VALIDATORS.md").read_text())
        self.assertIn("No full API service was installed or started on OVH", mainnet)
        self.assertIn("31 targeted route tests", Path(__file__).with_name("VALIDATORS.md").read_text())
        self.assertIn("14,064 regular files", mainnet)
        self.assertIn("staging duplicate was removed", " ".join(mainnet.split()))
        self.assertIn("candidate build plus API tests passed on OVH", normalized_mainnet)
        self.assertIn("restored binary still returns HTTP 503", normalized_mainnet)
        self.assertIn("both known stale block hashes returned 404", normalized_mainnet)
        self.assertIn("No public API/Explorer", mainnet)
        self.assertIn("old-primary cutover has occurred", mainnet)
        self.assertIn("/var/lib/aperod/mainnet", mainnet)
        self.assertIn("aperod_mainnet_api_20261124", mainnet)
        self.assertIn("4,099,675", mainnet)
        self.assertIn("924,790–1,723,544", mainnet)
        self.assertIn("aperod-explorer-indexer.service` is active", mainnet)
        self.assertIn("enabled for boot", mainnet)
        self.assertIn(
            "Do not copy an older `node.yaml` from another server",
            " ".join(mainnet.split()),
        )
        self.assertIn("evidence-verify", mainnet)
        deploy = Path(__file__).resolve().parent
        self.assertTrue((deploy / "mainnet-bootstrap-4076315.json").is_file())
        self.assertTrue((deploy / "mainnet-avm-checkpoint-4076315.json").is_file())
        genesis = Path(__file__).resolve().parents[1] / "config" / "genesis-mainnet.yaml"
        self.assertTrue(genesis.is_file())
        self.assertNotIn("testnet", mainnet.lower())
        self.assertNotIn("testnet", Path(__file__).with_name("VALIDATORS.md").read_text().lower())
        self.assertNotIn("testnet", Path(__file__).with_name("README-public.md").read_text().lower())
        self.assertIn("STALE_FIRST", verifier)
        self.assertIn("SNAPSHOT_HEIGHT", verifier)

    def test_public_validator_guide_does_not_claim_unverified_registration(self):
        readme = Path(__file__).with_name("README-public.md").read_text()
        self.assertRegex(readme, r"(?i)A verified self-service mainnet bootstrap release is not yet published\.")
        self.assertRegex(readme, r"The current `install-validator\.sh` must not be used as a Mainnet installer\.")
        self.assertIn("have been removed", readme)
        self.assertIn("only creates an", readme)
        self.assertIn("off-chain application record", readme)
        self.assertNotIn("curl -s -X POST https://aperod.com/api/validators/apply", readme)
        self.assertNotIn("Your node enters the active set automatically", readme)
        self.assertRegex(readme, r"Do not\s+send APRO or expose a consensus key")
        self.assertNotIn("No approval needed", readme)
        self.assertNotIn("1,501,714 APRO / year", readme)

    def test_validator_guide_distinguishes_peer_probe_from_validator_admission(self):
        validators = Path(__file__).with_name("VALIDATORS.md").read_text()
        self.assertIn("A verified self-service mainnet bootstrap release is not yet published.", validators)
        self.assertIn("single-operator trust policy", validators)
        self.assertIn("not a validator quorum", validators)
        self.assertIn("applied live blocks through height `4,083,185`", validators)
        self.assertIn("after restart", validators)
        self.assertIn("4,095,578", validators)
        self.assertIn("4,095,577", validators)
        self.assertIn(
            "3e6579833d018e732a38bfe87f53fd80ff6a360dcc5f4540187b785a4fe23756",
            validators,
        )
        self.assertIn("**not** a clean install", validators)
        self.assertIn("public installer", validators)
        self.assertIn("HTTP 503", validators)
        self.assertIn("committee finality as unverified", " ".join(validators.split()))
        self.assertIn("candidate binary built on OVH", " ".join(validators.split()))
        self.assertIn("previous sync-only binary restored", " ".join(validators.split()))
        self.assertRegex(validators, r"active-set\s+admission")
        self.assertIn("/var/lib/aperod/mainnet", validators)
        self.assertIn("single current OVH tree", validators)
        self.assertIn("P2P listener is loopback-only", validators)
        self.assertIn("historical height-index completeness is not established", validators)

    def test_investor_quick_start_does_not_link_an_unverified_installer(self):
        investors = Path(__file__).with_name("INVESTORS.md").read_text()
        self.assertIn("There is no verified public Mainnet validator installation command yet.", investors)
        self.assertNotIn("[install-validator.sh]", investors)

    def test_testnet_installer_does_not_advertise_mainnet_activation(self):
        installer = Path(__file__).with_name("install-validator.sh").read_text()
        self.assertIn("This installer is for testnet only", installer)
        self.assertIn("does not join a server to mainnet", installer)
        self.assertIn("off-chain record", installer)
        self.assertNotIn("The node joins the active validator set automatically", installer)
        self.assertNotIn("Transfer at least ${BOLD}100,000 APRO", installer)
        self.assertNotIn("Double-sign penalty: 10%", installer)

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

    def test_public_root_readme_removes_legacy_install_and_admission(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            readme = root / "README.md"
            readme.write_text(
                "# Aperod\n\n## ⬇ Install a Full Node\n\n"
                "set -euo pipefail\nbash deploy/install-node.sh\n\n"
                "## 🛡 Become a Validator\n\n"
                "bash deploy/install-validator.sh\n"
                "curl -s -X POST https://aperod.com/api/validators/apply\n"
                "Your node enters the active set automatically\n\n"
                "## 🔨 Building from Source\n\n"
                "# Start a full node\n./build/aperod-node --config config/testnet.yaml\n"
            )
            publisher.harden_public_installer_docs(root)
            result = readme.read_text()
            self.assertIn("](deploy/MAINNET.md)", result)
            self.assertIn("## 🔨 Building from Source", result)
            self.assertIn("--validate-config", result)
            for retired in ("bash deploy/install-node.sh", "bash deploy/install-validator.sh",
                            "/api/validators/apply", "enters the active set automatically",
                            "--config config/testnet.yaml"):
                self.assertNotIn(retired, result)
            publisher.harden_public_installer_docs(root)
            self.assertEqual(result, readme.read_text())

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
