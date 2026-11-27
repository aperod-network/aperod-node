#!/usr/bin/env python3
"""Expose a local node's validator mode, key, registry, P2P, and sync state."""

from __future__ import annotations

import argparse
import grp
import json
import os
import re
import sys
import threading
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.error import URLError
from urllib.parse import urljoin, urlparse
from urllib.request import (
    HTTPRedirectHandler,
    ProxyHandler,
    Request,
    build_opener,
)


DEFAULT_API_URL = "http://127.0.0.1:18545"
DEFAULT_LISTEN_ADDRESS = "127.0.0.1:18547"
DEFAULT_POLL_SECONDS = 15
DEFAULT_REQUEST_TIMEOUT_SECONDS = 3
DEFAULT_MAX_SYNC_LAG_BLOCKS = 100
PUBKEY_PATTERN = re.compile(r"^[0-9a-f]{64}$")


@dataclass(frozen=True)
class ValidatorSnapshot:
    status_available: bool = False
    validator_mode_enabled: bool = False
    key_loaded: bool = False
    registry_active: bool = False
    p2p_connected: bool = False
    online: bool = False
    sync_healthy: bool = False
    peer_count: int = 0
    max_peer_lag_blocks: int = 0
    chain_height: int = 0


def _nonnegative_int(value: object, field: str) -> int:
    if isinstance(value, bool):
        raise ValueError(f"{field} must be a nonnegative integer")
    if isinstance(value, float) and not value.is_integer():
        raise ValueError(f"{field} must be a nonnegative integer")
    try:
        parsed = int(value)  # API may encode heights as decimal strings.
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{field} must be a nonnegative integer") from exc
    if parsed < 0:
        raise ValueError(f"{field} must be a nonnegative integer")
    return parsed


def validator_mode_enabled_from_config(config_path: str | Path) -> bool:
    """Read only consensus.non_validator; reject missing or ambiguous settings."""
    text = Path(config_path).read_text(encoding="utf-8")
    in_consensus = False
    values: list[bool] = []
    for raw_line in text.splitlines():
        if not raw_line.strip() or raw_line.lstrip().startswith("#"):
            continue
        leading = raw_line[: len(raw_line) - len(raw_line.lstrip())]
        if "\t" in leading:
            raise ValueError("node config uses a tab-indented line")
        indent = len(raw_line) - len(raw_line.lstrip(" "))
        content = raw_line.strip()
        if indent == 0:
            in_consensus = bool(re.fullmatch(r"consensus\s*:\s*(?:#.*)?", content))
            continue
        if in_consensus and indent == 2:
            match = re.fullmatch(
                r"non_validator\s*:\s*(true|false)\s*(?:#.*)?",
                content,
                re.IGNORECASE,
            )
            if match:
                values.append(match.group(1).lower() == "false")
    if len(values) != 1:
        raise ValueError("node config must contain exactly one consensus.non_validator boolean")
    return values[0]


def write_validator_mode_file(config_path: str | Path, output_path: str | Path) -> None:
    """Write a minimal, group-readable mode value for the unprivileged exporter."""
    enabled = validator_mode_enabled_from_config(config_path)
    output = Path(output_path)
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_name(f".{output.name}.{os.getpid()}.tmp")
    try:
        temporary.write_text("enabled\n" if enabled else "disabled\n", encoding="ascii")
        os.chmod(temporary, 0o640)
        if os.geteuid() == 0:
            os.chown(temporary, 0, grp.getgrnam("prometheus").gr_gid)
        os.replace(temporary, output)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def read_validator_mode_file(mode_path: str | Path) -> bool:
    mode = Path(mode_path).read_text(encoding="ascii").strip()
    if mode not in {"enabled", "disabled"}:
        raise ValueError("validator mode file must contain enabled or disabled")
    return mode == "enabled"


