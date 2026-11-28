import unittest
import json
from http.server import HTTPServer
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Thread
from urllib.error import URLError
from urllib.request import urlopen

from validator_health_exporter import (
    NodeAPI,
    SnapshotStore,
    ValidatorSnapshot,
    make_handler,
    render_metrics,
    render_status,
    snapshot_from_payloads,
    validator_mode_enabled_from_config,
)


KEY = "08fd" + "ab" * 30


def payloads(
    *,
    key=KEY,
    status="active",
    peer_heights=(1000,),
    height=1000,
    validator_mode_enabled=True,
):
    return (
        {"pub_key": key},
        {"validators": [{"pub_key": KEY.upper(), "status": status}]},
        {"height": height, "validator_mode_enabled": validator_mode_enabled},
        [{"height": peer_height, "addr": "peer address is intentionally not exported"} for peer_height in peer_heights],
    )


class ValidatorHealthExporterTests(unittest.TestCase):
    def test_requires_local_key_active_registry_and_p2p_peer(self):
        snapshot = snapshot_from_payloads(*payloads())
        self.assertTrue(snapshot.status_available)
        self.assertTrue(snapshot.validator_mode_enabled)
        self.assertTrue(snapshot.key_loaded)
        self.assertTrue(snapshot.registry_active)
        self.assertTrue(snapshot.p2p_connected)
        self.assertTrue(snapshot.online)

    def test_offline_when_node_is_configured_as_non_validator(self):
        snapshot = snapshot_from_payloads(*payloads(validator_mode_enabled=False))
        self.assertFalse(snapshot.validator_mode_enabled)
        self.assertTrue(snapshot.key_loaded)
        self.assertTrue(snapshot.registry_active)
        self.assertTrue(snapshot.p2p_connected)
        self.assertFalse(snapshot.online)
        self.assertTrue(snapshot.sync_healthy)

    def test_missing_validator_mode_fails_closed(self):
        my_validator, registry, status, peers = payloads()
        status.pop("validator_mode_enabled")
        snapshot = snapshot_from_payloads(my_validator, registry, status, peers)
        self.assertFalse(snapshot.validator_mode_enabled)
        self.assertFalse(snapshot.online)

    def test_reads_only_consensus_non_validator_setting(self):
        with TemporaryDirectory() as directory:
            config = Path(directory) / "node.yaml"
            config.write_text(
                "network: mainnet\n"
                "non_validator: false\n"
                "consensus:\n"
                "  proposer_window: 21\n"
                "  non_validator: true # sync-only\n",
                encoding="utf-8",
            )
            self.assertFalse(validator_mode_enabled_from_config(config))
            config.write_text(
                "network: mainnet\n"
                "consensus:\n"
                "  non_validator: false\n",
                encoding="utf-8",
            )
            self.assertTrue(validator_mode_enabled_from_config(config))

    def test_missing_or_ambiguous_consensus_mode_is_rejected(self):
        with TemporaryDirectory() as directory:
            config = Path(directory) / "node.yaml"
            for contents in (
                "network: mainnet\n",
                "consensus:\n  non_validator: true\n  non_validator: false\n",
            ):
                config.write_text(contents, encoding="utf-8")
                with self.assertRaises(ValueError):
                    validator_mode_enabled_from_config(config)

    def test_offline_when_local_key_is_missing(self):
        snapshot = snapshot_from_payloads(*payloads(key=None))
        self.assertFalse(snapshot.key_loaded)
        self.assertFalse(snapshot.registry_active)
        self.assertFalse(snapshot.online)

    def test_offline_when_key_is_not_active_in_registry(self):
        snapshot = snapshot_from_payloads(*payloads(status="unbonding"))
        self.assertTrue(snapshot.key_loaded)
        self.assertFalse(snapshot.registry_active)
        self.assertFalse(snapshot.online)

    def test_offline_when_no_p2p_peers_are_connected(self):
        snapshot = snapshot_from_payloads(*payloads(peer_heights=()))
        self.assertTrue(snapshot.registry_active)
        self.assertFalse(snapshot.p2p_connected)
        self.assertFalse(snapshot.online)

    def test_online_but_sync_unhealthy_when_peer_is_far_behind(self):
        snapshot = snapshot_from_payloads(*payloads(peer_heights=(1_000_000,), height=1_022_607))
        self.assertTrue(snapshot.online)
        self.assertFalse(snapshot.sync_healthy)
        self.assertEqual(snapshot.max_peer_lag_blocks, 22_607)

    def test_peer_with_unknown_height_is_not_counted_as_synced(self):
        snapshot = snapshot_from_payloads(*payloads(peer_heights=(0,), height=1000))
        self.assertTrue(snapshot.online)
        self.assertFalse(snapshot.sync_healthy)
        self.assertEqual(snapshot.max_peer_lag_blocks, 1000)

    def test_sync_health_allows_a_near_tip_peer(self):
        snapshot = snapshot_from_payloads(*payloads(peer_heights=(950,), height=1000))
        self.assertTrue(snapshot.online)
        self.assertTrue(snapshot.sync_healthy)
        self.assertEqual(snapshot.max_peer_lag_blocks, 50)

    def test_unavailable_snapshot_does_not_claim_offline_or_zero_lag(self):
        text = render_metrics(ValidatorSnapshot())
        samples = {line.split()[0] for line in text.splitlines() if line and not line.startswith("#")}
        self.assertIn("aperod_validator_monitor_status_available 0", text)
        self.assertNotIn("aperod_validator_monitor_online", samples)
        self.assertNotIn("aperod_validator_monitor_sync_healthy", samples)
        self.assertNotIn("aperod_validator_monitor_peer_sync_lag_blocks_max", samples)
        self.assertNotIn("peer address", text)

    def test_status_json_reports_verified_online_and_sync_state(self):
        snapshot = snapshot_from_payloads(*payloads(peer_heights=(899,), height=1000))
        status = render_status(snapshot, checked_at_ms=1_795_622_367_607)
        self.assertEqual(status, {
            "status_available": True,
            "status": "online",
            "online": True,
            "validator_mode_enabled": True,
            "sync_healthy": False,
            "peer_count": 1,
            "max_peer_lag_blocks": 101,
            "chain_height": 1000,
            "checked_at_ms": 1_795_622_367_607,
        })
        self.assertNotIn("addr", status)

    def test_unavailable_status_is_unknown_not_false_offline_or_zero_lag(self):
        status = render_status(ValidatorSnapshot(), checked_at_ms=1_795_622_367_607)
        self.assertEqual(status, {
            "status_available": False,
            "status": "unknown",
            "online": None,
            "validator_mode_enabled": None,
            "sync_healthy": None,
            "peer_count": None,
            "max_peer_lag_blocks": None,
            "chain_height": None,
            "checked_at_ms": None,
        })

    def test_offline_status_reports_sync_health_separately(self):
        snapshot = snapshot_from_payloads(*payloads(peer_heights=(), height=1000))
        status = render_status(snapshot, checked_at_ms=1_795_622_367_607)
        self.assertEqual(status["status"], "offline")
        self.assertTrue(status["status_available"])
        self.assertFalse(status["online"])
        self.assertFalse(status["sync_healthy"])
        self.assertEqual(status["peer_count"], 0)

    def test_status_http_endpoint_returns_json_without_cache(self):
        class HealthyAPI:
            def collect(self, _max_lag):
                return snapshot_from_payloads(*payloads())

        store = SnapshotStore(HealthyAPI(), 100)
        store.refresh()
        server = HTTPServer(("127.0.0.1", 0), make_handler(store))
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with urlopen(f"http://127.0.0.1:{server.server_port}/status?probe=1", timeout=2) as response:
                status = json.load(response)
                self.assertEqual(response.headers.get("Cache-Control"), "no-store")
            self.assertEqual(status["status"], "online")
            self.assertTrue(status["status_available"])
            self.assertNotIn("peer_addresses", status)
            self.assertNotIn("addr", status)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    def test_api_refuses_non_loopback_destinations(self):
        with self.assertRaises(ValueError):
            NodeAPI("http://example.com:18545", 1)

    def test_collector_fails_closed_on_api_error(self):
        class FailedAPI:
            def collect(self, _max_lag):
                raise URLError("unavailable")

        snapshot = SnapshotStore(FailedAPI(), 100).refresh()
        self.assertFalse(snapshot.status_available)
        self.assertFalse(snapshot.online)
        self.assertEqual(snapshot.peer_count, 0)


if __name__ == "__main__":
    unittest.main()
