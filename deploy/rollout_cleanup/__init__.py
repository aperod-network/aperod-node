"""Fail-closed cleanup of explicitly registered Aperod rollout artifacts."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
import re
import stat
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path
from typing import Any

from .runtime import CleanupError, DEFAULT_RUNTIME, Runtime, strict_json_bytes
from .provider import BackupProvider, DEFAULT_PROVIDER, NativeB2Provider


STATE_DIR = Path("/var/lib/aperod-rollout-cleanup")
RELEASES_DIR = Path("/opt/aperod/releases")
SETTINGS_FILE = Path("/opt/aperod/data/integration-settings.json")
INSTALLED_BINARY = Path("/usr/local/bin/aperod-node")
MAX_JSON = 4 * 1024 * 1024
PF_KTHREAD = 0x00200000
HEX64 = re.compile(r"^[0-9a-fA-F]{64}$")
UUID_RE = re.compile(r"^[0-9a-fA-F-]{36}$")
PROTECTED_NAMES = re.compile(
    r"(?:key|wallet|identity|migration|migrat|archive|backup|secret|keystore|"
    r"private|validator|witness|node\.key|seed|mnemonic)",
    re.IGNORECASE,
)
LEVELDB_NAME = re.compile(
    r"^(?:CURRENT|LOCK|LOG(?:\.old)?|MANIFEST-[0-9]+|OPTIONS-[0-9]+|"
    r"[0-9]+\.ldb|[0-9]+\.sst|[0-9]+\.log|snapshot[^/]*\.(?:gz|sha256))$"
)
JOB_FILE = re.compile(r"^[0-9a-fA-F-]{36}\.json$")
CONTEXT_MARKER = re.compile(r"^backup-context-[0-9a-f]{32}\.json$")


def read_json(path: Path, *, bounded: bool = True) -> Any:
    flags = (os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
             | getattr(os, "O_NONBLOCK", 0))
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > MAX_JSON:
                raise CleanupError("unsafe or oversized JSON file")
            raw = stream.read(MAX_JSON + 1)
    except OSError as exc:
        raise CleanupError("cannot safely read JSON file") from exc
    return strict_json_bytes(raw) if bounded else json.loads(raw)


def _json_bytes(value: Any) -> bytes:
    try:
        raw = (json.dumps(value, sort_keys=True, separators=(",", ":"),
                          ensure_ascii=True, allow_nan=False) + "\n").encode()
    except (TypeError, ValueError) as exc:
        raise CleanupError("value cannot be represented as strict JSON") from exc
    if len(raw) > MAX_JSON:
        raise CleanupError("JSON exceeds size limit")
    return raw


def atomic_json(path: Path, value: Any) -> None:
    """Write an owner-only atomic JSON record and durably sync file and parent."""
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    _assert_root_dir(path.parent, require_root=(os.geteuid() == 0))
    temp = path.parent / (".tmp-" + uuid.uuid4().hex)
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(_json_bytes(value))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        os.chmod(path, 0o600, follow_symlinks=False)
        dfd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(dfd)
        finally:
            os.close(dfd)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temp.unlink()


def _rt(runtime: Runtime | None = None) -> Runtime:
    return runtime or DEFAULT_RUNTIME


def _owns_root(info: os.stat_result, runtime: Runtime | None = None) -> bool:
    return bool(_rt(runtime).root_owned(info))


def _assert_root_dir(path: Path, *, require_root: bool = True,
                     runtime: Runtime | None = None) -> os.stat_result:
    info = os.lstat(path)
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise CleanupError("state parent is not a real directory")
    if require_root and not _owns_root(info, runtime):
        raise CleanupError("state directory is not root-owned")
    if info.st_mode & 0o022:
        raise CleanupError("state directory is group/world writable")
    return info


def _check_root(runtime: Runtime | None = None) -> None:
    if not _rt(runtime).is_root():
        raise CleanupError("must run as root")


@contextlib.contextmanager
def state_lock(state_dir: Path | None = None, *, runtime: Runtime | None = None):
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    _check_root(rt)
    state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    _assert_root_dir(state_dir, runtime=rt)
    lockpath = state_dir / "lock"
    fd = os.open(lockpath, os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or not _owns_root(info, rt) or info.st_mode & 0o077:
            raise CleanupError("unsafe state lock")
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def _safe_record(path: Path, runtime: Runtime | None = None) -> dict[str, Any]:
    value = _read_secure_object(path, root_owned=True, runtime=runtime)
    if not isinstance(value, dict):
        raise CleanupError("state record is not an object")
    return value


def _read_secure_object(path: Path, *, root_owned: bool, runtime: Runtime | None = None) -> Any:
    flags = (os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
             | getattr(os, "O_NONBLOCK", 0))
    try:
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_size > MAX_JSON
                    or info.st_mode & 0o077 or (root_owned and not _owns_root(info, runtime))):
                raise CleanupError("unsafe secured JSON ownership, mode, or size")
            raw = stream.read(MAX_JSON + 1)
    except OSError as exc:
        raise CleanupError("cannot safely read secured JSON file") from exc
    return strict_json_bytes(raw)


def _sha_file(path: Path) -> str:
    digest = hashlib.sha256()
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    fd = os.open(path, flags)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise CleanupError("expected regular file")
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(fd)
        try:
            current = os.lstat(path)
        except OSError as exc:
            raise CleanupError("file changed while hashing") from exc
        if (not stat.S_ISREG(current.st_mode) or stat.S_ISLNK(current.st_mode)
                or (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
                != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns)
                or (current.st_dev, current.st_ino) != (after.st_dev, after.st_ino)):
            raise CleanupError("file changed while hashing")
    finally:
        os.close(fd)
    return digest.hexdigest()


def _identity(path: Path, *, allow_dir: bool = False) -> dict[str, int]:
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not (stat.S_ISREG(info.st_mode) or (allow_dir and stat.S_ISDIR(info.st_mode))):
        raise CleanupError("unsafe source identity type")
    return {"device": info.st_dev, "inode": info.st_ino}


def _mountpoints() -> set[str]:
    try:
        lines = Path("/proc/self/mountinfo").read_text().splitlines()
    except OSError as exc:
        raise CleanupError("cannot inspect mount table") from exc
    points: set[str] = set()
    for line in lines:
        fields = line.split()
        if len(fields) < 6:
            raise CleanupError("malformed mount table")
        points.add(fields[4].replace("\\040", " ").replace("\\011", "\t")
                   .replace("\\012", "\n").replace("\\134", "\\"))
    return points


def _assert_not_mountpoint(path: Path) -> None:
    if str(path) in _mountpoints():
        raise CleanupError("artifact contains a mount point")


def _process_fd_paths(pid: int, runtime: Runtime | None = None) -> list[tuple[int, str]]:
    fd_dir = Path(_rt(runtime).proc_root) / str(pid) / "fd"
    paths: list[tuple[int, str]] = []
    try:
        with os.scandir(fd_dir) as descriptors:
            for descriptor in descriptors:
                try:
                    paths.append((int(descriptor.name), os.readlink(fd_dir / descriptor.name).removesuffix(" (deleted)")))
                except FileNotFoundError as exc:
                    raise CleanupError("process descriptor changed during source check") from exc
                except OSError as exc:
                    raise CleanupError("cannot inspect process descriptors") from exc
    except OSError as exc:
        raise CleanupError("cannot inspect process descriptors") from exc
    return paths


def _verify_process_binding(pid: int, data_dir: Path, config: Path,
                            runtime: Runtime | None = None) -> None:
    """Bind caller-supplied paths to the service's actual live command/fds."""
    callback = _rt(runtime).verify_process_binding
    if callback is not None:
        callback(pid, data_dir, config)
        return
    try:
        proc_dir = Path(_rt(runtime).proc_root) / str(pid)
        cmdline_raw = (proc_dir / "cmdline").read_bytes()
        cmdline = [arg.decode(errors="strict") for arg in cmdline_raw.rstrip(b"\0").split(b"\0") if arg]
    except (OSError, UnicodeDecodeError) as exc:
        raise CleanupError("cannot inspect live process command line") from exc
    descriptors = _process_fd_paths(pid, runtime)
    try:
        cwd = Path(os.readlink(proc_dir / "cwd"))
    except OSError as exc:
        raise CleanupError("cannot inspect live process cwd") from exc
    if str(cwd).endswith(" (deleted)"):
        raise CleanupError("live process cwd is deleted or ambiguous")
    db = str((data_dir / "chain.db").resolve(strict=True))
    source_open = any(value == db or value.startswith(db + os.sep) for _, value in descriptors)
    if source_open:
        source_open = False
        for descriptor, value in descriptors:
            if value == db or value.startswith(db + os.sep):
                try:
                    fd_info = os.stat(proc_dir / "fd" / str(descriptor))
                    path_info = os.stat(value)
                except OSError:
                    continue
                if (fd_info.st_dev, fd_info.st_ino) == (path_info.st_dev, path_info.st_ino):
                    source_open = True
                    break
    if not source_open:
        raise CleanupError("live process does not hold an fd beneath the declared chain.db")
    config_text = str(config.resolve(strict=True))
    config_ref = config_text in cmdline or any(value == config_text for _, value in descriptors)
    config_args = []
    for index, argument in enumerate(cmdline):
        if argument in ("--config", "-config", "--config-file") and index + 1 < len(cmdline):
            config_args.append(cmdline[index + 1])
        elif argument.startswith(("--config=", "--config-file=")):
            config_args.append(argument.split("=", 1)[1])
    for argument in config_args:
        if "://" not in argument:
            candidate = Path(argument)
            if not candidate.is_absolute():
                candidate = cwd / candidate
            try:
                matches = str(candidate.resolve()) == config_text
            except (OSError, RuntimeError) as exc:
                raise CleanupError("cannot resolve live process config reference") from exc
            if matches:
                config_ref = True
                break
    if not config_ref:
        raise CleanupError("live process does not reference the declared config")