def snapshot_from_payloads(
    my_validator: object,
    registry: object,
    status: object,
    peers: object,
    max_sync_lag_blocks: int = DEFAULT_MAX_SYNC_LAG_BLOCKS,
) -> ValidatorSnapshot:
    if not isinstance(my_validator, dict):
        raise ValueError("my-validator response must be an object")
    if not isinstance(registry, dict) or not isinstance(registry.get("validators"), list):
        raise ValueError("validator registry response must contain a validators array")
    if not isinstance(status, dict):
        raise ValueError("node status response must be an object")
    if not isinstance(peers, list):
        raise ValueError("network peers response must be an array")
    if max_sync_lag_blocks < 0:
        raise ValueError("sync lag limit must be nonnegative")

    validator_mode_enabled = status.get("validator_mode_enabled") is True
    public_key = my_validator.get("pub_key")
    public_key = public_key.strip().lower() if isinstance(public_key, str) else ""
    key_loaded = bool(PUBKEY_PATTERN.fullmatch(public_key))
    registry_active = key_loaded and any(
        isinstance(entry, dict)
        and isinstance(entry.get("pub_key"), str)
        and entry["pub_key"].strip().lower() == public_key
        and entry.get("status") == "active"
        for entry in registry["validators"]
    )

    chain_height = _nonnegative_int(status.get("height"), "height")
    peer_heights = [
        _nonnegative_int(peer.get("height"), "peer height")
        for peer in peers
        if isinstance(peer, dict)
    ]
    if len(peer_heights) != len(peers):
        raise ValueError("every network peer must include a height")

    peer_count = len(peer_heights)
    p2p_connected = peer_count > 0
    max_peer_lag = max(
        (max(0, chain_height - peer_height) for peer_height in peer_heights),
        default=0,
    )
    has_near_tip_peer = any(
        peer_height > 0 and max(0, chain_height - peer_height) <= max_sync_lag_blocks
        for peer_height in peer_heights
    )
    online = validator_mode_enabled and key_loaded and registry_active and p2p_connected

    return ValidatorSnapshot(
        status_available=True,
        validator_mode_enabled=validator_mode_enabled,
        key_loaded=key_loaded,
        registry_active=registry_active,
        p2p_connected=p2p_connected,
        online=online,
        sync_healthy=p2p_connected and has_near_tip_peer,
        peer_count=peer_count,
        max_peer_lag_blocks=max_peer_lag,
        chain_height=chain_height,
    )


class NodeAPI:
    def __init__(
        self,
        base_url: str,
        timeout_seconds: float,
        validator_mode_file: str | None = None,
    ) -> None:
        parsed = urlparse(base_url)
        if parsed.scheme != "http" or parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
            raise ValueError("node API must use HTTP on a loopback address")
        self.base_url = base_url.rstrip("/") + "/"
        self.timeout_seconds = timeout_seconds
        self.validator_mode_file = validator_mode_file
        self.opener = build_opener(ProxyHandler({}), _NoRedirectHandler())

    def get_json(self, path: str) -> object:
        request = Request(
            urljoin(self.base_url, path.lstrip("/")),
            headers={"Accept": "application/json"},
        )
        with self.opener.open(request, timeout=self.timeout_seconds) as response:
            if response.status != 200:
                raise URLError(f"node API returned HTTP {response.status}")
            return json.loads(response.read().decode("utf-8"))

    def collect(self, max_sync_lag_blocks: int) -> ValidatorSnapshot:
        if not self.validator_mode_file:
            raise ValueError("validator mode file is not configured")
        status = self.get_json("/api/v1/status")
        if not isinstance(status, dict):
            raise ValueError("node status response must be an object")
        status = dict(status)
        status["validator_mode_enabled"] = read_validator_mode_file(self.validator_mode_file)
        return snapshot_from_payloads(
            self.get_json("/api/v1/my-validator"),
            self.get_json("/api/v1/validators"),
            status,
            self.get_json("/api/v1/network/peers"),
            max_sync_lag_blocks=max_sync_lag_blocks,
        )


class _NoRedirectHandler(HTTPRedirectHandler):
    def redirect_request(self, *_args: object, **_kwargs: object):
        raise URLError("node API redirects are disabled")


