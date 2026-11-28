import unittest

import verify_public_mainnet as check


class VerifyPublicMainnetTests(unittest.TestCase):
    def baseline(self):
        genesis = {"height": 0, "hash": check.GENESIS, "prev_hash": "0" * 64}
        return {
            "0": (200, genesis),
            check.GENESIS: (200, genesis),
            "1": (200, {"height": 1, "hash": check.FIRST, "prev_hash": check.GENESIS_INDEX}),
            str(check.SNAPSHOT_HEIGHT): (
                200,
                {"height": check.SNAPSHOT_HEIGHT, "hash": check.SNAPSHOT_TIP},
            ),
            check.STALE_GENESIS: (404, None),
            check.STALE_FIRST: (404, None),
        }

    def test_pinned_chain_passes(self):
        responses = self.baseline()
        self.assertEqual(check.verify_host("test", lambda _, block: responses[block]), [])

    def test_stale_height_and_hash_fail_even_when_checkpoint_matches(self):
        responses = self.baseline()
        responses["0"] = (200, {"height": 0, "hash": check.STALE_GENESIS, "pruned": True})
        responses[check.STALE_GENESIS] = responses["0"]
        errors = check.verify_host("test", lambda _, block: responses[block])
        self.assertTrue(any("unexpected block" in error for error in errors))
        self.assertTrue(any("pruned row" in error for error in errors))
        self.assertTrue(any("expected HTTP 404" in error for error in errors))

    def test_different_legacy_parent_or_redirect_fails(self):
        responses = self.baseline()
        responses["1"][1]["prev_hash"] = check.GENESIS
        responses[check.GENESIS] = (302, None)
        errors = check.verify_host("test", lambda _, block: responses[block])
        self.assertTrue(any("unexpected parent" in error for error in errors))
        self.assertTrue(any("got 302" in error for error in errors))
