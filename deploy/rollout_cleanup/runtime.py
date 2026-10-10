"""Injectable host/runtime boundary used by the production engine and fixtures."""

from __future__ import annotations

import os
import json
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable


MAX_JSON = 4 * 1024 * 1024


class CleanupError(Exception):
    """A conservative refusal with a safe-to-display message."""


def _pairs_no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise CleanupError("duplicate JSON object key")
        result[key] = value
    return result


def strict_json_bytes(raw: bytes) -> Any:
    if len(raw) > MAX_JSON:
        raise CleanupError("JSON exceeds size limit")
    try:
        return json.loads(
            raw.decode("utf-8"), object_pairs_hook=_pairs_no_duplicates,
            parse_constant=lambda _value: (_ for _ in ()).throw(CleanupError("non-finite JSON number")),
        )
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError) as exc:
        raise CleanupError("invalid JSON") from exc


@dataclass
class Runtime:
    """Host operations and installation paths.

    The CLI always uses ``DEFAULT_RUNTIME``. Tests may pass a fixture instance
    directly to library operations; there is deliberately no environment or
    command-line switch that weakens installed CLI checks.
    """

    state_dir: Path = Path("/var/lib/aperod-rollout-cleanup")
    releases_dir: Path = Path("/opt/aperod/releases")
    settings_file: Path = Path("/opt/aperod/data/integration-settings.json")
    installed_binary: Path = Path("/usr/local/bin/aperod-node")
    clock: Callable[[], float] = time.time
    sleep: Callable[[float], None] = time.sleep
    proc_root: Path = Path("/proc")
    completion_polls: int = 3
    completion_interval: float = 0.5
    is_root: Callable[[], bool] = lambda: os.geteuid() == 0
    root_owned: Callable[[Any], bool] = lambda info: info.st_uid == 0
    service_pid: Callable[[str], int] | None = None
    process_start_ticks: Callable[[int], int] | None = None
    process_exe: Callable[[int], Path] | None = None
    live_exe_sha256: Callable[[int], str] | None = None
    api_json: Callable[[str], Any] | None = None
    verify_process_binding: Callable[[int, Path, Path], None] | None = None


DEFAULT_RUNTIME = Runtime()