def render_metrics(snapshot: ValidatorSnapshot) -> str:
    metrics = (
        ("aperod_validator_monitor_status_available", "Whether all local node status endpoints responded."),
        ("aperod_validator_monitor_validator_mode_enabled", "Whether the local node is configured to participate as a validator."),
        ("aperod_validator_monitor_local_key_loaded", "Whether the local validator signing identity is loaded."),
        ("aperod_validator_monitor_local_active", "Whether the local key is active in the on-chain validator registry."),
        ("aperod_validator_monitor_p2p_connected", "Whether the node has at least one established P2P peer."),
        ("aperod_validator_monitor_online", "Whether validator mode is enabled, the local key is active, and the node has a P2P peer."),
        ("aperod_validator_monitor_sync_healthy", "Whether the node has a peer within the configured block lag, independent of validator mode."),
        ("aperod_validator_monitor_peer_count", "Number of connected P2P peers."),
        ("aperod_validator_monitor_peer_sync_lag_blocks_max", "Maximum reported block-height lag among connected peers."),
        ("aperod_validator_monitor_chain_height", "Current local Mainnet chain height."),
    )
    values = {"aperod_validator_monitor_status_available": int(snapshot.status_available)}
    if snapshot.status_available:
        values.update(
            {
                "aperod_validator_monitor_validator_mode_enabled": int(snapshot.validator_mode_enabled),
                "aperod_validator_monitor_local_key_loaded": int(snapshot.key_loaded),
                "aperod_validator_monitor_local_active": int(snapshot.registry_active),
                "aperod_validator_monitor_p2p_connected": int(snapshot.p2p_connected),
                "aperod_validator_monitor_online": int(snapshot.online),
                "aperod_validator_monitor_sync_healthy": int(snapshot.sync_healthy),
                "aperod_validator_monitor_peer_count": snapshot.peer_count,
                "aperod_validator_monitor_peer_sync_lag_blocks_max": snapshot.max_peer_lag_blocks,
                "aperod_validator_monitor_chain_height": snapshot.chain_height,
            }
        )
    lines = []
    for name, help_text in metrics:
        lines.extend((f"# HELP {name} {help_text}", f"# TYPE {name} gauge"))
        if name in values:
            lines.append(f"{name} {values[name]}")
    return "\n".join(lines) + "\n"


def render_status(snapshot: ValidatorSnapshot, checked_at_ms: int | None) -> dict[str, object]:
    available = snapshot.status_available and checked_at_ms is not None
    online = snapshot.online if available else None
    return {
        "status_available": available,
        "status": "online" if online is True else "offline" if online is False else "unknown",
        "online": online,
        "validator_mode_enabled": snapshot.validator_mode_enabled if available else None,
        "sync_healthy": snapshot.sync_healthy if available else None,
        "peer_count": snapshot.peer_count if available else None,
        "max_peer_lag_blocks": snapshot.max_peer_lag_blocks if available else None,
        "chain_height": snapshot.chain_height if available else None,
        "checked_at_ms": checked_at_ms if available else None,
    }


class SnapshotStore:
    def __init__(self, api: NodeAPI, max_sync_lag_blocks: int) -> None:
        self.api = api
        self.max_sync_lag_blocks = max_sync_lag_blocks
        self._snapshot = ValidatorSnapshot()
        self._checked_at_ms: int | None = None
        self._lock = threading.Lock()

    def refresh(self) -> ValidatorSnapshot:
        try:
            snapshot = self.api.collect(self.max_sync_lag_blocks)
        except Exception as exc:  # Fail closed: no stale "online" state on API failure.
            print(f"validator status poll failed: {type(exc).__name__}", file=sys.stderr, flush=True)
            snapshot = ValidatorSnapshot()
        with self._lock:
            self._snapshot = snapshot
            self._checked_at_ms = time.time_ns() // 1_000_000
        return snapshot

    def read(self) -> ValidatorSnapshot:
        with self._lock:
            return self._snapshot

    def read_status(self) -> dict[str, object]:
        with self._lock:
            return render_status(self._snapshot, self._checked_at_ms)


