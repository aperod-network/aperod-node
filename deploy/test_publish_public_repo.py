import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

import publish_public_repo as publisher
from publication_security import git, inspect

TEST_PRIVATE_REPO = "test-private-owner/" + "source-monorepo"
TEST_PRIVATE_REPO_MARKERS = (TEST_PRIVATE_REPO.encode(),)


def fixture(root):
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.name", "Test"], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.email", "test@example.invalid"], check=True)
    (root / "go.mod").write_text("module example.invalid/test\n\ngo 1.25.0\n")
    subprocess.run(["git", "-C", str(root), "add", "."], check=True)
    subprocess.run(["git", "-C", str(root), "commit", "-qm", "fixture"], check=True)


class PublisherTests(unittest.TestCase):
    def test_drift_report_identifies_both_sides_and_content_changes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            (root / "changed.go").write_text("package source\n")
            (root / "removed.go").write_text("package source\n")
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "source tree"], check=True)
            (root / "added.go").write_text("package source\n")
            (root / "changed.go").write_text("package source\n// updated\n")
            (root / "removed.go").unlink()
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)

            report = publisher.drift_report(root)

        self.assertEqual(len(report), 4)
        self.assertIn("[WARN] MISSING in public repo: added.go", report)
        self.assertIn("[WARN] DIFFERS: changed.go", report)
        self.assertIn("[WARN] ONLY in public repo: removed.go", report)
        self.assertEqual(report[-1], "Complete source-distribution drift: 3 files.")

    def test_publication_uses_only_aperod_network_as_author(self):
        self.assertEqual(
            publisher.AUTHOR,
            {"name": "aperod-network", "email": "aperod-network@users.noreply.github.com"},
        )
        self.assertEqual(publisher.MESSAGE, "chore: maintain verified source distribution")
        self.assertNotIn("Co-authored-by:", publisher.MESSAGE)
        self.assertNotIn("Co-authored-by:", publisher.MERGE_MESSAGE)

    def test_private_repository_identity_is_read_at_runtime(self):
        with patch.dict(publisher.os.environ, {
            "GITHUB_REPOSITORY": TEST_PRIVATE_REPO,
        }):
            markers = publisher.private_repository_markers(Path("."))
        self.assertIn(TEST_PRIVATE_REPO.encode(), markers)
        self.assertNotIn(b"aperod-network/aperod-node", markers)

    def test_live_branch_policy_requires_trusted_checks_and_admin_enforcement(self):
        calls = []

        def request(method, url, data=None):
            calls.append((method, url))
            if url == "https://api.github.com/user":
                return {"login": "aperod-network"}
            if url == publisher.API:
                return {
                    "id": publisher.PUBLIC_REPO_ID, "private": False,
                    "fork": False, "default_branch": "main",
                }
            if url.endswith("/branches/main/protection"):
                return {
                    "required_status_checks": {
                        "strict": True,
                        "checks": [
                            {"context": name, "app_id": app_id}
                            for name, app_id in publisher.BRANCH_REQUIRED_CHECKS
                        ],
                    },
                    "enforce_admins": {"enabled": True},
                    "allow_force_pushes": {"enabled": False},
                    "allow_deletions": {"enabled": False},
                }
            if "/rulesets?" in url:
                return []
            raise AssertionError(f"unexpected request: {method} {url}")

        publisher.verify_public_repo_policy(request)
        self.assertEqual(len(calls), 4)

    def test_live_branch_policy_fails_closed_if_admins_can_bypass(self):
        def request(method, url, data=None):
            if url == "https://api.github.com/user":
                return {"login": "aperod-network"}
            if url == publisher.API:
                return {
                    "id": publisher.PUBLIC_REPO_ID, "private": False,
                    "fork": False, "default_branch": "main",
                }
            if url.endswith("/branches/main/protection"):
                return {
                    "required_status_checks": {
                        "strict": True,
                        "checks": [
                            {"context": name, "app_id": app_id}
                            for name, app_id in publisher.BRANCH_REQUIRED_CHECKS
                        ],
                    },
                    "enforce_admins": {"enabled": False},
                    "allow_force_pushes": {"enabled": False},
                    "allow_deletions": {"enabled": False},
                }
            raise AssertionError(f"unexpected request: {method} {url}")

        with self.assertRaisesRegex(RuntimeError, "trusted policy"):
            publisher.verify_public_repo_policy(request)

    def test_live_branch_policy_rejects_other_publisher_identity(self):
        def request(method, url, data=None):
            if url == "https://api.github.com/user":
                return {"login": "another-account"}
            raise AssertionError(f"unexpected request after identity mismatch: {url}")

        with self.assertRaisesRegex(RuntimeError, "not authenticated as aperod-network"):
            publisher.verify_public_repo_policy(request)

    def test_live_branch_policy_rejects_unreviewed_rulesets(self):
        def request(method, url, data=None):
            if url == "https://api.github.com/user":
                return {"login": "aperod-network"}
            if url == publisher.API:
                return {
                    "id": publisher.PUBLIC_REPO_ID, "private": False,
                    "fork": False, "default_branch": "main",
                }
            if url.endswith("/branches/main/protection"):
                return {
                    "required_status_checks": {
                        "strict": True,
                        "checks": [
                            {"context": name, "app_id": app_id}
                            for name, app_id in publisher.BRANCH_REQUIRED_CHECKS
                        ],
                    },
                    "enforce_admins": {"enabled": True},
                    "allow_force_pushes": {"enabled": False},
                    "allow_deletions": {"enabled": False},
                }
            if "/rulesets?" in url:
                return [{"id": 9, "enforcement": "active"}]
            raise AssertionError(f"unexpected request: {method} {url}")

        with self.assertRaisesRegex(RuntimeError, "rulesets"):
            publisher.verify_public_repo_policy(request)

    def test_publish_uses_scanned_git_data_api_and_explicit_single_author(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            base = git(root, "rev-parse", "HEAD").decode().strip()
            (root / "source.go").write_text("package source\n")
            subprocess.run(["git", "-C", str(root), "add", "source.go"], check=True)
            tree = git(root, "write-tree").decode().strip()
            blob_row = git(root, "ls-tree", "-r", tree, "--", "source.go")
            blob_sha = blob_row.decode().split()[2]
            base_tree = git(root, "rev-parse", "HEAD^{tree}").decode().strip()
            events = []
            payloads = {}

            def request(method, url, data=None):
                events.append((method, url))
                if method == "GET" and url.endswith("/git/ref/heads/main"):
                    return {"object": {"sha": base}}
                if method == "GET" and url == "https://api.github.com/user":
                    return {"login": "aperod-network"}
                if method == "GET" and url == publisher.API:
                    return {
                        "id": publisher.PUBLIC_REPO_ID, "private": False,
                        "fork": False, "default_branch": "main",
                    }
                if method == "GET" and url.endswith("/branches/main/protection"):
                    return {
                        "required_status_checks": {
                            "strict": True,
                            "checks": [
                                {"context": name, "app_id": app_id}
                                for name, app_id in publisher.BRANCH_REQUIRED_CHECKS
                            ],
                        },
                        "enforce_admins": {"enabled": True},
                        "allow_force_pushes": {"enabled": False},
                        "allow_deletions": {"enabled": False},
                    }
                if method == "GET" and "/rulesets?" in url:
                    return []
                if method == "GET" and url.endswith("/git/commits/" + base):
                    return {"tree": {"sha": base_tree}}
                if method == "POST" and url.endswith("/git/blobs"):
                    return {"sha": blob_sha}
                if method == "POST" and url.endswith("/git/trees"):
                    return {"sha": tree}
                if method == "POST" and url.endswith("/git/commits"):
                    payloads["commit"] = data
                    return {"sha": "c" * 40}
                if method == "POST" and url.endswith("/git/refs"):
                    return {}
                if method == "POST" and url.endswith("/pulls"):
                    payloads["pull"] = data
                    return {"number": 1, "html_url": "https://github.com/aperod-network/aperod-node/pull/1"}
                raise AssertionError(f"unexpected request: {method} {url}")

            def verified(*args, **kwargs):
                events.append(("VERIFY", "whole frozen candidate"))

            with patch.object(publisher, "verify", side_effect=verified):
                pr = publisher.publish(
                    root, tree, base, request,
                    private_repo_markers=TEST_PRIVATE_REPO_MARKERS,
                    commit_message="fix: verify source cleanup failures",
                    pr_title="Fix unchecked cleanup failures in the node source",
                    pr_body="The verified source tree checks cleanup errors that previously failed CI.",
                )

            self.assertEqual(events[0][0], "VERIFY")
            self.assertEqual(payloads["commit"]["author"], publisher.AUTHOR)
            self.assertEqual(payloads["commit"]["committer"], publisher.AUTHOR)
            self.assertEqual(payloads["commit"]["message"], "fix: verify source cleanup failures")
            self.assertEqual(
                payloads["pull"],
                {
                    "title": "Fix unchecked cleanup failures in the node source",
                    "head": "verified-publication/" + "c" * 16,
                    "base": "main",
                    "body": "The verified source tree checks cleanup errors that previously failed CI.",
                },
            )
            self.assertNotIn("Co-authored-by:", payloads["commit"]["message"])
            self.assertEqual(pr["_verified_base_sha"], base)
            self.assertFalse(any("/contents/" in url for _, url in events))
            self.assertIn(("POST", f"{publisher.API}/git/blobs"), events)
            self.assertTrue(all(method != "POST" or "/contents/" not in url
                                for method, url in events))

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
                                              "SSH_PASSWORD": "TEST_ONLY",
                                              "PYTHONPATH": "/untrusted/module/path"}), \
             patch.object(publisher.subprocess, "run") as child:
            publisher.run("go", "vet", "./...")
        env = child.call_args.kwargs["env"]
        self.assertNotIn("PUBLIC_GITHUB_TOKEN", env)
        self.assertNotIn("SSH_PASSWORD", env)
        self.assertNotIn("PYTHONPATH", env)

    def test_go_tests_receive_only_the_pyyaml_userbase(self):
        python_userbase = Path("/trusted/python-userbase")
        with patch.dict(publisher.os.environ, {"PUBLIC_GITHUB_TOKEN": "TEST_ONLY",
                                               "SSH_PASSWORD": "TEST_ONLY",
                                               "PYTHONPATH": "/untrusted/module/path"}), \
             patch.object(publisher.subprocess, "run") as child:
            publisher.run("go", "test", "./...",
                          test_python_userbase=python_userbase)
        env = child.call_args.kwargs["env"]
        self.assertEqual(env["PYTHONUSERBASE"], str(python_userbase))
        self.assertNotIn("PYTHONPATH", env)
        self.assertNotIn("PUBLIC_GITHUB_TOKEN", env)
        self.assertNotIn("SSH_PASSWORD", env)

    def test_test_python_userbase_copies_only_trusted_pyyaml(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp) / "installed"
            package = base / "yaml"
            package.mkdir(parents=True)
            (package / "__init__.py").write_text("SOURCE = 'installed'\n")
            (package / "scanner.py").write_text("SAFE = True\n")
            userbase = Path(temp) / "isolated"
            fake_distribution = Mock()
            fake_distribution.locate_file.side_effect = (
                lambda rel: base if str(rel) == "" else package
            )
            with patch.object(publisher, "distribution",
                              return_value=fake_distribution):
                publisher.prepare_test_python_userbase(userbase)
            target = (
                userbase / "lib"
                / f"python{publisher.sys.version_info.major}.{publisher.sys.version_info.minor}"
                / "site-packages" / "yaml"
            )
            self.assertEqual((target / "__init__.py").read_text(),
                             "SOURCE = 'installed'\n")
            self.assertTrue((target / "scanner.py").is_file())

    def test_failed_required_check_never_merges(self):
        methods = []
        def request(method, url, data=None):
            methods.append(method)
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            return {"check_runs": [{"name": "Whole-tree publication security",
                                   "status": "completed", "conclusion": "failure",
                                   "app": {"id": 15368}}]}
        with self.assertRaisesRegex(RuntimeError, "check failed"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request, expected_head_sha="reviewed")
        self.assertNotIn("PUT", methods)

    def test_failed_lifecycle_matrix_check_never_merges(self):
        methods = []
        failed = "install-node.sh e2e smoke test (update)"

        def request(method, url, data=None):
            methods.append(method)
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            return {"check_runs": [
                {"name": name, "status": "completed",
                 "conclusion": "failure" if name == failed else "success",
                 "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                     name, publisher.GITHUB_ACTIONS_APP_ID)}}
                for name in publisher.REQUIRED_CHECKS
            ]}

        with self.assertRaisesRegex(RuntimeError, "check failed"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request, expected_head_sha="reviewed")
        self.assertNotIn("PUT", methods)

    def test_failed_codeql_check_never_merges(self):
        methods = []
        failed = "CodeQL"

        def request(method, url, data=None):
            methods.append(method)
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            return {"check_runs": [
                {"name": name, "status": "completed",
                 "conclusion": "failure" if name == failed else "success",
                 "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                     name, publisher.GITHUB_ACTIONS_APP_ID)}}
                for name in publisher.REQUIRED_CHECKS
            ]}

        with self.assertRaisesRegex(RuntimeError, "check failed"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request, expected_head_sha="reviewed")
        self.assertNotIn("PUT", methods)

    def test_neutral_codeql_requires_completed_go_analysis_and_no_open_pr_alerts(self):
        methods = []

        def request(method, url, data=None):
            methods.append((method, url))
            if method == "PUT":
                return {"merged": True}
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            if url.endswith("/code-scanning/alerts?pr=1&tool_name=CodeQL&state=open&per_page=100"):
                return []
            checks = []
            for name in publisher.REQUIRED_CHECKS:
                neutral = name == "CodeQL"
                checks.append({
                    "name": name,
                    "status": "completed",
                    "conclusion": "neutral" if neutral else "success",
                    "output": {
                        "title": publisher.CODEQL_NEUTRAL_ALLOWED_TITLE,
                        "summary": "Default setup: /language:go",
                    } if neutral else {},
                    "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                        name, publisher.GITHUB_ACTIONS_APP_ID)},
                })
            return {"check_runs": checks}

        with patch.object(publisher.time, "sleep"), \
                patch.object(publisher, "verify_public_repo_policy"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request, expected_head_sha="reviewed")
        self.assertIn(
            ("GET", f"{publisher.API}/code-scanning/alerts?pr=1&tool_name=CodeQL&state=open&per_page=100"),
            methods,
        )
        self.assertIn(("PUT", f"{publisher.API}/pulls/1/merge"), methods)

    def test_neutral_codeql_waits_for_go_analysis_before_evaluating(self):
        methods = []
        check_reads = 0

        def request(method, url, data=None):
            nonlocal check_reads
            methods.append((method, url))
            if method == "PUT":
                return {"merged": True}
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            check_reads += 1
            checks = []
            for name in publisher.REQUIRED_CHECKS:
                neutral = name == "CodeQL" and check_reads == 1
                pending_go = name == "Analyze (go)" and check_reads == 1
                checks.append({
                    "name": name,
                    "status": "in_progress" if pending_go else "completed",
                    "conclusion": None if pending_go else (
                        "neutral" if neutral else "success"
                    ),
                    "output": {
                        "title": publisher.CODEQL_NEUTRAL_ALLOWED_TITLE,
                        "summary": "Default setup: /language:go",
                    } if neutral else {},
                    "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                        name, publisher.GITHUB_ACTIONS_APP_ID)},
                })
            return {"check_runs": checks}

        with patch.object(publisher.time, "sleep"), \
                patch.object(publisher, "verify_public_repo_policy"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request, expected_head_sha="reviewed")
        self.assertEqual(check_reads, 2)
        self.assertFalse(any("/code-scanning/alerts?" in url for _, url in methods))
        self.assertIn(("PUT", f"{publisher.API}/pulls/1/merge"), methods)

    def test_neutral_codeql_with_open_alerts_never_merges(self):
        methods = []

        def request(method, url, data=None):
            methods.append((method, url))
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            if url.endswith("/code-scanning/alerts?pr=1&tool_name=CodeQL&state=open&per_page=100"):
                return [{"number": 7, "state": "open"}]
            checks = []
            for name in publisher.REQUIRED_CHECKS:
                neutral = name == "CodeQL"
                checks.append({
                    "name": name,
                    "status": "completed",
                    "conclusion": "neutral" if neutral else "success",
                    "output": {
                        "title": publisher.CODEQL_NEUTRAL_ALLOWED_TITLE,
                        "summary": "Default setup: /language:go",
                    } if neutral else {},
                    "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                        name, publisher.GITHUB_ACTIONS_APP_ID)},
                })
            return {"check_runs": checks}

        with self.assertRaisesRegex(RuntimeError, "open pull-request alerts"):
            publisher.await_merge({"number": 1, "html_url": "test"}, request, expected_head_sha="reviewed")
        self.assertNotIn(("PUT", f"{publisher.API}/pulls/1/merge"), methods)

    def test_untrusted_status_provider_cannot_authorize_merge(self):
        methods = []
        def request(method, url, data=None):
            methods.append(method)
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            return {"check_runs": [
                {"name": name, "status": "completed", "conclusion": "success",
                 "app": {"id": publisher.GITHUB_ACTIONS_APP_ID if name == "CodeQL"
                         else publisher.TRUSTED_CHECK_APP_IDS.get(
                             name, publisher.GITHUB_ACTIONS_APP_ID)}}
                for name in publisher.REQUIRED_CHECKS
            ]}
        with patch.object(publisher.time, "monotonic", side_effect=[0, 0, 2]), \
             patch.object(publisher.time, "sleep"):
            with self.assertRaisesRegex(RuntimeError, "did not complete"):
                publisher.await_merge(
                    {"number": 1, "html_url": "test"}, request,
                    timeout=1, expected_head_sha="reviewed",
                )
        self.assertNotIn("PUT", methods)

    def test_policy_change_after_checks_prevents_merge(self):
        methods = []

        def request(method, url, data=None):
            methods.append(method)
            if method == "PUT":
                return {"merged": True}
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            return {"check_runs": [
                {"name": name, "status": "completed", "conclusion": "success",
                 "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                     name, publisher.GITHUB_ACTIONS_APP_ID)}}
                for name in publisher.REQUIRED_CHECKS
            ]}

        with patch.object(publisher, "verify_public_repo_policy",
                          side_effect=RuntimeError("protection changed")):
            with self.assertRaisesRegex(RuntimeError, "protection changed"):
                publisher.await_merge(
                    {"number": 1, "html_url": "test"}, request,
                    expected_head_sha="reviewed",
                )
        self.assertNotIn("PUT", methods)

    def test_merge_is_bound_to_reviewed_head(self):
        bodies = []
        def request(method, url, data=None):
            if method == "PUT":
                bodies.append(data)
                return {"merged": True}
            if url.endswith("/pulls/1"):
                return {"state": "open", "head": {"sha": "reviewed"}, "base": {"ref": "main"}}
            return {"check_runs": [{"name": name, "status": "completed",
                                   "conclusion": "success",
                                   "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                                       name, publisher.GITHUB_ACTIONS_APP_ID)}}
                                   for name in publisher.REQUIRED_CHECKS]}
        with patch.object(publisher, "verify_public_repo_policy"):
            publisher.await_merge(
                {"number": 1, "html_url": "test"}, request,
                expected_head_sha="reviewed",
            )
        self.assertEqual(bodies[0]["sha"], "reviewed")
        self.assertNotIn("force", bodies[0])

    def test_await_merge_waits_for_uploaded_head_before_reading_checks(self):
        heads = ["stale", "reviewed", "reviewed"]
        pull_reads = 0
        events = []

        def request(method, url, data=None):
            nonlocal pull_reads
            if method == "PUT":
                events.append(("merge", pull_reads))
                return {"merged": True}
            if url.endswith("/pulls/1"):
                pull_reads += 1
                return {
                    "state": "open",
                    "head": {"sha": heads.pop(0)},
                    "base": {"ref": "main"},
                }
            if "/check-runs?" in url:
                events.append(("checks", pull_reads))
                return {"check_runs": [
                    {"name": name, "status": "completed", "conclusion": "success",
                     "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                         name, publisher.GITHUB_ACTIONS_APP_ID)}}
                    for name in publisher.REQUIRED_CHECKS
                ]}
            raise AssertionError(f"unexpected request: {method} {url}")

        with patch.object(publisher.time, "sleep"), \
                patch.object(publisher, "verify_public_repo_policy"):
            publisher.await_merge(
                {"number": 1, "html_url": "test"}, request,
                expected_head_sha="reviewed",
            )
        self.assertEqual(events[0], ("checks", 2))
        self.assertEqual(events[1], ("merge", 3))

    def test_await_merge_refuses_head_changed_after_checks(self):
        heads = ["reviewed", "changed"]
        methods = []

        def request(method, url, data=None):
            methods.append((method, url))
            if method == "PUT":
                return {"merged": True}
            if url.endswith("/pulls/1"):
                return {
                    "state": "open",
                    "head": {"sha": heads.pop(0)},
                    "base": {"ref": "main"},
                }
            return {"check_runs": [
                {"name": name, "status": "completed", "conclusion": "success",
                 "app": {"id": publisher.TRUSTED_CHECK_APP_IDS.get(
                     name, publisher.GITHUB_ACTIONS_APP_ID)}}
                for name in publisher.REQUIRED_CHECKS
            ]}

        with self.assertRaisesRegex(RuntimeError, "head or base changed"):
            publisher.await_merge(
                {"number": 1, "html_url": "test"}, request,
                expected_head_sha="reviewed",
            )
        self.assertFalse(any(method == "PUT" for method, _ in methods))

    def test_await_merge_refuses_non_main_base(self):
        methods = []

        def request(method, url, data=None):
            methods.append(method)
            return {
                "state": "open",
                "head": {"sha": "reviewed"},
                "base": {"ref": "release"},
            }

        with self.assertRaisesRegex(RuntimeError, "base is not main"):
            publisher.await_merge(
                {"number": 1, "html_url": "test"}, request,
                expected_head_sha="reviewed",
            )
        self.assertNotIn("PUT", methods)

    def test_policy_failure_prevents_every_network_request(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            (root / "credential.key").write_bytes(b"\xff" * 64)
            subprocess.run(["git", "-C", str(root), "add", "-f", "credential.key"], check=True)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with self.assertRaisesRegex(RuntimeError, "Publication policy"):
                publisher.publish(
                    root, tree, "head", request,
                    private_repo_markers=TEST_PRIVATE_REPO_MARKERS,
                )
            request.assert_not_called()

    def test_missing_scanner_prevents_every_network_request(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with patch.object(publisher.shutil, "which", return_value=None):
                with self.assertRaisesRegex(RuntimeError, "scanner missing"):
                    publisher.publish(
                        root, tree, "head", request,
                        private_repo_markers=TEST_PRIVATE_REPO_MARKERS,
                    )
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
                    publisher.publish(
                        root, tree, "head", request,
                        private_repo_markers=TEST_PRIVATE_REPO_MARKERS,
                    )
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
                    publisher.publish(
                        root, tree, "head", request,
                        private_repo_markers=TEST_PRIVATE_REPO_MARKERS,
                    )
            request.assert_not_called()

    def test_secret_in_unchanged_file_is_scanned_before_any_upload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            (root / "unchanged.md").write_text("TEST_SECRET_UNCHANGED_VALUE\n")
            subprocess.run(["git", "-C", str(root), "add", "unchanged.md"], check=True)
            subprocess.run(["git", "-C", str(root), "commit", "-qm", "unchanged fixture"], check=True)
            (root / "new.go").write_text("package source\n")
            subprocess.run(["git", "-C", str(root), "add", "new.go"], check=True)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()

            def reject_secret_scan(*args, **kwargs):
                if args[:2] == ("gitleaks", "git"):
                    paths = git(root, "ls-tree", "-r", "--name-only", "HEAD").decode()
                    self.assertIn("unchanged.md", paths)
                    self.assertIn("--redact=100", args)
                    raise subprocess.CalledProcessError(1, "gitleaks")
                raise AssertionError(f"unexpected verification command: {args[:2]}")

            with patch.object(publisher.shutil, "which", return_value="/scanner"), \
                    patch.object(publisher, "run", side_effect=reject_secret_scan):
                with self.assertRaises(subprocess.CalledProcessError) as error:
                    publisher.publish(
                        root, tree, "head", request,
                        private_repo_markers=TEST_PRIVATE_REPO_MARKERS,
                    )
            self.assertNotIn("TEST_SECRET_UNCHANGED_VALUE", str(error.exception))
            request.assert_not_called()

    def test_private_repository_reference_blocks_before_any_upload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture(root)
            marker = TEST_PRIVATE_REPO.encode()
            (root / "README.md").write_bytes(
                b"Private issue tracker: https://github.com/" + marker + b"\n"
            )
            subprocess.run(["git", "-C", str(root), "add", "README.md"], check=True)
            tree = git(root, "write-tree").decode().strip()
            request = Mock()
            with self.assertRaisesRegex(RuntimeError, "private repository references") as error:
                publisher.publish(root, tree, "head", request,
                                  private_repo_markers=(marker,))
            self.assertNotIn(marker.decode(), str(error.exception))
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

    def test_extensionless_build_files_are_included_in_public_snapshot(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            source, candidate = base / "source", base / "candidate"
            source.mkdir()
            candidate.mkdir()
            fixture(candidate)
            (source / "go.mod").write_text("module example.invalid/test\n")
            dockerfile = source / "deploy/fixtures/node-lifecycle/Dockerfile"
            dockerfile.parent.mkdir(parents=True)
            dockerfile.write_text("FROM ubuntu:24.04\nCOPY tool /usr/local/libexec/tool\n")
            (source / "Makefile").write_text("build:\n\tgo build ./...\n")

            tree = publisher.prepare(source, candidate)
            paths = git(candidate, "ls-tree", "-r", "--name-only", tree).decode().splitlines()

            self.assertIn("deploy/fixtures/node-lifecycle/Dockerfile", paths)
            self.assertIn("Makefile", paths)
            self.assertEqual(
                (candidate / "deploy/fixtures/node-lifecycle/Dockerfile").read_text(),
                dockerfile.read_text(),
            )
            self.assertEqual((candidate / "Makefile").read_text(),
                             (source / "Makefile").read_text())

    def test_private_sync_wrapper_is_not_in_public_candidate(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            source, candidate = base / "source", base / "candidate"
            source.mkdir()
            candidate.mkdir()
            fixture(candidate)
            (source / "go.mod").write_text("module example.invalid/test\n")
            wrapper = source / "deploy/sync-public-repo.sh"
            wrapper.parent.mkdir(parents=True)
            wrapper.write_text("# private publication entrypoint\n")

            tree = publisher.prepare(source, candidate)
            paths = git(candidate, "ls-tree", "-r", "--name-only", tree).decode().splitlines()

            self.assertTrue(wrapper.exists())
            self.assertNotIn("deploy/sync-public-repo.sh", paths)

    def test_disguised_binary_and_hex_secret_are_refused(self):
        from publication_security import content_problem
        for contents in (b"\x7fELF" + b"A" * 70, b"a" * 128):
            self.assertIsNotNone(content_problem("documentation.txt", contents))


if __name__ == "__main__":
    unittest.main()