import contextlib
import io
import json
import unittest
from unittest.mock import patch

from rollout_cleanup import cli


class RolloutCliRedactionTests(unittest.TestCase):
    def test_sensitive_fields_and_inline_credentials_are_not_written(self):
        canary = "test-only-not-a-real-secret"
        payload = {
            "status": "ok",
            "password": canary,
            "dbPassword": canary,
            "walletMnemonicPhrase": "alpha beta gamma",
            "nested": {
                "api_key": canary,
                "message": f"request failed token={canary}; mnemonic phrase: alpha beta gamma",
                "url": "https://operator:secret@example.invalid/path",
                "safe": "read-only",
            },
        }
        output = io.StringIO()
        with patch.object(cli, "_dispatch", return_value=payload):
            with contextlib.redirect_stdout(output):
                status = cli.main(["scan", "--dry-run"])

        self.assertEqual(status, 0)
        rendered = output.getvalue()
        self.assertNotIn(canary, rendered)
        parsed = json.loads(rendered)
        self.assertEqual(parsed["status"], "ok")
        self.assertEqual(parsed["nested"]["safe"], "read-only")
        self.assertNotIn("password", parsed)
        self.assertNotIn("dbPassword", parsed)
        self.assertNotIn("walletMnemonicPhrase", parsed)
        self.assertNotIn("api_key", parsed["nested"])
        self.assertIn("[REDACTED]", parsed["nested"]["message"])
        self.assertNotIn("alpha beta gamma", parsed["nested"]["message"])
        self.assertNotIn("operator:secret@", parsed["nested"]["url"])


if __name__ == "__main__":
    unittest.main()