def _loopback_listen_address(value: str) -> tuple[str, int]:
    host, separator, port_text = value.rpartition(":")
    if not separator or host != "127.0.0.1":
        raise ValueError("exporter must listen on 127.0.0.1 only")
    port = int(port_text)
    if not 1 <= port <= 65535:
        raise ValueError("listen port must be between 1 and 65535")
    return host, port


def make_handler(store: SnapshotStore):
    class MetricsHandler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            request_path = urlparse(self.path).path
            if request_path == "/healthz":
                self.send_response(200)
                self.send_header("Content-Length", "3")
                self.end_headers()
                self.wfile.write(b"ok\n")
                return
            if request_path == "/status":
                body = json.dumps(store.read_status(), separators=(",", ":"), allow_nan=False).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json; charset=utf-8")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if request_path != "/metrics":
                self.send_error(404)
                return
            body = render_metrics(store.read()).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format: str, *_args: object) -> None:
            return

    return MetricsHandler


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--once", action="store_true", help="print one snapshot and exit")
    parser.add_argument(
        "--node-config",
        default=os.environ.get("APEROD_NODE_CONFIG"),
        help="Mainnet node config used by the privileged mode probe",
    )
    parser.add_argument(
        "--validator-mode-file",
        default=os.environ.get("APEROD_VALIDATOR_MODE_FILE"),
        help="mode file created by the privileged systemd pre-start probe",
    )
    parser.add_argument(
        "--write-validator-mode-file",
        help="write enabled/disabled from consensus.non_validator and exit",
    )
    parser.add_argument(
        "--api-url",
        default=os.environ.get("APEROD_NODE_API_URL", DEFAULT_API_URL),
    )
    parser.add_argument(
        "--listen-address",
        default=os.environ.get("APEROD_EXPORTER_LISTEN", DEFAULT_LISTEN_ADDRESS),
    )
    parser.add_argument(
        "--poll-seconds",
        type=int,
        default=int(os.environ.get("APEROD_EXPORTER_POLL_SECONDS", DEFAULT_POLL_SECONDS)),
    )
    parser.add_argument(
        "--request-timeout-seconds",
        type=float,
        default=float(os.environ.get("APEROD_EXPORTER_REQUEST_TIMEOUT_SECONDS", DEFAULT_REQUEST_TIMEOUT_SECONDS)),
    )
    parser.add_argument(
        "--max-sync-lag-blocks",
        type=int,
        default=int(os.environ.get("APEROD_EXPORTER_MAX_SYNC_LAG_BLOCKS", DEFAULT_MAX_SYNC_LAG_BLOCKS)),
    )
    args = parser.parse_args()
    if args.poll_seconds < 1 or args.request_timeout_seconds <= 0 or args.max_sync_lag_blocks < 0:
        parser.error("poll interval, timeout, and sync lag limit must be positive")
    if args.write_validator_mode_file:
        if not args.node_config:
            parser.error("--node-config is required with --write-validator-mode-file")
        try:
            write_validator_mode_file(args.node_config, args.write_validator_mode_file)
        except (OSError, ValueError, KeyError) as exc:
            parser.error(f"could not determine validator mode: {type(exc).__name__}")
        return 0
    if not args.validator_mode_file:
        parser.error("--validator-mode-file is required")

    try:
        api = NodeAPI(args.api_url, args.request_timeout_seconds, args.validator_mode_file)
        listen_host, listen_port = _loopback_listen_address(args.listen_address)
    except ValueError as exc:
        parser.error(str(exc))

    store = SnapshotStore(api, args.max_sync_lag_blocks)
    if args.once:
        print(render_metrics(store.refresh()), end="")
        return 0

    store.refresh()
    stop = threading.Event()

    def poll_forever() -> None:
        while not stop.wait(args.poll_seconds):
            store.refresh()

    poller = threading.Thread(target=poll_forever, name="validator-status-poller", daemon=True)
    poller.start()
    server = ThreadingHTTPServer((listen_host, listen_port), make_handler(store))
    server.daemon_threads = True
    try:
        server.serve_forever(poll_interval=0.5)
    except KeyboardInterrupt:
        pass
    finally:
        stop.set()
        server.shutdown()
        server.server_close()
        poller.join(timeout=args.request_timeout_seconds + 1)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
