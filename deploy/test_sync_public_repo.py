import unittest
from unittest.mock import Mock
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


if __name__ == "__main__":
    unittest.main()