def _config_hash(path: Path) -> str:
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise CleanupError("config is not a regular non-symlink file")
    return _sha_file(path)


def _http_json(url: str, runtime: Runtime | None = None) -> Any:
    callback = _rt(runtime).api_json
    if callback is not None:
        return callback(url)
    parsed = urllib.parse.urlsplit(url)
    allowed_path = (parsed.path == "/api/v1/status"
                    or re.fullmatch(r"/api/v1/blocks/[0-9]+", parsed.path) is not None)
    if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "::1", "localhost")
            or parsed.username is not None or parsed.password is not None
            or not allowed_path or parsed.query or parsed.fragment):
        raise CleanupError("API URL must be an allowed local HTTP node endpoint")
    req = urllib.request.Request(url, headers={"Accept": "application/json"})

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None

    try:
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
        with opener.open(req, timeout=5) as response:
            raw = response.read(MAX_JSON + 1)
    except (OSError, urllib.error.URLError, TimeoutError) as exc:
        raise CleanupError("node API request failed") from exc
    return strict_json_bytes(raw)


def _hash_field(value: Any) -> str:
    if isinstance(value, str) and HEX64.fullmatch(value):
        return value.lower()
    if isinstance(value, dict):
        for key in ("hash", "block_hash", "blockHash"):
            if key in value:
                return _hash_field(value[key])
    raise CleanupError("unsupported block hash response schema")


def _block(api: str, height: int, runtime: Runtime | None = None) -> tuple[int, str]:
    value = _http_json(api.rstrip("/") + f"/api/v1/blocks/{height}", runtime)
    if isinstance(value, dict):
        raw_height = value.get("height", value.get("block_height", height))
        block = value.get("block", value)
        if isinstance(block, dict):
            returned = block.get("height", raw_height)
            if type(returned) is not int or returned != height:
                raise CleanupError("block API returned an unexpected height")
            return height, _hash_field(block)
    raise CleanupError("unsupported block response schema")


def _status(api: str, runtime: Runtime | None = None) -> dict[str, Any]:
    value = _http_json(api.rstrip("/") + "/api/v1/status", runtime)
    if not isinstance(value, dict):
        raise CleanupError("unsupported status response schema")
    if value.get("ok") is not True or value.get("syncing") is not False or value.get("utxo_rebuilding") is not False:
        raise CleanupError("node is not fully ready")
    height = value.get("height")
    if type(height) is not int or height < 0:
        raise CleanupError("status response has no valid height")
    return value


def _ready_height(api: str, runtime: Runtime | None = None) -> int:
    return _status(api, runtime)["height"]


def _advancing_height(api: str, begin_height: int, runtime: Runtime | None = None) -> int:
    rt = _rt(runtime)
    probes = max(2, min(int(rt.completion_polls), 8))
    interval = max(0.0, min(float(rt.completion_interval), 5.0))
    previous = begin_height
    final = begin_height
    for index in range(probes):
        current = _status(api, rt)["height"]
        if current < previous:
            raise CleanupError("node height regressed during readiness validation")
        previous = current
        final = current
        if index + 1 < probes:
            rt.sleep(interval)
    if final <= begin_height:
        raise CleanupError("node height has not advanced beyond rollout origin")
    return final


def _service_pid(service: str, runtime: Runtime | None = None) -> int:
    callback = _rt(runtime).service_pid
    if callback is not None:
        pid = callback(service)
        if type(pid) is not int or pid <= 1:
            raise CleanupError("service has no live PID")
        return pid
    if not re.fullmatch(r"[A-Za-z0-9_.@-]{1,128}", service):
        raise CleanupError("invalid service name")
    try:
        out = subprocess.check_output(
            ["systemctl", "show", "--property=MainPID", "--value", service],
            stderr=subprocess.DEVNULL, timeout=5, text=True,
        ).strip()
        pid = int(out)
    except (OSError, subprocess.SubprocessError, ValueError) as exc:
        raise CleanupError("cannot determine service PID") from exc
    if pid <= 1:
        raise CleanupError("service has no live PID")
    return pid


def _proc_start_ticks(pid: int, runtime: Runtime | None = None) -> int:
    callback = _rt(runtime).process_start_ticks
    if callback is not None:
        try:
            value = callback(pid)
        except (OSError, KeyError, ValueError) as exc:
            raise CleanupError("cannot read process start identity") from exc
        if type(value) is not int or value < 0:
            raise CleanupError("invalid process start identity")
        return value
    try:
        raw = (Path(_rt(runtime).proc_root) / str(pid) / "stat").read_text()
        tail = raw[raw.rfind(")") + 2:].split()
        return int(tail[19])  # field 22; tail begins at field 3
    except (OSError, ValueError, IndexError) as exc:
        raise CleanupError("cannot read process start identity") from exc


def _process_exe(pid: int, runtime: Runtime | None = None) -> Path:
    callback = _rt(runtime).process_exe
    if callback is not None:
        return Path(callback(pid))
    try:
        return Path(os.readlink(Path(_rt(runtime).proc_root) / str(pid) / "exe"))
    except OSError as exc:
        raise CleanupError("cannot read live process executable") from exc


def _sha_live_exe(pid: int, runtime: Runtime | None = None) -> str:
    callback = _rt(runtime).live_exe_sha256
    if callback is not None:
        return callback(pid)
    try:
        fd = os.open(Path(_rt(runtime).proc_root) / str(pid) / "exe",
                     os.O_RDONLY | getattr(os, "O_CLOEXEC", 0))
    except OSError as exc:
        raise CleanupError("cannot open live process executable") from exc
    digest = hashlib.sha256()
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise CleanupError("live process executable has unsafe type")
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    finally:
        os.close(fd)
    return digest.hexdigest()


