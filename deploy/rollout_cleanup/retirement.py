"""Explicit archive-before-delete retirement of historical release data."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import urllib.parse
import urllib.request
import uuid
from pathlib import Path
from typing import Any

from . import (
    CleanupError, _assert_not_mountpoint, _assert_root_dir, _capture_origin,
    _delete_manifest, _file_fingerprint, _identity,
    _manifest, _origin_unchanged, _proc_references, _read_secure_object, _release_path, _rt,
    _safe_record, _sha_file, _owns_root, atomic_json, state_lock,
)
from .provider import NativeB2Provider

RETIREMENT_FILE = re.compile(r"^retirement-[0-9a-fA-F-]{36}\.json$")
SNAPSHOT_FILE = re.compile(r"^snapshot[^/]*\.gz(?:\.sha256)?$")
MAX_ARCHIVE_SIZE = 8 * 1024 * 1024 * 1024
MAX_MEMBER_SIZE = 4 * 1024 * 1024 * 1024
MIN_FREE_SPACE = 5 * 1024 * 1024 * 1024


def _retirement_path(state_dir: Path, retirement_id: str) -> Path:
    if not re.fullmatch(r"[0-9a-fA-F-]{36}", retirement_id):
        raise CleanupError("invalid retirement id")
    return state_dir / ("retirement-" + retirement_id + ".json")


def _closed_historical_path(release: Path, runtime: Any) -> tuple[Path, Path, Path]:
    """Validate the only accepted historical target and each parent component."""
    root = Path(release)
    if not root.is_absolute() or root.is_symlink():
        raise CleanupError("release path must be an absolute real directory")
    root = root.resolve(strict=True)
    if root.parent != Path(runtime.releases_dir).resolve(strict=True):
        raise CleanupError("release must be a direct child of the allowed releases directory")
    root = _release_path(str(root), runtime=runtime)
    _assert_not_mountpoint(root)
    rollback = root / "rollback"
    stopped = rollback / "stopped-data"
    for parent in (rollback, stopped):
        info = os.lstat(parent)
        if (not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode)
                or not _owns_root(info, runtime) or info.st_mode & 0o022):
            raise CleanupError("historical parent directories must be closed root-owned directories")
        if info.st_dev != os.lstat(root).st_dev:
            raise CleanupError("historical parent contains a mount or cross-device directory")
        _assert_not_mountpoint(parent)
    return root, rollback, stopped


def _targets(stopped: Path, runtime: Any) -> list[dict[str, Any]]:
    """Capture only chain.db and top-level snapshot files; never neighboring data."""
    targets: list[dict[str, Any]] = []
    db = stopped / "chain.db"
    db_info = os.lstat(db)
    if not stat.S_ISDIR(db_info.st_mode) or stat.S_ISLNK(db_info.st_mode):
        raise CleanupError("historical chain.db must be a real directory")
    entries, size = _manifest(db, "stopped-copy", runtime, allow_current_bak=True)
    targets.append({
        "kind": "stopped-copy", "path": str(db),
        "identity": _identity(db, allow_dir=True),
        "root_mode": stat.S_IMODE(db_info.st_mode),
        "manifest": entries, "bytes": size,
    })
    for path in _snapshot_paths(stopped):
        name = path.name
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode):
            raise CleanupError("historical snapshot must be a regular file")
        fingerprint = _file_fingerprint(path, os.lstat(stopped).st_dev, runtime)
        # The candidate deletion adapter deliberately uses a single "." entry.
        entry = {"path": ".", **fingerprint}
        targets.append({
            "kind": "candidate", "path": str(path),
            "identity": _identity(path), "root_mode": stat.S_IMODE(info.st_mode),
            "manifest": [entry], "bytes": fingerprint["size"],
        })
    if len(targets) == 1 and not entries:
        raise CleanupError("historical retirement selection is empty")
    return targets


def _snapshot_paths(stopped: Path) -> list[Path]:
    names = set(os.listdir(stopped))
    archives = {name for name in names
                if name.endswith(".gz") and SNAPSHOT_FILE.fullmatch(name)}
    selected = set(archives)
    selected.update(name + ".sha256" for name in archives if name + ".sha256" in names)
    return [stopped / name for name in sorted(selected)]


def _archive_manifest(targets: list[dict[str, Any]]) -> list[dict[str, Any]]:
    members: list[dict[str, Any]] = []
    for target in targets:
        base = Path(target["path"]).name
        if target["kind"] == "candidate":
            item = target["manifest"][0]
            members.append({
                "path": base, "type": "file", "size": item["size"],
                "sha256": item["sha256"],
            })
            continue
        members.append({"path": base, "type": "dir", "size": 0, "sha256": None})
        for item in target["manifest"]:
            if item["path"] == ".":
                continue
            members.append({
                "path": base + "/" + item["path"], "type": item["type"],
                "size": item.get("size", 0), "sha256": item.get("sha256"),
            })
    return sorted(members, key=lambda row: row["path"])


def _manifest_hash(members: list[dict[str, Any]]) -> str:
    raw = json.dumps(members, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()
    return hashlib.sha256(raw).hexdigest()


def _ensure_staging_headroom(work: Path, targets: list[dict[str, Any]]) -> None:
    source_bytes = sum(target.get("bytes", 0) for target in targets)
    if any(type(target.get("bytes")) is not int or target["bytes"] < 0 for target in targets):
        raise CleanupError("invalid historical source size")
    required = source_bytes * 4 + MIN_FREE_SPACE
    if shutil.disk_usage(work).free < required:
        raise CleanupError("insufficient staging disk headroom for historical archive")


def _safe_tar_name(name: str) -> bool:
    path = Path(name)
    return (not path.is_absolute() and bool(name) and all(part not in ("", ".", "..") for part in name.split("/"))
            and "\\" not in name)


def _write_archive(targets: list[dict[str, Any]], archive_path: Path) -> None:
    with tarfile.open(archive_path, "w:gz", compresslevel=1, format=tarfile.PAX_FORMAT) as tar:
        for target in targets:
            path = Path(target["path"])
            if target["kind"] == "candidate":
                tar.add(path, arcname=path.name, recursive=False)
            else:
                tar.add(path, arcname=path.name, recursive=True)


def _validate_archive(archive_path: Path, members: list[dict[str, Any]]) -> None:
    expected = {entry["path"]: entry for entry in members}
    seen: set[str] = set()
    count = 0
    try:
        with tarfile.open(archive_path, mode="r|gz") as tar:
            for member in tar:
                count += 1
                if count > len(expected):
                    raise CleanupError("archive contains unexpected members")
                name = member.name.rstrip("/") if member.isdir() else member.name
                if not _safe_tar_name(name) or name in seen or name not in expected:
                    raise CleanupError("archive contains duplicate or unexpected member")
                seen.add(name)
                item = expected[name]
                if item["type"] == "dir":
                    if not member.isdir() or member.issym() or member.islnk():
                        raise CleanupError("archive directory member has an unsafe type")
                    if member.size != 0:
                        raise CleanupError("archive directory has unexpected data")
                    continue
                if not member.isfile() or member.issym() or member.islnk() or member.size != item["size"]:
                    raise CleanupError("archive file member type or size mismatch")
                if member.size > MAX_MEMBER_SIZE:
                    raise CleanupError("archive member exceeds size bound")
                stream = tar.extractfile(member)
                if stream is None:
                    raise CleanupError("archive member cannot be read")
                digest = hashlib.sha256()
                size = 0
                while True:
                    chunk = stream.read(1024 * 1024)
                    if not chunk:
                        break
                    size += len(chunk)
                    if size > item["size"]:
                        raise CleanupError("archive member exceeds declared size")
                    digest.update(chunk)
                if size != item["size"] or digest.hexdigest() != item["sha256"]:
                    raise CleanupError("archive member content mismatch")
        if seen != set(expected):
            raise CleanupError("archive is truncated or missing expected members")
    except (tarfile.TarError, EOFError, OSError) as exc:
        raise CleanupError("archive is truncated or invalid") from exc


def _run_gpg_encrypt(source: Path, destination: Path, password: bytes) -> None:
    if not password:
        raise CleanupError("APEROD_BACKUP_PASSWORD is required to archive historical data")
    homedir = destination.parent / "gnupg"
    homedir.mkdir(mode=0o700, exist_ok=True)
    os.chmod(homedir, 0o700)
    try:
        result = subprocess.run(
            ["gpg", "--no-options", "--no-symkey-cache", "--homedir", str(homedir),
             "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase-fd", "0",
             "--symmetric", "--cipher-algo", "AES256", "--compress-algo", "none",
             "--output", str(destination), str(source)],
            input=password + b"\n", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            timeout=600, check=False,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise CleanupError("GPG historical archive encryption failed") from exc
    if result.returncode != 0 or not destination.is_file():
        raise CleanupError("GPG historical archive encryption failed")


def _run_gpg_decrypt(source: Path, destination: Path, password: bytes) -> None:
    homedir = destination.parent / "gnupg"
    homedir.mkdir(mode=0o700, exist_ok=True)
    os.chmod(homedir, 0o700)
    try:
        result = subprocess.run(
            ["gpg", "--no-options", "--no-symkey-cache", "--homedir", str(homedir),
             "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase-fd", "0",
             "--compress-algo", "none", "--decrypt", "--output", str(destination), str(source)],
            input=password + b"\n", stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            timeout=600, check=False,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise CleanupError("GPG historical archive readback failed") from exc
    if result.returncode != 0 or not destination.is_file():
        raise CleanupError("GPG historical archive readback failed")


def _settings(runtime: Any) -> dict[str, Any]:
    # The live API legitimately owns this mode-0600 configuration. Root-owned
    # authorization receipts remain a separate, stricter trust boundary.
    value = _read_secure_object(Path(runtime.settings_file), root_owned=False, runtime=runtime)
    if not isinstance(value, dict):
        raise CleanupError("integration settings are unsupported")
    return value


class _RetirementB2Provider(NativeB2Provider):
    """Strict one-version B2 readback; no S3 GET or redirect/proxy fallback."""

    @classmethod
    def _authorized_download(cls, settings: dict[str, Any], bucket_id: str,
                             object_name: str) -> tuple[str, str]:
        import base64

        _, bucket, access, secret = cls._configuration(settings)
        auth = cls._request(cls.AUTH_URL, basic=base64.b64encode(
            (access + ":" + secret).encode()).decode("ascii"))
        api_url, token, download_url = (
            auth.get("apiUrl"), auth.get("authorizationToken"), auth.get("downloadUrl")
        )
        if not all(isinstance(value, str) for value in (api_url, token, download_url)):
            raise CleanupError("incomplete B2 download authorization")
        for value in (api_url, download_url):
            parsed = urllib.parse.urlsplit(value)
            if (parsed.scheme != "https" or not cls._is_b2_host(parsed.hostname)
                    or parsed.port not in (None, 443) or parsed.username is not None
                    or parsed.password is not None or parsed.query or parsed.fragment):
                raise CleanupError("untrusted B2 download host")
        listed = cls._request(api_url.rstrip("/") + "/b2api/v2/b2_list_buckets", token, {
            "accountId": auth.get("accountId"), "bucketName": bucket,
        })
        rows = listed.get("buckets")
        if (not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict)
                or rows[0].get("bucketId") != bucket_id
                or rows[0].get("bucketName") != bucket):
            raise CleanupError("configured B2 bucket identity changed")
        rules = rows[0].get("lifecycleRules")
        if not isinstance(rules, list):
            raise CleanupError("cannot inspect B2 bucket lifecycle rules")
        namespace = "historical-retirement/"
        for rule in rules:
            if not isinstance(rule, dict) or not isinstance(rule.get("fileNamePrefix"), str):
                raise CleanupError("cannot safely inspect B2 lifecycle rule")
            prefix = rule["fileNamePrefix"]
            if prefix.startswith(namespace) or namespace.startswith(prefix):
                raise CleanupError("B2 lifecycle rule may expire historical-retirement objects")
        # b2_download_file_by_id requires the account authorization token.
        # Scoped download-authorization tokens are for name-based downloads.
        return download_url.rstrip("/"), token

    @classmethod
    def download_file_id(cls, settings: dict[str, Any], bucket_id: str,
                         object_name: str, file_id: str, output: Path, expected_size: int) -> None:
        download_url, token = cls._authorized_download(settings, bucket_id, object_name)
        query = urllib.parse.urlencode({"fileId": file_id})
        url = download_url + "/b2api/v2/b2_download_file_by_id?" + query
        parsed = urllib.parse.urlsplit(url)
        host = (parsed.hostname or "").lower().rstrip(".")
        if (parsed.scheme != "https" or not host.endswith(".backblazeb2.com")
                or host == "backblazeb2.com" or parsed.port not in (None, 443)):
            raise CleanupError("untrusted B2 download URL")
        request = urllib.request.Request(url, headers={"Authorization": token})

        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None

        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
            with opener.open(request, timeout=30) as response, output.open("xb") as destination:
                final = urllib.parse.urlsplit(response.geturl())
                final_host = (final.hostname or "").lower().rstrip(".")
                if final.scheme != "https" or not final_host.endswith(".backblazeb2.com"):
                    raise CleanupError("B2 download redirected to an untrusted host")
                length = response.headers.get("Content-Length")
                if length is not None and (not length.isdigit() or int(length) != expected_size):
                    raise CleanupError("B2 historical archive size mismatch")
                total = 0
                while True:
                    block = response.read(1024 * 1024)
                    if not block:
                        break
                    total += len(block)
                    if total > expected_size or total > MAX_ARCHIVE_SIZE:
                        raise CleanupError("B2 historical archive exceeds expected size")
                    destination.write(block)
                destination.flush()
                os.fsync(destination.fileno())
        except CleanupError:
            raise
        except (OSError, TimeoutError) as exc:
            raise CleanupError("B2 immutable historical archive download failed") from exc
        if total != expected_size:
            raise CleanupError("B2 historical archive size mismatch")


def _lifecycle_and_identity(settings: dict[str, Any]) -> dict[str, str]:
    provider = _RetirementB2Provider()
    endpoint, bucket, access, secret = provider._configuration(settings)
    import base64
    auth = provider._request(provider.AUTH_URL, basic=base64.b64encode(
        (access + ":" + secret).encode()).decode("ascii"))
    api_url, token, account_id = (
        auth.get("apiUrl"), auth.get("authorizationToken"), auth.get("accountId")
    )
    if not all(isinstance(value, str) and value for value in (api_url, token, account_id)):
        raise CleanupError("incomplete B2 authorization response")
    parsed = urllib.parse.urlsplit(api_url)
    if (parsed.scheme != "https" or not provider._is_b2_host(parsed.hostname)
            or parsed.port not in (None, 443) or parsed.username is not None
            or parsed.password is not None or parsed.path not in ("", "/")
            or parsed.query or parsed.fragment):
        raise CleanupError("untrusted B2 API URL")
    listed = provider._request(api_url.rstrip("/") + "/b2api/v2/b2_list_buckets", token, {
        "accountId": account_id, "bucketName": bucket,
    })
    # _authenticate already validates bucket identity; this call also ensures
    # the lifecycle configuration is inspectable before any remote upload.
    rows = listed.get("buckets")
    if (not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict)
            or rows[0].get("bucketName") != bucket):
        raise CleanupError("cannot inspect B2 bucket lifecycle rules")
    bucket_id = rows[0].get("bucketId")
    if not isinstance(bucket_id, str) or not bucket_id:
        raise CleanupError("invalid B2 bucket identity")
    rules = rows[0].get("lifecycleRules")
    if not isinstance(rules, list):
        raise CleanupError("cannot inspect B2 bucket lifecycle rules")
    namespace = "historical-retirement/"
    for rule in rules:
        if not isinstance(rule, dict) or not isinstance(rule.get("fileNamePrefix"), str):
            raise CleanupError("cannot safely inspect B2 lifecycle rule")
        prefix = rule["fileNamePrefix"]
        if prefix.startswith(namespace) or namespace.startswith(prefix):
            raise CleanupError("B2 lifecycle rule may expire historical-retirement objects")
    return {"endpoint": endpoint, "bucket": bucket, "bucket_id": bucket_id}


def _remote_verify(settings: dict[str, Any], object_name: str, expected_size: int,
                   expected_sha: str, provider: Any = None) -> dict[str, Any]:
    provider = provider or _RetirementB2Provider()
    rows = provider.versions(settings, object_name)
    uploads = [row for row in rows if row.get("fileName") == object_name and row.get("action") == "upload"]
    if len(uploads) != 1:
        raise CleanupError("historical archive does not have one immutable B2 upload version")
    row = uploads[0]
    if type(row.get("size")) is not int or row["size"] != expected_size:
        raise CleanupError("B2 historical archive size mismatch")
    return row


def _archive_and_verify(record: dict[str, Any], stopped: Path, targets: list[dict[str, Any]],
                        runtime: Any, settings: dict[str, Any], provider: Any = None) -> dict[str, Any]:
    password = os.environ.get("APEROD_BACKUP_PASSWORD")
    if not password:
        raise CleanupError("APEROD_BACKUP_PASSWORD is required to archive historical data")
    provider = provider or _RetirementB2Provider()
    identity = _lifecycle_and_identity(settings)
    archive_members = _archive_manifest(targets)
    manifest_hash = _manifest_hash(archive_members)
    if record.get("manifest_sha256") != manifest_hash:
        raise CleanupError("historical manifest changed before archiving")
    state_dir = Path(runtime.state_dir)
    work = Path(tempfile.mkdtemp(prefix="retirement-work-", dir=state_dir))
    try:
        os.chmod(work, 0o700)
        _assert_root_dir(work, runtime=runtime)
        _ensure_staging_headroom(work, targets)
        plain = work / "selected.tar.gz"
        encrypted = work / "archive.tar.gz.gpg"
        decoded = work / "readback.tar.gz"
        _write_archive(targets, plain)
        if plain.stat().st_size > MAX_ARCHIVE_SIZE:
            raise CleanupError("historical archive exceeds size bound")
        _run_gpg_encrypt(plain, encrypted, password.encode())
        encrypted_size = encrypted.stat().st_size
        encrypted_sha = _sha_file(encrypted)
        object_name = "historical-retirement/" + record["id"] + "/archive.tar.gz.gpg"
        remote = "s3backup:" + identity["bucket"] + "/" + object_name
        item = settings.get("s3backup")
        if not isinstance(item, dict):
            raise CleanupError("S3 backup settings are unsupported")
        region = item.get("region", "us-west-004")
        if not isinstance(region, str) or not region:
            raise CleanupError("S3 backup region is invalid")
        upload_env = {
            "PATH": os.environ.get("PATH", os.defpath),
            "RCLONE_CONFIG_S3BACKUP_TYPE": "s3",
            "RCLONE_CONFIG_S3BACKUP_PROVIDER": "Other",
            "RCLONE_CONFIG_S3BACKUP_ENDPOINT": identity["endpoint"],
            "RCLONE_CONFIG_S3BACKUP_ACCESS_KEY_ID": item["accessKeyId"],
            "RCLONE_CONFIG_S3BACKUP_SECRET_ACCESS_KEY": item["secretAccessKey"],
            "RCLONE_CONFIG_S3BACKUP_REGION": region,
            "RCLONE_CONFIG_S3BACKUP_ACL": "private",
        }
        try:
            result = subprocess.run(
                ["rclone", "copyto", str(encrypted), remote, "--immutable", "--s3-no-check-bucket"],
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                timeout=600, check=False, env=upload_env,
            )
        except (OSError, subprocess.SubprocessError) as exc:
            raise CleanupError("historical archive upload failed") from exc
        if result.returncode != 0:
            raise CleanupError("historical archive upload failed")
        row = _remote_verify(settings, object_name, encrypted_size, encrypted_sha, provider)
        _RetirementB2Provider.download_file_id(
            settings, identity["bucket_id"], object_name, row["fileId"], decoded.with_suffix(".gpg"),
            encrypted_size,
        )
        downloaded = decoded.with_suffix(".gpg")
        if _sha_file(downloaded) != encrypted_sha or downloaded.stat().st_size != encrypted_size:
            raise CleanupError("B2 historical archive hash or size mismatch")
        _run_gpg_decrypt(downloaded, decoded, password.encode())
        _validate_archive(decoded, archive_members)
        # Pin only the exact uploaded immutable version and its verified bytes.
        exact_versions = provider.versions(settings, object_name)
        matching = [item for item in exact_versions if item.get("fileId") == row["fileId"]
                    and item.get("fileName") == object_name and item.get("action") == "upload"
                    and item.get("size") == encrypted_size]
        if len(matching) != 1:
            raise CleanupError("verified immutable B2 fileId is no longer present")
        return {
            "sha256": encrypted_sha, "size": encrypted_size, "file_id": row["fileId"],
            "bucket_id": identity["bucket_id"], "bucket": identity["bucket"],
            "endpoint": identity["endpoint"], "manifest_sha256": manifest_hash,
            "provider_fingerprint": _RetirementB2Provider.fingerprint(settings),
            "verified_at": int(runtime.clock()), "object_name": object_name,
        }
    finally:
        shutil.rmtree(work)


def _validated_archive_proof(record: dict[str, Any], settings: dict[str, Any],
                            identity: dict[str, str]) -> dict[str, Any]:
    proof = record.get("archive")
    if not isinstance(proof, dict):
        raise CleanupError("historical archive proof is missing")
    fields = {
        "sha256", "size", "file_id", "bucket_id", "bucket", "endpoint",
        "provider_fingerprint", "manifest_sha256", "verified_at", "object_name",
    }
    if set(proof) != fields:
        raise CleanupError("historical archive proof schema is invalid")
    if (not isinstance(proof.get("sha256"), str)
            or not re.fullmatch(r"[0-9a-f]{64}", proof["sha256"])
            or type(proof.get("size")) is not int or not 0 < proof["size"] <= MAX_ARCHIVE_SIZE
            or type(proof.get("verified_at")) is not int or proof["verified_at"] < 0):
        raise CleanupError("historical archive proof fingerprint is invalid")
    for key in ("file_id", "bucket_id", "bucket", "endpoint", "provider_fingerprint",
                "manifest_sha256", "object_name"):
        if not isinstance(proof.get(key), str) or not proof[key]:
            raise CleanupError("historical archive proof identity is invalid")
    if any(not re.fullmatch(r"[0-9a-f]{64}", proof[key])
           for key in ("provider_fingerprint", "manifest_sha256")):
        raise CleanupError("historical archive proof digest is invalid")
    expected_name = "historical-retirement/" + record["id"] + "/archive.tar.gz.gpg"
    members = _archive_manifest(record["targets"])
    manifest_hash = _manifest_hash(members)
    if (proof["object_name"] != expected_name
            or proof["manifest_sha256"] != manifest_hash
            or proof["manifest_sha256"] != record.get("manifest_sha256")):
        raise CleanupError("historical archive proof namespace or manifest binding is invalid")
    if (identity.get("bucket_id") != proof["bucket_id"]
            or identity.get("bucket") != proof["bucket"]
            or identity.get("endpoint") != proof["endpoint"]
            or _RetirementB2Provider.fingerprint(settings) != proof["provider_fingerprint"]):
        raise CleanupError("historical archive bucket identity changed")
    return proof


def _confirm_archive_proof(record: dict[str, Any], settings: dict[str, Any],
                           runtime: Any, provider: Any = None, *,
                           readback: bool = True) -> None:
    """Confirm immutable object identity; optionally perform a full archive readback."""
    identity = _lifecycle_and_identity(settings)
    proof = _validated_archive_proof(record, settings, identity)
    provider = provider or _RetirementB2Provider()
    rows = provider.versions(settings, proof["object_name"])
    exact = [row for row in rows if row.get("fileName") == proof["object_name"]
             and row.get("fileId") == proof["file_id"] and row.get("action") == "upload"]
    if (len(exact) != 1 or exact[0].get("size") != proof.get("size")
            or exact[0].get("bucketId") != proof.get("bucket_id")):
        raise CleanupError("verified immutable B2 fileId is no longer present")
    if not readback:
        return
    work = Path(tempfile.mkdtemp(prefix="retirement-work-", dir=runtime.state_dir))
    try:
        os.chmod(work, 0o700)
        _assert_root_dir(work, runtime=runtime)
        _ensure_staging_headroom(work, record["targets"])
        encrypted = work / "archive.tar.gz.gpg"
        plain = work / "readback.tar.gz"
        _RetirementB2Provider.download_file_id(
            settings, proof["bucket_id"], proof["object_name"], proof["file_id"],
            encrypted, proof["size"],
        )
        if _sha_file(encrypted) != proof["sha256"] or encrypted.stat().st_size != proof["size"]:
            raise CleanupError("B2 historical archive hash or size mismatch")
        password = os.environ.get("APEROD_BACKUP_PASSWORD")
        if not password:
            raise CleanupError("APEROD_BACKUP_PASSWORD is required to verify historical archive")
        _run_gpg_decrypt(encrypted, plain, password.encode())
        members = _archive_manifest(record["targets"])
        _validate_archive(plain, members)
        if _manifest_hash(members) != proof["manifest_sha256"]:
            raise CleanupError("historical archive manifest proof changed")
    finally:
        shutil.rmtree(work)


def register(release: str, data_dir: str, config: str, service: str,
             api_url: str, runtime: Any = None) -> dict[str, Any]:
    """Register a historical stopped-data copy; this is not rollout registration."""
    rt = _rt(runtime)
    state_dir = Path(rt.state_dir)
    with state_lock(state_dir, runtime=rt):
        root, _rollback, stopped = _closed_historical_path(Path(release), rt)
        origin = _capture_origin(Path(data_dir), Path(config), service, api_url, rt)
        _origin_unchanged(origin, rt)
        pre_hash_paths = [stopped / "chain.db", *_snapshot_paths(stopped)]
        for path in pre_hash_paths:
            _proc_references(origin["pid"], path, Path(origin["config"]), runtime=rt)
        targets = _targets(stopped, rt)
        _origin_unchanged(origin, rt)
        for target in targets:
            _proc_references(origin["pid"], Path(target["path"]), Path(origin["config"]),
                             runtime=rt, manifest=target["manifest"])
        _origin_unchanged(origin, rt)
        now = int(rt.clock())
        retirement_id = str(uuid.uuid4())
        record = {
            "schema": 1, "id": retirement_id, "type": "historical-retirement",
            "created_at": now, "status": "registered", "release": str(root),
            "release_identity": _identity(root, allow_dir=True),
            "stopped_data": str(stopped), "stopped_data_identity": _identity(stopped, allow_dir=True),
            "origin": origin, "targets": targets,
            "manifest_sha256": _manifest_hash(_archive_manifest(targets)),
            "archive": None, "receipts": [],
        }
        atomic_json(_retirement_path(state_dir, retirement_id), record)
        return {"id": retirement_id, "registered": True, "status": record["status"]}


def _revalidate_record(record: dict[str, Any], runtime: Any) -> tuple[Path, list[dict[str, Any]]]:
    root, _rollback, stopped = _closed_historical_path(Path(record["release"]), runtime)
    if (_identity(root, allow_dir=True) != record["release_identity"]
            or str(stopped) != record["stopped_data"]
            or _identity(stopped, allow_dir=True) != record["stopped_data_identity"]):
        raise CleanupError("historical release or stopped-data identity changed")
    receipts = {item.get("path"): item for item in record.get("receipts", [])
                if isinstance(item, dict)}
    expected_targets = {item["path"]: item for item in record["targets"]}
    if len(expected_targets) != len(record["targets"]):
        raise CleanupError("historical record contains duplicate targets")
    for path_text, target in expected_targets.items():
        path = Path(path_text)
        if target["kind"] == "stopped-copy":
            allowed = path == stopped / "chain.db"
        elif target["kind"] == "candidate":
            allowed = path.parent == stopped and bool(SNAPSHOT_FILE.fullmatch(path.name))
        else:
            allowed = False
        if not allowed:
            raise CleanupError("historical record target is outside the approved selection")
    current_paths: set[str] = set()
    db = stopped / "chain.db"
    if db.exists() or db.is_symlink():
        current_paths.add(str(db))
    current_paths.update(str(path) for path in _snapshot_paths(stopped))
    if current_paths - set(expected_targets):
        raise CleanupError("historical selection gained an unregistered target")
    current: list[dict[str, Any]] = []
    for path_text, target in expected_targets.items():
        path = Path(path_text)
        if not path.exists() and not path.is_symlink():
            receipt = receipts.get(path_text)
            if receipt is None or receipt.get("manifest_sha256") != _target_manifest_hash(target):
                raise CleanupError("registered historical target disappeared unexpectedly")
            continue
        current_paths.add(path_text)
        if target["kind"] == "stopped-copy":
            manifest, size = _manifest(path, "stopped-copy", runtime, allow_current_bak=True)
        else:
            fingerprint = _file_fingerprint(path, os.lstat(stopped).st_dev, runtime)
            manifest, size = [{"path": ".", **fingerprint}], fingerprint["size"]
        if (manifest != target["manifest"] or size != target["bytes"]
                or _identity(path, allow_dir=target["kind"] == "stopped-copy") != target["identity"]
                or stat.S_IMODE(os.lstat(path).st_mode) != target["root_mode"]):
            raise CleanupError("historical retirement source changed after registration")
        if path_text in receipts:
            raise CleanupError("a receipted historical target unexpectedly still exists")
        current.append(target)
    if current_paths - set(expected_targets):
        raise CleanupError("historical selection changed after registration")
    return stopped, current


def _target_manifest_hash(target: dict[str, Any]) -> str:
    return _manifest_hash([
        {"path": item["path"], "type": item["type"], "size": item.get("size", 0),
         "sha256": item.get("sha256")}
        for item in target["manifest"]
    ])


def _target_current_manifest(target: dict[str, Any], runtime: Any) -> list[dict[str, Any]]:
    path = Path(target["path"])
    if target["kind"] == "stopped-copy":
        return _manifest(path, "stopped-copy", runtime, allow_current_bak=True)[0]
    fingerprint = _file_fingerprint(path, os.lstat(path.parent).st_dev, runtime)
    return [{"path": ".", **fingerprint}]


def _delete_targets(record: dict[str, Any], targets: list[dict[str, Any]],
                    runtime: Any, settings: dict[str, Any]) -> int:
    removed = 0
    config = Path(record["origin"]["config"])
    for target in targets:
        path = Path(target["path"])
        if not path.exists() and not path.is_symlink():
            # Safe resume after a prior successful unlink. The receipt retains
            # which exact manifest was removed; do not infer completion by name.
            receipt = next((item for item in record.get("receipts", [])
                            if item.get("path") == target["path"]), None)
            if receipt is not None and receipt.get("manifest_sha256") == _target_manifest_hash(target):
                continue
            raise CleanupError("registered historical target disappeared unexpectedly")
        _origin_unchanged(record["origin"], runtime)
        current = _target_current_manifest(target, runtime)
        if current != target["manifest"]:
            raise CleanupError("historical target changed before deletion")
        _proc_references(record["origin"]["pid"], path, config, runtime=runtime,
                         manifest=current)
        _confirm_archive_proof(record, settings, runtime, readback=False)
        # Check the live origin, source fingerprints and every host reference at
        # the last possible point before the descriptor-relative unlink.
        _origin_unchanged(record["origin"], runtime)
        if _target_current_manifest(target, runtime) != target["manifest"]:
            raise CleanupError("historical target changed immediately before deletion")
        _proc_references(record["origin"]["pid"], path, config, runtime=runtime,
                         manifest=target["manifest"])
        removed += _delete_manifest(target, runtime)
        record.setdefault("receipts", []).append({
            "path": target["path"], "manifest_sha256": _target_manifest_hash(target),
            "deleted_at": int(runtime.clock()),
        })
        record["status"] = "archived"
        atomic_json(_retirement_path(Path(runtime.state_dir), record["id"]), record)
    return removed


def scan_retirements(apply: bool = False, runtime: Any = None) -> dict[str, Any]:
    """Inspect retirement records; dry-run never uploads, encrypts, or unlinks."""
    rt = _rt(runtime)
    state_dir = Path(rt.state_dir)
    with state_lock(state_dir, runtime=rt):
        records: list[dict[str, Any]] = []
        outcomes: list[dict[str, Any]] = []
        for entry in sorted(state_dir.iterdir()):
            if not RETIREMENT_FILE.fullmatch(entry.name):
                continue
            try:
                record = _safe_record(entry, rt)
                if record.get("type") != "historical-retirement" or record.get("id") != entry.name[11:-5]:
                    raise CleanupError("invalid historical retirement record")
                records.append(record)
            except CleanupError:
                outcomes.append({"id": entry.name[11:-5], "status": "refused",
                                 "error": "invalid or unsafe historical retirement record"})
            except (OSError, ValueError, KeyError, TypeError):
                outcomes.append({"id": entry.name[11:-5], "status": "refused",
                                 "error": "operation refused: unsafe record or host I/O failure"})
        for record in records:
            if record["status"] == "completed":
                outcomes.append({"id": record["id"], "status": "completed", "removed_bytes": 0})
                continue
            try:
                stopped, targets = _revalidate_record(record, rt)
                _origin_unchanged(record["origin"], rt)
                for target in targets:
                    _proc_references(record["origin"]["pid"], Path(target["path"]),
                                     Path(record["origin"]["config"]), runtime=rt,
                                     manifest=target["manifest"])
                if not apply:
                    outcomes.append({"id": record["id"], "status": "ready",
                                     "targets": len(targets), "bytes": sum(t["bytes"] for t in targets)})
                    continue
                settings = _settings(rt)
                if record.get("archive") is None:
                    record["archive"] = _archive_and_verify(record, stopped, targets, rt, settings)
                else:
                    # A resumed partial job performs one complete readback;
                    # per-target checks below avoid re-downloading the full archive.
                    _confirm_archive_proof(record, settings, rt)
                record["status"] = "archived"
                atomic_json(_retirement_path(state_dir, record["id"]), record)
                # Source must still be byte-for-byte identical after remote
                # verification; no backup chain coverage is inferred here.
                _origin_unchanged(record["origin"], rt)
                _revalidate_record(record, rt)
                removed_bytes = _delete_targets(record, targets, rt, settings)
                record["status"] = "completed"
                record["completed_at"] = int(rt.clock())
                atomic_json(_retirement_path(state_dir, record["id"]), record)
                outcomes.append({"id": record["id"], "status": "completed",
                                 "removed_bytes": removed_bytes})
            except CleanupError as exc:
                outcomes.append({"id": record.get("id"), "status": "refused", "error": str(exc)})
            except (OSError, ValueError, KeyError, TypeError):
                outcomes.append({"id": record.get("id"), "status": "refused",
                                 "error": "operation refused: unsafe record or host I/O failure"})
        return {
            "apply": bool(apply), "status": "refused" if any(
                row.get("status") == "refused" for row in outcomes
            ) else "ok", "retirements": outcomes,
        }


__all__ = ["register", "scan_retirements"]