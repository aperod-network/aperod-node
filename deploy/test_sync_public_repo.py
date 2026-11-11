import unittest
import urllib.request
from unittest.mock import Mock, patch
import sync_public_repo


class LegacyPublicationTests(unittest.TestCase):
    def test_direct_upload_route_is_disabled(self):
        request = Mock()
        with self.assertRaisesRegex(RuntimeError, "Legacy publication disabled"):
            sync_public_repo.sync("files", "deletions", "head", request)
        request.assert_not_called()

    def test_cli_cannot_accept_a_token_argument(self):
        with self.assertRaises(SystemExit) as result:
            sync_public_repo.main()
        self.assertIn("Legacy publication disabled", str(result.exception))

    def test_authenticated_transport_rejects_non_api_hosts_before_request(self):
        with patch.object(sync_public_repo.urllib.request, "build_opener") as opener:
            with self.assertRaisesRegex(RuntimeError, "outside GitHub API"):
                sync_public_repo.make_request("test-only-token")(
                    "POST", "https://example.invalid/repos/owner/repo/git/blobs",
                    {"content": "test"},
                )
        opener.assert_not_called()

    def test_redirect_cannot_forward_token_outside_github_api(self):
        req = urllib.request.Request(
            "https://api.github.com/repos/aperod-network/aperod-node",
            headers={"Authorization": "Bearer test-only-token"},
        )
        with self.assertRaisesRegex(RuntimeError, "redirect refused"):
            sync_public_repo.SafeRedirect().redirect_request(
                req, None, 302, "Found", {}, "https://example.invalid/collect"
            )


if __name__ == "__main__":
    unittest.main()