def _capture_origin(data_dir: Path, config: Path, service: str, api: str,
                    runtime: Runtime | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    pid = _service_pid(service, rt)
    ticks = _proc_start_ticks(pid, rt)
    _verify_process_binding(pid, data_dir, config, rt)
    db = data_dir / "chain.db"
    db_id = _identity(db, allow_dir=True)
    conf_id = _identity(config)
    config_hash = _config_hash(config)
    height = _ready_height(api, rt)
    _, anchor_hash = _block(api, height, rt)
    _, genesis = _block(api, 0, rt)
    exe = _process_exe(pid, rt)
    if not exe.is_absolute() or not exe.exists():
        raise CleanupError("live process executable is unavailable")
    if (_identity(db, allow_dir=True) != db_id or _identity(config) != conf_id
            or _config_hash(config) != config_hash):
        raise CleanupError("source or config changed during origin capture")
    _verify_process_binding(pid, data_dir, config, rt)
    if _service_pid(service, rt) != pid or _proc_start_ticks(pid, rt) != ticks:
        raise CleanupError("live service changed during origin capture")
    return {
        "data_dir": str(data_dir.resolve(strict=True)),
        "chain_db": db_id,
        "config": str(config.resolve(strict=True)),
        "config_identity": conf_id,
        "config_sha256": config_hash,
        "service": service,
        "pid": pid,
        "start_ticks": ticks,
        "live_exe": str(exe),
        "height": height,
        "hash": anchor_hash,
        "genesis_hash": genesis,
        "api_url": api,
    }


def _release_path(path_text: str, releases_dir: Path | None = None,
                  runtime: Runtime | None = None) -> Path:
    rt = _rt(runtime)
    releases_dir = Path(releases_dir or rt.releases_dir)
    release = Path(path_text)
    try:
        resolved_root = releases_dir.resolve(strict=True)
        resolved = release.resolve(strict=True)
    except OSError as exc:
        raise CleanupError("release path does not exist") from exc
    _assert_root_dir(resolved_root, runtime=rt)
    if resolved.parent != resolved_root or release.is_symlink():
        raise CleanupError("release must be a direct child of the allowed releases directory")
    info = os.lstat(resolved)
    if not stat.S_ISDIR(info.st_mode) or not _owns_root(info, rt) or info.st_mode & 0o022:
        raise CleanupError("release must be a closed root-owned directory")
    matches = 0
    for item in os.scandir(resolved_root):
        try:
            other = item.stat(follow_symlinks=False)
        except OSError as exc:
            raise CleanupError("cannot validate release directory") from exc
        if other.st_dev == info.st_dev and other.st_ino == info.st_ino:
            matches += 1
    if matches != 1:
        raise CleanupError("release directory identity is ambiguous")
    return resolved


def _registered_release(job: dict[str, Any], runtime: Runtime | None = None) -> Path:
    release = _release_path(job["release"], runtime=runtime)
    if _identity(release, allow_dir=True) != job.get("release_identity"):
        raise CleanupError("registered release directory identity changed")
    return release


def begin(release: str, data_dir: str, config: str, service: str, api_url: str,
          *, state_dir: Path | None = None,
          runtime: Runtime | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    root = _release_path(release, runtime=rt)
    origin = _capture_origin(Path(data_dir), Path(config), service, api_url, rt)
    now = int(rt.clock())
    job_id = str(uuid.uuid4())
    job = {
        "schema": 1, "id": job_id, "created_at": now, "status": "begun",
        "release": str(root), "release_identity": _identity(root, allow_dir=True),
        "origin": origin, "marks": [],
    }
    atomic_json(state_dir / (job_id + ".json"), job)
    return {"id": job_id}


def _inside_artifact(path: Path, release: Path) -> bool:
    return path.parent == release


def _file_fingerprint(path: Path, root_device: int,
                      runtime: Runtime | None = None) -> dict[str, Any]:
    info = os.lstat(path)
    if (not _owns_root(info, runtime) or info.st_mode & (0o022 | stat.S_ISUID | stat.S_ISGID | stat.S_ISVTX)
            or stat.S_ISLNK(info.st_mode)):
        raise CleanupError("artifact contains non-root-owned, writable, or symlink entry")
    if info.st_dev != root_device:
        raise CleanupError("artifact contains a mount or cross-device entry")
    _assert_not_mountpoint(path)
    if stat.S_ISDIR(info.st_mode):
        if PROTECTED_NAMES.search(path.name):
            raise CleanupError("artifact contains a protected key or migration directory")
        return {"type": "dir", "device": info.st_dev, "inode": info.st_ino,
                "mode": stat.S_IMODE(info.st_mode)}
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise CleanupError("artifact contains an unsafe file type or hard link")
    if PROTECTED_NAMES.search(path.name):
        raise CleanupError("artifact contains a protected key, migration, or archive name")
    if not LEVELDB_NAME.fullmatch(path.name):
        raise CleanupError("artifact contains an unapproved filename")
    return {
        "type": "file", "device": info.st_dev, "inode": info.st_ino,
        "mode": stat.S_IMODE(info.st_mode), "size": info.st_size,
        "mtime_ns": info.st_mtime_ns, "sha256": _sha_file(path),
    }


def _manifest(artifact: Path, kind: str,
              runtime: Runtime | None = None) -> tuple[list[dict[str, Any]], int]:
    root_info = os.lstat(artifact)
    if (not _owns_root(root_info, runtime) or root_info.st_mode & (0o022 | stat.S_ISUID | stat.S_ISGID | stat.S_ISVTX)
            or stat.S_ISLNK(root_info.st_mode)):
        raise CleanupError("artifact root must be closed and root-owned")
    _assert_not_mountpoint(artifact)
    if kind == "candidate":
        if artifact.name != "node.candidate" or not stat.S_ISREG(root_info.st_mode) or root_info.st_nlink != 1:
            raise CleanupError("candidate must be a direct regular node.candidate file")
        if PROTECTED_NAMES.search(artifact.name):
            raise CleanupError("protected artifact name")
        return [{
            "path": ".", "type": "file", "device": root_info.st_dev, "inode": root_info.st_ino,
            "mode": stat.S_IMODE(root_info.st_mode), "size": root_info.st_size,
            "mtime_ns": root_info.st_mtime_ns, "sha256": _sha_file(artifact),
        }], root_info.st_size
    if not stat.S_ISDIR(root_info.st_mode):
        raise CleanupError("stopped-copy must be a directory")
    entries: list[dict[str, Any]] = []
    total = 0
    stack = [(artifact, ".")]
    while stack:
        directory, relative = stack.pop()
        names = sorted(os.listdir(directory))
        for name in names:
            if name in (".", "..") or "/" in name:
                raise CleanupError("invalid artifact entry name")
            child = directory / name
            rel = name if relative == "." else relative + "/" + name
            item = _file_fingerprint(child, root_info.st_dev, runtime)
            if item["type"] == "dir":
                stack.append((child, rel))
            else:
                total += item["size"]
            entries.append({"path": rel, **item})
    return sorted(entries, key=lambda item: item["path"]), total


def mark(job_id: str, artifact_text: str, kind: str, *, state_dir: Path | None = None,
         runtime: Runtime | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    _validate_job_id(job_id)
    if kind not in ("stopped-copy", "candidate"):
        raise CleanupError("invalid mark arguments")
    job = _safe_record(state_dir / (job_id + ".json"), rt)
    if job.get("id") != job_id or job.get("status") != "begun":
        raise CleanupError("only an incomplete rollout can be marked")
    release = _registered_release(job, rt)
    artifact = Path(artifact_text)
    if not artifact.is_absolute() or artifact.parent.resolve(strict=True) != release:
        raise CleanupError("artifact must be a direct release child")
    if not _inside_artifact(artifact, release) or artifact.is_symlink():
        raise CleanupError("artifact path is ambiguous")
    expected_name = "stopped-copy" if kind == "stopped-copy" else "node.candidate"
    if artifact.name != expected_name:
        raise CleanupError("artifact name does not match artifact kind")
    manifest, size = _manifest(artifact, kind, rt)
    mark_data = {
        "path": str(artifact), "kind": kind, "identity": _identity(artifact, allow_dir=(kind == "stopped-copy")),
        "root_mode": stat.S_IMODE(os.lstat(artifact).st_mode), "manifest": manifest, "bytes": size,
        "sha256": _sha_file(artifact) if kind == "candidate" else None,
    }
    if any(item["path"] == str(artifact) for item in job["marks"]):
        raise CleanupError("artifact is already registered")
    job["marks"].append(mark_data)
    atomic_json(state_dir / (job_id + ".json"), job)
    return {"id": job_id, "marked": True, "bytes": size}


def _origin_static_unchanged(origin: dict[str, Any], runtime: Runtime | None = None) -> None:
    rt = _rt(runtime)
    db = Path(origin["data_dir"]) / "chain.db"
    if _identity(db, allow_dir=True) != origin["chain_db"]:
        raise CleanupError("live chain.db identity changed")
    config = Path(origin["config"])
    if _identity(config) != origin["config_identity"] or _config_hash(config) != origin["config_sha256"]:
        raise CleanupError("live config identity or content changed")
    if _identity(db, allow_dir=True) != origin["chain_db"] or _identity(config) != origin["config_identity"]:
        raise CleanupError("live source or config identity changed during verification")
    _, genesis = _block(origin["api_url"], 0, rt)
    if genesis != origin["genesis_hash"]:
        raise CleanupError("live chain genesis changed")
    _, anchor_hash = _block(origin["api_url"], origin["height"], rt)
    if anchor_hash != origin["hash"]:
        raise CleanupError("canonical rollout origin anchor changed")


def _origin_unchanged(origin: dict[str, Any], runtime: Runtime | None = None) -> None:
    rt = _rt(runtime)
    _origin_static_unchanged(origin, rt)
    if (_service_pid(origin["service"], rt) != origin["pid"]
            or _proc_start_ticks(origin["pid"], rt) != origin["start_ticks"]):
        raise CleanupError("live service PID changed")
    if str(_process_exe(origin["pid"], rt)) != origin.get("live_exe"):
        raise CleanupError("live executable identity changed")
    _verify_process_binding(origin["pid"], Path(origin["data_dir"]), Path(origin["config"]), rt)


def complete(job_id: str, expected_binary_sha256: str, *, state_dir: Path | None = None,
             runtime: Runtime | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    _validate_job_id(job_id)
    if not HEX64.fullmatch(expected_binary_sha256):
        raise CleanupError("invalid complete arguments")
    job_path = state_dir / (job_id + ".json")
    job = _safe_record(job_path, rt)
    if job.get("id") != job_id or job.get("status") != "begun":
        raise CleanupError("job is not incomplete")
    _registered_release(job, rt)
    origin = job["origin"]
    # A rollout intentionally restarts the node. Bind completion to one stable
    # post-rollout PID/start-tick identity instead of requiring the begin PID.
    _origin_static_unchanged(origin, rt)
    live_pid = _service_pid(origin["service"], rt)
    live_ticks = _proc_start_ticks(live_pid, rt)
    _verify_process_binding(live_pid, Path(origin["data_dir"]), Path(origin["config"]), rt)
    live_exe = _process_exe(live_pid, rt)
    if not live_exe.is_absolute() or not live_exe.exists():
        raise CleanupError("post-rollout executable is unavailable")
    installed_hash = _sha_file(rt.installed_binary)
    if installed_hash != expected_binary_sha256.lower() or _sha_live_exe(live_pid, rt) != installed_hash:
        raise CleanupError("installed binary and live executable hash parity failed")
    height = _advancing_height(origin["api_url"], origin["height"], rt)
    _, tip = _block(origin["api_url"], height, rt)
    if _status(origin["api_url"], rt)["height"] < height:
        raise CleanupError("node tip regressed after readiness validation")
    _origin_static_unchanged(origin, rt)
    if (_service_pid(origin["service"], rt) != live_pid
            or _proc_start_ticks(live_pid, rt) != live_ticks
            or _process_exe(live_pid, rt) != live_exe
            or _sha_file(rt.installed_binary) != installed_hash
            or _sha_live_exe(live_pid, rt) != installed_hash):
        raise CleanupError("post-rollout process changed during completion validation")
    _verify_process_binding(live_pid, Path(origin["data_dir"]), Path(origin["config"]), rt)
    job.update({"status": "completed", "completed_at": int(rt.clock()),
                "completion_height": height, "completion_hash": tip,
                "binary_sha256": installed_hash, "runtime_pid": live_pid,
                "runtime_start_ticks": live_ticks, "runtime_exe": str(live_exe)})
    atomic_json(job_path, job)
    return {"id": job_id, "status": "completed", "height": height}


def _source_current(context: dict[str, Any], runtime: Runtime | None = None) -> None:
    origin = context["origin"]
    _origin_unchanged(origin, runtime)


def _binary_unchanged(job: dict[str, Any], runtime: Runtime | None = None) -> None:
    rt = _rt(runtime)
    expected = job.get("binary_sha256")
    if not isinstance(expected, str) or not HEX64.fullmatch(expected):
        raise CleanupError("completed job lacks its binary identity proof")
    origin = job["origin"]
    pid = job.get("runtime_pid")
    if (type(pid) is not int or _sha_file(rt.installed_binary) != expected
            or _sha_live_exe(pid, rt) != expected):
        raise CleanupError("live and installed binary identity changed")


def _completed_job_unchanged(job: dict[str, Any], runtime: Runtime | None = None) -> None:
    rt = _rt(runtime)
    origin = job["origin"]
    _origin_static_unchanged(origin, rt)
    pid, ticks = job.get("runtime_pid"), job.get("runtime_start_ticks")
    if (type(pid) is not int or type(ticks) is not int
            or _service_pid(origin["service"], rt) != pid
            or _proc_start_ticks(pid, rt) != ticks):
        raise CleanupError("completed rollout PID/start-tick identity changed")
    if str(_process_exe(pid, rt)) != job.get("runtime_exe"):
        raise CleanupError("completed rollout executable identity changed")
    _verify_process_binding(pid, Path(origin["data_dir"]), Path(origin["config"]), rt)
    _binary_unchanged(job, rt)
    _status(origin["api_url"], rt)
    if (_service_pid(origin["service"], rt) != pid
            or _proc_start_ticks(pid, rt) != ticks):
        raise CleanupError("completed rollout PID changed during scan validation")


def _remote_head(rows: list[dict[str, Any]]) -> dict[str, Any]:
    versions = [row for row in rows if row.get("action") in ("upload", "hide")]
    if not versions:
        raise CleanupError("B2 object has no immutable versions")
    newest = max(row["uploadTimestamp"] for row in versions)
    heads = [row for row in versions if row["uploadTimestamp"] == newest]
    if len(heads) != 1:
        raise CleanupError("B2 object head identity is ambiguous")
    return heads[0]


def _write_context(context_path: Path, context: dict[str, Any], anchors_path: Path, anchors: dict[str, Any]) -> None:
    atomic_json(context_path, context)
    atomic_json(anchors_path, anchors)


def _state_json_paths(state_dir: Path) -> list[Path]:
    try:
        with os.scandir(state_dir) as entries:
            paths = [
                state_dir / entry.name for entry in entries
                if JOB_FILE.fullmatch(entry.name) or CONTEXT_MARKER.fullmatch(entry.name)
            ]
    except OSError as exc:
        raise CleanupError("cannot enumerate cleanup state") from exc
    return sorted(paths)


def _invalidate_context_marker(marker_path: Path, runtime: Runtime | None = None) -> None:
    rt = _rt(runtime)
    marker = _safe_record(marker_path, rt)
    context_text = marker.get("context")
    if isinstance(context_text, str):
        context_path = Path(context_text)
        try:
            context = _read_context(context_path, rt)
            context["authorization"] = None
            context["invalidated_at"] = int(rt.clock())
            atomic_json(context_path, context)
        except (CleanupError, OSError, ValueError):
            # The stale credential file remains unusable because its state
            # marker is removed and all pin/publish commands require that marker.
            pass
    os.unlink(marker_path)


def _require_active_context(path: str | Path, state_dir: Path | None = None,
                            runtime: Runtime | None = None) -> None:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    canonical = str(Path(path).resolve(strict=True))
    for marker_path in _state_json_paths(state_dir):
        if not CONTEXT_MARKER.fullmatch(marker_path.name):
            continue
        try:
            marker = _safe_record(marker_path, rt)
        except CleanupError:
            continue
        if marker.get("valid") is True and marker.get("context") == canonical:
            return
    raise CleanupError("backup context authorization has been invalidated")


def backup_begin(data_dir: str, config: str, service: str, api_url: str,
                 anchors_output: str, context_output: str, *, state_dir: Path | None = None,
                 runtime: Runtime | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    context_path, anchors_path = Path(context_output), Path(anchors_output)
    if (not context_path.is_absolute() or not anchors_path.is_absolute()
            or context_path.resolve() == anchors_path.resolve()
            or context_path.name in ("integration-settings.json", "verified-generation.json")
            or anchors_path.name in ("integration-settings.json", "verified-generation.json")
            or context_path.is_relative_to(state_dir) or anchors_path.is_relative_to(state_dir)):
        raise CleanupError("backup outputs must be distinct absolute files outside protected state/settings")
    # Invalidates all old in-flight pin authorizations before any new work.
    with state_lock(state_dir, runtime=rt):
        for entry in _state_json_paths(state_dir):
            if CONTEXT_MARKER.fullmatch(entry.name):
                try:
                    record = _safe_record(entry, rt)
                    if record.get("valid") is True:
                        _invalidate_context_marker(entry, rt)
                except (OSError, CleanupError):
                    continue
        origin = _capture_origin(Path(data_dir), Path(config), service, api_url, rt)
        completed = []
        for path in _state_json_paths(state_dir):
            if not JOB_FILE.fullmatch(path.name):
                continue
            job = _safe_record(path, rt)
            if job.get("id") != path.stem or job.get("schema") != 1:
                raise CleanupError("rollout state identity is malformed during backup capture")
            if job.get("status") != "completed":
                continue
            marks = job.get("marks")
            if not isinstance(marks, list):
                raise CleanupError("completed rollout marks are malformed")
            pending = []
            for mark_data in marks:
                if not isinstance(mark_data, dict):
                    raise CleanupError("completed rollout mark is malformed")
                if mark_data.get("removed_at") is not None:
                    continue
                artifact_text = mark_data.get("path")
                if not isinstance(artifact_text, str) or not Path(artifact_text).is_absolute():
                    raise CleanupError("pending rollout artifact path is malformed")
                if os.path.lexists(artifact_text):
                    _revalidate_artifact(mark_data, job, rt)
                    pending.append(mark_data)
            if not pending:
                continue
            old_origin = job.get("origin")
            if not isinstance(old_origin, dict):
                raise CleanupError("pending rollout origin is malformed")
            if (old_origin.get("chain_db") != origin["chain_db"]
                    or old_origin.get("config_sha256") != origin["config_sha256"]
                    or old_origin.get("genesis_hash") != origin["genesis_hash"]):
                raise CleanupError("pending rollout source no longer matches the live backup source")
            anchor_height, anchor_hash = old_origin.get("height"), old_origin.get("hash")
            if type(anchor_height) is not int or anchor_height < 0 or not isinstance(anchor_hash, str):
                raise CleanupError("pending rollout canonical anchor is malformed")
            _, canonical_hash = _block(origin["api_url"], anchor_height, rt)
            if canonical_hash != anchor_hash:
                raise CleanupError("pending rollout origin anchor is unavailable or noncanonical")
            completed.append({"id": job["id"], "height": anchor_height, "hash": anchor_hash})
        _origin_unchanged(origin, rt)
        anchors = {"genesis_hash": origin["genesis_hash"], "anchors": completed}
        context = {
            "schema": 1, "created_at": int(rt.clock()), "origin": origin, "anchors": anchors,
            "authorization": None, "settings_fingerprint": None,
        }
        _write_context(context_path, context, anchors_path, anchors)
        _origin_unchanged(origin, rt)
        atomic_json(state_dir / ("backup-context-" + uuid.uuid4().hex + ".json"),
                    {"context": str(context_path.resolve()), "created_at": int(rt.clock()), "valid": True})
    return {"context": str(context_path), "anchors": str(anchors_path)}


def _read_context(path: str | Path, runtime: Runtime | None = None) -> dict[str, Any]:
    value = _safe_external_json(Path(path), runtime)
    if not isinstance(value, dict) or value.get("schema") != 1 or not isinstance(value.get("origin"), dict):
        raise CleanupError("invalid backup context")
    return value


def _safe_external_json(path: Path, runtime: Runtime | None = None) -> Any:
    return _read_secure_object(path, root_owned=True, runtime=runtime)


def backup_pin(context_file: str, settings_file: str, remote_object: str,
               *, state_dir: Path | None = None, runtime: Runtime | None = None,
               provider: BackupProvider | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    provider = provider or DEFAULT_PROVIDER
    if not remote_object or len(remote_object) > 1024 or remote_object.startswith("/"):
        raise CleanupError("invalid remote object name")
    context = _read_context(context_file, rt)
    _require_active_context(context_file, state_dir, rt)
    _source_current(context, rt)
    settings = read_json(Path(settings_file))
    if not isinstance(settings, dict):
        raise CleanupError("invalid integration settings")
    identity = provider.identity(settings)
    rows = provider.versions(settings, remote_object)
    chosen = _remote_head(rows)
    if (chosen.get("action") != "upload" or not isinstance(chosen.get("fileId"), str)
            or type(chosen.get("size")) is not int
            or chosen.get("bucketId") != identity["bucket_id"]
            or chosen.get("fileName") != remote_object
            or type(chosen.get("uploadTimestamp")) is not int):
        raise CleanupError("current B2 object version is hidden or has an invalid schema")
    _source_current(context, rt)
    context["authorization"] = {
        "file_id": chosen["fileId"], "file_name": remote_object,
        "file_size": chosen.get("size"), "bucket_id": identity["bucket_id"],
        "endpoint": identity["endpoint"], "bucket": identity["bucket"],
        "upload_timestamp": chosen["uploadTimestamp"],
    }
    context["settings_fingerprint"] = provider.fingerprint(settings)
    context["pin_time"] = int(rt.clock())
    _source_current(context, rt)
    atomic_json(Path(context_file), context)
    return {"pinned": True, "file_id": chosen["fileId"], "file_name": remote_object}


def backup_publish(context_file: str, proof_file: str, settings_file: str, remote_object: str,
                   archive_sha256: str, archive_size: int, tip_height: int, tip_hash: str,
                    *, state_dir: Path | None = None, runtime: Runtime | None = None,
                    provider: BackupProvider | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    provider = provider or DEFAULT_PROVIDER
    if not HEX64.fullmatch(archive_sha256) or not HEX64.fullmatch(tip_hash) or type(archive_size) is not int or archive_size < 0 or type(tip_height) is not int:
        raise CleanupError("invalid archive or tip metadata")
    context = _read_context(context_file, rt)
    _require_active_context(context_file, state_dir, rt)
    proof = read_json(Path(proof_file))
    _validate_proof(proof)
    if proof.get("genesis_hash") != context["origin"]["genesis_hash"] or proof.get("tip_height") != tip_height or proof.get("tip_hash") != tip_hash:
        raise CleanupError("backup proof does not match the requested tip")
    if proof.get("anchors") != context["anchors"]["anchors"]:
        raise CleanupError("backup proof anchors do not match captured anchors")
    auth = context.get("authorization")
    if not isinstance(auth, dict) or auth.get("file_name") != remote_object:
        raise CleanupError("no matching B2 object pin authorization")
    _source_current(context, rt)
    settings = read_json(Path(settings_file))
    identity = provider.identity(settings)
    endpoint, bucket, bucket_id = identity["endpoint"], identity["bucket"], identity["bucket_id"]
    if (provider.fingerprint(settings) != context.get("settings_fingerprint")
            or endpoint != auth.get("endpoint") or bucket != auth.get("bucket")
            or bucket_id != auth.get("bucket_id")):
        raise CleanupError("B2 provider configuration changed")
    rows = provider.versions(settings, remote_object)
    matches = [
        x for x in rows if x.get("fileId") == auth.get("file_id")
        and x.get("fileName") == remote_object and x.get("bucketId") == auth.get("bucket_id")
        and x.get("size") == archive_size and x.get("action") == "upload"
        and x.get("uploadTimestamp") == auth.get("upload_timestamp")
    ]
    if not matches:
        raise CleanupError("pinned immutable B2 file version changed or disappeared")
    latest = _remote_head(rows)
    if (latest.get("action") != "upload" or latest.get("fileId") != auth.get("file_id")
            or latest.get("bucketId") != auth.get("bucket_id")
            or latest.get("fileName") != remote_object
            or latest.get("size") != archive_size
            or latest.get("uploadTimestamp") != auth.get("upload_timestamp")):
        raise CleanupError("pinned immutable B2 object is no longer the current head")
    # Re-read the native B2 versions after all local source/proof checks. A
    # changed head invalidates this receipt even when an older fileId survives.
    after_rows = provider.versions(settings, remote_object)
    after_head = _remote_head(after_rows)
    if (after_head is None or after_head.get("action") != "upload"
            or after_head.get("fileId") != auth.get("file_id")
            or after_head.get("fileName") != remote_object
            or after_head.get("bucketId") != auth.get("bucket_id")
            or after_head.get("size") != archive_size
            or after_head.get("uploadTimestamp") != auth.get("upload_timestamp")):
        raise CleanupError("pinned immutable B2 head changed during publish validation")
    _source_current(context, rt)
    record = {
        "schema": 1, "verified_at": int(rt.clock()), "backup_height": tip_height,
        "tip_hash": tip_hash.lower(), "genesis_hash": context["origin"]["genesis_hash"],
        "anchors": proof["anchors"], "source": context["origin"]["chain_db"],
        "config_sha256": context["origin"]["config_sha256"],
        "archive_sha256": archive_sha256.lower(), "archive_size": archive_size,
        "provider_fingerprint": context["settings_fingerprint"],
        "b2": {"endpoint": endpoint, "bucket": bucket, "remote_object": remote_object,
               "file_id": auth["file_id"], "file_name": remote_object, "file_size": archive_size,
                "bucket_id": bucket_id, "upload_timestamp": auth["upload_timestamp"]},
    }
    _source_current(context, rt)
    atomic_json(Path(settings_file).parent / "verified-generation.json", record)
    return {"published": True, "backup_height": tip_height}


def _validate_proof(proof: Any) -> None:
    required = {"schema", "genesis_hash", "tip_height", "tip_hash", "anchors"}
    if not isinstance(proof, dict) or set(proof) != required or proof.get("schema") != 1:
        raise CleanupError("invalid backup proof schema")
    if (not isinstance(proof["genesis_hash"], str) or not HEX64.fullmatch(proof["genesis_hash"])
            or not isinstance(proof["tip_hash"], str) or not HEX64.fullmatch(proof["tip_hash"])
            or type(proof["tip_height"]) is not int or proof["tip_height"] < 0
            or not isinstance(proof["anchors"], list) or len(proof["anchors"]) > 100000):
        raise CleanupError("invalid backup proof tip or anchors")
    seen: set[str] = set()
    for anchor in proof["anchors"]:
        if not isinstance(anchor, dict) or set(anchor) != {"id", "height", "hash"}:
            raise CleanupError("invalid backup anchor schema")
        try:
            _validate_job_id(anchor["id"])
        except (CleanupError, TypeError):
            raise CleanupError("invalid backup anchor id")
        if (not isinstance(anchor["id"], str)
                or type(anchor["height"]) is not int or anchor["height"] < 0
                or not isinstance(anchor["hash"], str) or not HEX64.fullmatch(anchor["hash"])
                or anchor["height"] >= proof["tip_height"] or anchor["id"] in seen):
            raise CleanupError("invalid or duplicate backup anchor")
        seen.add(anchor["id"])


def backup_abort(context_file: str, *, state_dir: Path | None = None,
                 runtime: Runtime | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    path = Path(context_file)
    with state_lock(state_dir, runtime=rt):
        _require_active_context(path, state_dir, rt)
        context = _read_context(path, rt)
        context["authorization"] = None
        context["aborted_at"] = int(rt.clock())
        atomic_json(path, context)
        for entry in _state_json_paths(state_dir):
            if not CONTEXT_MARKER.fullmatch(entry.name):
                continue
            try:
                marker = _safe_record(entry, rt)
                if marker.get("context") == str(path.resolve()):
                    _invalidate_context_marker(entry, rt)
            except (OSError, CleanupError):
                continue
    return {"aborted": True}


def _proc_references(pid: int, artifact: Path, config: Path, *,
                     runtime: Runtime | None = None,
                     manifest: list[dict[str, Any]] | None = None) -> None:
    """Inspect every host process, not just the service, before deleting."""
    rt = _rt(runtime)
    proc_root = Path(rt.proc_root)
    artifact = artifact.resolve(strict=True)
    artifact_text = str(artifact)
    identities: set[tuple[int, int]] = set()
    root_info = os.lstat(artifact)
    identities.add((root_info.st_dev, root_info.st_ino))
    if manifest:
        identities.update((item["device"], item["inode"]) for item in manifest)
    try:
        declared_config = Path(config).resolve(strict=True)
        config_info = os.stat(declared_config)
    except OSError as exc:
        raise CleanupError("cannot resolve declared process config reference") from exc
    if (declared_config == artifact or artifact in declared_config.parents
            or (config_info.st_dev, config_info.st_ino) in identities):
        raise CleanupError("declared process config references cleanup artifact")

    def references_link(link: Path, cwd: Path | None = None) -> bool:
        try:
            target = os.readlink(link)
        except FileNotFoundError:
            return False
        except OSError as exc:
            raise CleanupError("cannot inspect host process references") from exc
        normalized = target.removesuffix(" (deleted)")
        if normalized == artifact_text or normalized.startswith(artifact_text + os.sep):
            return True
        try:
            info = os.stat(link)
        except FileNotFoundError:
            return normalized == artifact_text or normalized.startswith(artifact_text + os.sep)
        except OSError as exc:
            raise CleanupError("cannot resolve host process reference identity") from exc
        if (info.st_dev, info.st_ino) in identities:
            return True
        if cwd is not None:
            candidate = Path(normalized)
            if not candidate.is_absolute():
                candidate = cwd / candidate
            try:
                resolved = candidate.resolve(strict=False)
            except (OSError, RuntimeError) as exc:
                raise CleanupError("cannot resolve process command-line path") from exc
            if resolved == artifact or artifact in resolved.parents:
                return True
            try:
                candidate_info = os.stat(candidate)
            except FileNotFoundError:
                return False
            except OSError as exc:
                raise CleanupError("cannot inspect process command-line path") from exc
            if (candidate_info.st_dev, candidate_info.st_ino) in identities:
                return True
        return False

    def path_references_artifact(candidate: Path) -> bool:
        try:
            resolved = candidate.resolve(strict=False)
        except (OSError, RuntimeError) as exc:
            raise CleanupError("cannot resolve host process path reference") from exc
        if resolved == artifact or artifact in resolved.parents:
            return True
        try:
            info = os.stat(candidate)
        except FileNotFoundError:
            return False
        except OSError as exc:
            raise CleanupError("cannot inspect host process path reference") from exc
        return (info.st_dev, info.st_ino) in identities

    def config_path_values(raw: bytes) -> list[str]:
        if len(raw) > 1024 * 1024:
            raise CleanupError("host process config exceeds size limit")
        try:
            text = raw.decode("utf-8", errors="strict")
        except UnicodeDecodeError as exc:
            raise CleanupError("host process config is not UTF-8") from exc

        path_values: list[str] = []
        path_key = re.compile(
            r"(?:^|[_-])(?:path|file|dir|directory|root|source|destination)$",
            re.IGNORECASE,
        )

        def is_path_key(key: str) -> bool:
            normalized = re.sub(r"(?<=[a-z0-9])(?=[A-Z])", "_", key).replace("-", "_").lower()
            return bool(path_key.search(normalized))

        def collect_json(value: Any) -> None:
            if isinstance(value, dict):
                for key, child in value.items():
                    if isinstance(key, str) and is_path_key(key):
                        if isinstance(child, str):
                            path_values.append(child)
                        elif child is not None:
                            raise CleanupError("unsupported process config path value")
                    else:
                        collect_json(child)
            elif isinstance(value, list):
                for child in value:
                    collect_json(child)

        stripped = text.lstrip()
        if stripped.startswith(("{", "[")):
            try:
                collect_json(strict_json_bytes(raw))
            except CleanupError as exc:
                raise CleanupError("cannot safely parse host process config") from exc
            return path_values

        yaml_pair = re.compile(r"^\s*(?:-\s*)?([A-Za-z0-9_.-]+)\s*(?::|=)\s*(.*?)\s*$")
        for line in text.splitlines():
            match = yaml_pair.match(line)
            if not match or not is_path_key(match.group(1)):
                continue
            value = match.group(2).strip()
            if not value:
                continue
            if value.startswith(("{", "[", "!", "&", "*")):
                raise CleanupError("unsupported structured process config path")
            if value[0] in ("'", '"'):
                quote = value[0]
                end = value.rfind(quote)
                trailing = value[end + 1:].strip()
                if end <= 0 or (trailing and not trailing.startswith("#")):
                    raise CleanupError("unsupported quoted process config path")
                value = value[1:end]
            else:
                value = value.split("#", 1)[0].strip()
            if value and value.lower() not in ("null", "none", "false", "~"):
                path_values.append(value)
        return path_values

    config_files_checked = 0
    config_bytes_checked = 0

    def inspect_config_file(candidate: Path, process_cwd: Path | None) -> bool:
        nonlocal config_files_checked, config_bytes_checked
        config_files_checked += 1
        if config_files_checked > 128:
            raise CleanupError("host process config reference count exceeds bound")
        try:
            resolved_config = candidate.resolve(strict=True)
            info = os.stat(resolved_config)
            if not stat.S_ISREG(info.st_mode):
                raise CleanupError("host process config is not a regular file")
            if (resolved_config == artifact or artifact in resolved_config.parents
                    or (info.st_dev, info.st_ino) in identities):
                return True
            fd = os.open(
                resolved_config,
                os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) |
                getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NONBLOCK", 0),
            )
        except CleanupError:
            raise
        except (OSError, RuntimeError) as exc:
            raise CleanupError("cannot safely resolve host process config") from exc
        try:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or before.st_size > 1024 * 1024:
                raise CleanupError("host process config is unsafe or oversized")
            if (before.st_dev, before.st_ino) in identities:
                return True
            chunks = []
            total = 0
            while True:
                chunk = os.read(fd, min(65536, 1024 * 1024 + 1 - total))
                if not chunk:
                    break
                chunks.append(chunk)
                total += len(chunk)
                if total > 1024 * 1024:
                    raise CleanupError("host process config exceeds size limit")
            after = os.fstat(fd)
            config_bytes_checked += total
            if config_bytes_checked > 16 * 1024 * 1024:
                raise CleanupError("host process config bytes exceed scan bound")
            if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (
                    after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns):
                raise CleanupError("host process config changed during reference scan")
            values = config_path_values(b"".join(chunks))
        finally:
            os.close(fd)

        for value in values:
            if "://" in value or "$" in value or value.startswith("~"):
                raise CleanupError("cannot safely resolve host process config path")
            value_path = Path(value)
            if value_path.is_absolute():
                candidates = [process_dir / "root" / value.lstrip(os.sep)]
            elif process_cwd is not None:
                candidates = [resolved_config.parent / value_path]
                candidates.append(process_cwd / value_path)
            else:
                candidates = [resolved_config.parent / value_path]
            for path in candidates:
                if path_references_artifact(path):
                    return True
        return False

    try:
        with os.scandir(proc_root) as processes:
            process_ids = sorted(int(item.name) for item in processes if item.name.isdecimal())
    except OSError as exc:
        raise CleanupError("cannot enumerate host processes") from exc
    scanned_expected = False
    for process_id in process_ids:
        process_dir = proc_root / str(process_id)

        def process_vanished() -> bool:
            try:
                os.stat(process_dir)
                return False
            except FileNotFoundError:
                return True
            except OSError as exc:
                raise CleanupError("cannot inspect host process directory") from exc

        try:
            start_before = _proc_start_ticks(process_id, rt)
        except CleanupError as exc:
            if process_vanished():
                continue
            raise CleanupError("cannot establish host process identity") from exc
        if process_id == pid:
            scanned_expected = True
        try:
            stat_text = (process_dir / "stat").read_text()
            close_paren = stat_text.rfind(")")
            if close_paren < 0:
                raise CleanupError("malformed host process stat")
            stat_tail = stat_text[close_paren + 2:].split()
            if len(stat_tail) <= 6:
                raise CleanupError("malformed host process stat")
            try:
                process_flags = int(stat_tail[6])
            except ValueError as exc:
                raise CleanupError("malformed host process flags") from exc
            if process_flags < 0:
                raise CleanupError("malformed host process flags")
            if stat_tail[0] == "Z":
                continue
            cmdline_raw = (process_dir / "cmdline").read_bytes()
            kernel_thread = bool(process_flags & PF_KTHREAD)
            if not cmdline_raw and not kernel_thread:
                raise CleanupError("host process command line is unexpectedly empty")
            cwd = None
            cwd_target = None
            cwd_identity = None
            process_cwd = None
            links = []
            if not kernel_thread:
                cwd_target = os.readlink(process_dir / "cwd")
                if cwd_target.endswith(" (deleted)") or not Path(cwd_target).is_absolute():
                    raise CleanupError("host process cwd is deleted or ambiguous")
                cwd_info = os.stat(process_dir / "cwd")
                if os.readlink(process_dir / "cwd") != cwd_target:
                    raise CleanupError("host process cwd changed during capture")
                cwd_identity = (cwd_info.st_dev, cwd_info.st_ino)
                cwd = Path(cwd_target)
                process_cwd = process_dir / "root" / cwd_target.lstrip(os.sep)
                links.extend((process_dir / "cwd", process_dir / "exe"))
            fd_dir = process_dir / "fd"
            with os.scandir(fd_dir) as descriptors:
                links.extend(fd_dir / descriptor.name for descriptor in descriptors)
            cmdline = [
                item.decode("utf-8", errors="strict")
                for item in cmdline_raw.split(b"\0") if item
            ]
        except FileNotFoundError:
            # A process can exit between enumeration and inspection. Verify it
            # actually vanished; otherwise the unreadable reference is fatal.
            try:
                _proc_start_ticks(process_id, rt)
            except CleanupError:
                if process_vanished():
                    continue
            raise CleanupError("host process disappeared during reference inspection")
        except (OSError, UnicodeDecodeError) as exc:
            if process_vanished():
                continue
            raise CleanupError("cannot completely inspect host process references") from exc
        for link in links:
            if references_link(link):
                raise CleanupError("host process references cleanup artifact")
        index = 0
        while index < len(cmdline):
            argument = cmdline[index]
            value = argument
            is_config_path = False
            is_declared_path = False
            if argument in ("--config", "-config", "--config-file"):
                index += 1
                if index >= len(cmdline):
                    raise CleanupError("malformed host process path argument")
                value = cmdline[index]
                is_config_path = True
                is_declared_path = True
            elif argument in ("--data-dir", "--datadir"):
                index += 1
                if index >= len(cmdline):
                    raise CleanupError("malformed host process path argument")
                value = cmdline[index]
                is_declared_path = True
            elif argument.startswith(("--config=", "--config-file=")):
                value = argument.split("=", 1)[1]
                is_config_path = True
                is_declared_path = True
            elif argument.startswith(("--data-dir=", "--datadir=")):
                value = argument.split("=", 1)[1]
                is_declared_path = True
            if is_config_path and (not value or "://" in value):
                raise CleanupError("cannot safely resolve host process config argument")
            if value and (is_declared_path or (not value.startswith("-") and "://" not in value)):
                path_value = Path(value)
                if path_value.is_absolute():
                    candidate = process_dir / "root" / value.lstrip(os.sep)
                elif process_cwd is not None:
                    candidate = process_cwd / path_value
                else:
                    candidate = path_value
                if path_references_artifact(candidate):
                    raise CleanupError("host process command line references cleanup artifact")
                if is_config_path and inspect_config_file(candidate, process_cwd):
                    raise CleanupError("host process config references cleanup artifact")
            index += 1
        if process_cwd is not None:
            try:
                current_target = os.readlink(process_dir / "cwd")
                current_info = os.stat(process_dir / "cwd")
            except OSError as exc:
                raise CleanupError("cannot recheck host process cwd") from exc
            if (current_target != cwd_target
                    or (current_info.st_dev, current_info.st_ino) != cwd_identity):
                raise CleanupError("host process cwd changed during reference inspection")
        try:
            start_after = _proc_start_ticks(process_id, rt)
        except CleanupError as exc:
            if process_vanished():
                continue
            raise CleanupError("cannot recheck host process identity") from exc
        if start_before != start_after:
            raise CleanupError("host process PID changed during reference inspection")
    if not scanned_expected:
        raise CleanupError("pinned node process vanished during reference inspection")


def _receipt(settings_path: Path, job: dict[str, Any], mark_data: dict[str, Any], *,
             runtime: Runtime | None = None,
             provider: BackupProvider | None = None) -> tuple[dict[str, Any], str]:
    rt = _rt(runtime)
    provider = provider or DEFAULT_PROVIDER
    receipt_path = settings_path.parent / "verified-generation.json"
    try:
        receipt = _safe_external_json(receipt_path, rt)
    except CleanupError as exc:
        if not os.path.lexists(receipt_path):
            raise CleanupError("no valid verified-generation receipt") from exc
        raise CleanupError("verified-generation receipt is malformed or unsafe") from exc
    if not isinstance(receipt, dict) or receipt.get("schema") != 1:
        raise CleanupError("no valid verified-generation receipt")
    now = rt.clock()
    if (type(receipt.get("verified_at")) is not int or receipt["verified_at"] > now + 30
            or now - receipt["verified_at"] > 48 * 3600
            or receipt["verified_at"] <= job["completed_at"]):
        raise CleanupError("verified backup is stale or predates rollout completion")
    if (type(receipt.get("backup_height")) is not int or receipt["backup_height"] < 0
            or receipt["backup_height"] <= job["completion_height"]):
        raise CleanupError("verified backup is not newer than rollout completion")
    origin = job["origin"]
    if receipt.get("genesis_hash") != origin["genesis_hash"] or receipt.get("source") != origin["chain_db"] or receipt.get("config_sha256") != origin["config_sha256"]:
        raise CleanupError("backup receipt source anchor does not match rollout")
    anchor = {"id": job["id"], "height": origin["height"], "hash": origin["hash"]}
    if anchor not in receipt.get("anchors", []):
        raise CleanupError("verified backup proof omits this rollout anchor")
    b2 = receipt.get("b2")
    if (not isinstance(b2, dict) or not isinstance(b2.get("file_id"), str)
            or not b2.get("file_id") or type(b2.get("file_size")) is not int
            or b2["file_size"] < 0):
        raise CleanupError("backup receipt lacks an immutable B2 fileId")
    if (not isinstance(receipt.get("archive_sha256"), str)
            or not HEX64.fullmatch(receipt["archive_sha256"])
            or type(receipt.get("archive_size")) is not int or receipt["archive_size"] < 0
            or b2.get("file_size") != receipt.get("archive_size")
            or type(b2.get("upload_timestamp")) is not int or b2["upload_timestamp"] < 0):
        raise CleanupError("backup receipt archive or B2 version metadata is invalid")
    settings = read_json(settings_path)
    if not isinstance(settings, dict):
        raise CleanupError("integration settings invalid")
    identity = provider.identity(settings)
    endpoint, bucket = identity["endpoint"], identity["bucket"]
    if provider.fingerprint(settings) != receipt.get("provider_fingerprint"):
        raise CleanupError("backup provider configuration changed")
    if (endpoint != b2.get("endpoint") or bucket != b2.get("bucket")
            or identity["bucket_id"] != b2.get("bucket_id")):
        raise CleanupError("receipt B2 bucket or endpoint changed")
    remote_object = b2.get("remote_object")
    if not isinstance(remote_object, str) or b2.get("file_name") != remote_object:
        raise CleanupError("receipt B2 object name is invalid")
    rows = provider.versions(settings, remote_object)
    if not any(row.get("fileId") == b2["file_id"] and row.get("fileName") == b2["file_name"]
               and row.get("bucketId") == b2["bucket_id"] and row.get("size") == b2["file_size"]
               and row.get("action") == "upload"
               and row.get("uploadTimestamp") == b2["upload_timestamp"]
               for row in rows):
        raise CleanupError("immutable B2 backup fileId is no longer present")
    return receipt, endpoint


def _revalidate_artifact(mark_data: dict[str, Any], job: dict[str, Any],
                         runtime: Runtime | None = None) -> int:
    artifact = Path(mark_data["path"])
    release = _registered_release(job, runtime)
    if artifact.parent != release or artifact.is_symlink():
        raise CleanupError("artifact moved or escaped its registered release")
    if _identity(artifact, allow_dir=(mark_data["kind"] == "stopped-copy")) != mark_data["identity"]:
        raise CleanupError("artifact inode changed")
    if stat.S_IMODE(os.lstat(artifact).st_mode) != mark_data["root_mode"]:
        raise CleanupError("artifact root permissions changed")
    manifest, size = _manifest(artifact, mark_data["kind"], runtime)
    if manifest != mark_data["manifest"]:
        raise CleanupError("artifact fingerprint changed after registration")
    return size


def _delete_manifest(mark_data: dict[str, Any], runtime: Runtime | None = None) -> int:
    """Unlink the already revalidated allow-listed tree without recursive APIs."""
    artifact = Path(mark_data["path"])
    if mark_data["kind"] == "candidate":
        _assert_not_mountpoint(artifact)
        parentfd = os.open(artifact.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            fd = os.open(artifact.name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parentfd)
            try:
                _verify_open_file(fd, mark_data["manifest"][0], runtime)
                opened = os.fstat(fd)
                current = os.stat(artifact.name, dir_fd=parentfd, follow_symlinks=False)
                if (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino):
                    raise CleanupError("candidate changed during descriptor-relative delete")
            finally:
                os.close(fd)
            os.unlink(artifact.name, dir_fd=parentfd)
            os.fsync(parentfd)
        finally:
            os.close(parentfd)
        return mark_data["bytes"]
    # Direct descriptor-relative traversal; check every identity immediately before unlink.
    rootfd = os.open(artifact, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    root_info = os.fstat(rootfd)
    if (not _owns_root(root_info, runtime)
            or (root_info.st_dev, root_info.st_ino) != (
        mark_data["identity"]["device"], mark_data["identity"]["inode"]
    ) or stat.S_IMODE(root_info.st_mode) != mark_data["root_mode"]
            or root_info.st_mode & 0o022):
        os.close(rootfd)
        raise CleanupError("artifact root changed during deletion")
    manifest_by_path = {item["path"]: item for item in mark_data["manifest"]}
    try:
        for item in sorted(mark_data["manifest"], key=lambda x: (x["path"].count("/"), x["path"]), reverse=True):
            if item["path"] == ".":
                continue
            parent_rel, _, name = item["path"].rpartition("/")
            _assert_not_mountpoint(artifact / item["path"])
            parentfd = os.dup(rootfd) if not parent_rel else _open_dir_chain(
                rootfd, parent_rel.split("/"), manifest_by_path, runtime
            )
            try:
                st = os.stat(name, dir_fd=parentfd, follow_symlinks=False)
                if st.st_dev != item["device"] or st.st_ino != item["inode"]:
                    raise CleanupError("artifact changed during descriptor-relative delete")
                if item["type"] == "dir":
                    if (not stat.S_ISDIR(st.st_mode) or not _owns_root(st, runtime)
                            or stat.S_IMODE(st.st_mode) != item["mode"] or st.st_mode & 0o022):
                        raise CleanupError("artifact directory changed during deletion")
                    childfd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parentfd)
                    try:
                        if os.listdir(childfd):
                            raise CleanupError("artifact directory gained unmarked contents")
                    finally:
                        os.close(childfd)
                    os.rmdir(name, dir_fd=parentfd)
                else:
                    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parentfd)
                    try:
                        _verify_open_file(fd, item, runtime)
                        opened = os.fstat(fd)
                        current = os.stat(name, dir_fd=parentfd, follow_symlinks=False)
                        if (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino):
                            raise CleanupError("artifact file changed during descriptor-relative delete")
                    finally:
                        os.close(fd)
                    os.unlink(name, dir_fd=parentfd)
                os.fsync(parentfd)
            finally:
                os.close(parentfd)
        parentfd = os.open(artifact.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            st = os.stat(artifact.name, dir_fd=parentfd, follow_symlinks=False)
            if st.st_ino != mark_data["identity"]["inode"] or st.st_dev != mark_data["identity"]["device"]:
                raise CleanupError("artifact root changed during deletion")
            if not stat.S_ISDIR(st.st_mode) or not _owns_root(st, runtime) or st.st_mode & 0o022:
                raise CleanupError("artifact root changed during deletion")
            if stat.S_IMODE(st.st_mode) != mark_data["root_mode"]:
                raise CleanupError("artifact root permissions changed during deletion")
            if os.listdir(rootfd):
                raise CleanupError("artifact root gained unmarked contents")
            os.close(rootfd)
            rootfd = -1
            os.rmdir(artifact.name, dir_fd=parentfd)
            os.fsync(parentfd)
        finally:
            os.close(parentfd)
    finally:
        if rootfd >= 0:
            os.close(rootfd)
    return mark_data["bytes"]


def _verify_open_file(fd: int, expected: dict[str, Any],
                      runtime: Runtime | None = None) -> None:
    before = os.fstat(fd)
    if (not stat.S_ISREG(before.st_mode) or not _owns_root(before, runtime) or before.st_nlink != 1
            or before.st_mode & 0o022 or before.st_dev != expected["device"]
            or before.st_ino != expected["inode"] or before.st_size != expected["size"]
            or before.st_mtime_ns != expected["mtime_ns"]):
        raise CleanupError("artifact file changed during deletion")
    digest = hashlib.sha256()
    os.lseek(fd, 0, os.SEEK_SET)
    while True:
        chunk = os.read(fd, 1024 * 1024)
        if not chunk:
            break
        digest.update(chunk)
    after = os.fstat(fd)
    if ((after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) !=
            (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
            or digest.hexdigest() != expected["sha256"]):
        raise CleanupError("artifact file fingerprint changed during deletion")


def _open_dir_chain(rootfd: int, components: list[str], manifest: dict[str, dict[str, Any]],
                    runtime: Runtime | None = None) -> int:
    current = os.dup(rootfd)
    traversed: list[str] = []
    try:
        for component in components:
            nxt = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=current)
            traversed.append(component)
            info = os.fstat(nxt)
            expected = manifest.get("/".join(traversed))
            if (expected is None or not stat.S_ISDIR(info.st_mode) or not _owns_root(info, runtime)
                    or stat.S_IMODE(info.st_mode) != expected["mode"] or info.st_mode & 0o022
                    or (info.st_dev, info.st_ino) != (expected["device"], expected["inode"])):
                os.close(nxt)
                raise CleanupError("artifact directory changed during descriptor-relative deletion")
            os.close(current)
            current = nxt
        return current
    except Exception:
        os.close(current)
        raise


def scan(apply: bool = False, *, state_dir: Path | None = None,
         settings_path: Path | None = None, runtime: Runtime | None = None,
         provider: BackupProvider | None = None) -> dict[str, Any]:
    rt = _rt(runtime)
    state_dir = Path(state_dir or rt.state_dir)
    settings_path = Path(settings_path or rt.settings_file)
    provider = provider or DEFAULT_PROVIDER
    report: dict[str, Any] = {"mode": "apply" if apply else "dry-run", "artifacts": [], "bytes": 0}
    with state_lock(state_dir, runtime=rt):
        for path in _state_json_paths(state_dir):
            if not JOB_FILE.fullmatch(path.name):
                continue
            try:
                job = _safe_record(path, rt)
            except CleanupError:
                continue
            if (job.get("schema") != 1 or job.get("status") != "completed"
                    or job.get("id") != path.stem):
                continue
            for mark_data in job.get("marks", []):
                if mark_data.get("removed_at") is not None:
                    continue
                row = {"id": job.get("id"), "path": mark_data.get("path"), "bytes": 0,
                       "size_bytes": 0, "eligible": False, "removed": False, "reasons": []}
                try:
                    if rt.clock() - job["completed_at"] > 48 * 3600:
                        raise CleanupError("completed rollout is older than 48 hours")
                    size = _revalidate_artifact(mark_data, job, rt)
                    row["size_bytes"] = size
                    origin = job["origin"]
                    _completed_job_unchanged(job, rt)
                    _proc_references(job["runtime_pid"], Path(mark_data["path"]), Path(origin["config"]),
                                     runtime=rt, manifest=mark_data["manifest"])
                    _receipt(settings_path, job, mark_data, runtime=rt, provider=provider)
                    # Repeat all mutable checks immediately before unlink.
                    _completed_job_unchanged(job, rt)
                    _proc_references(job["runtime_pid"], Path(mark_data["path"]), Path(origin["config"]),
                                     runtime=rt, manifest=mark_data["manifest"])
                    _receipt(settings_path, job, mark_data, runtime=rt, provider=provider)
                    _completed_job_unchanged(job, rt)
                    size = _revalidate_artifact(mark_data, job, rt)
                    # Hashing a large stopped database can take time. Inspect
                    # references and live identity again after that read.
                    _proc_references(job["runtime_pid"], Path(mark_data["path"]), Path(origin["config"]),
                                     runtime=rt, manifest=mark_data["manifest"])
                    _completed_job_unchanged(job, rt)
                    row["bytes"] = size
                    row["eligible"] = True
                    if apply:
                        row["bytes"] = _delete_manifest(mark_data, rt)
                        row["removed"] = True
                        report["bytes"] += row["bytes"]
                        mark_data["removed_at"] = int(rt.clock())
                        atomic_json(path, job)
                    else:
                        report["bytes"] += row["bytes"]
                except (CleanupError, OSError, KeyError, TypeError, ValueError) as exc:
                    row["reasons"].append(str(exc) if isinstance(exc, CleanupError) else "filesystem or record validation failed")
                report["artifacts"].append(row)
    return report


def _validate_job_id(value: str) -> None:
    try:
        valid = UUID_RE.fullmatch(value) and str(uuid.UUID(value)) == value.lower()
    except (ValueError, AttributeError):
        valid = False
    if not valid:
        raise CleanupError("invalid rollout id")