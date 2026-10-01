"""Native immutable Backblaze B2 version lookup for backup authorization."""

from __future__ import annotations

import base64
import hashlib
import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Protocol

from .runtime import CleanupError, strict_json_bytes


class BackupProvider(Protocol):
    def identity(self, settings: dict[str, Any]) -> dict[str, str]: ...
    def versions(self, settings: dict[str, Any], object_name: str) -> list[dict[str, Any]]: ...
    def fingerprint(self, settings: dict[str, Any]) -> str: ...


class NativeB2Provider:
    """Use B2's native fileId APIs only; S3 object heads are not proof."""

    AUTH_URL = "https://api.backblazeb2.com/b2api/v2/b2_authorize_account"

    @staticmethod
    def _configuration(settings: dict[str, Any]) -> tuple[str, str, str, str]:
        item = settings.get("s3backup")
        if not isinstance(item, dict):
            raise CleanupError("S3 backup settings are unsupported")
        endpoint, bucket = item.get("endpoint"), item.get("bucket", "aperod-vault")
        access, secret = item.get("accessKeyId"), item.get("secretAccessKey")
        if not all(isinstance(value, str) and value for value in (endpoint, bucket, access, secret)):
            raise CleanupError("S3 backup settings are incomplete")
        parsed = urllib.parse.urlsplit(endpoint if "://" in endpoint else "https://" + endpoint)
        if (parsed.scheme != "https" or not NativeB2Provider._is_b2_host(parsed.hostname)
                or parsed.username is not None or parsed.password is not None
                or parsed.port not in (None, 443) or parsed.path not in ("", "/")
                or parsed.query or parsed.fragment):
            raise CleanupError("only HTTPS Backblaze B2 cleanup authorization is supported")
        return parsed.geturl().rstrip("/"), bucket, access, secret

    @staticmethod
    def _is_b2_host(host: str | None) -> bool:
        if not host:
            return False
        normalized = host.lower().rstrip(".")
        return normalized == "backblazeb2.com" or normalized.endswith(".backblazeb2.com")

    @staticmethod
    def fingerprint(settings: dict[str, Any]) -> str:
        endpoint, bucket, access, secret = NativeB2Provider._configuration(settings)
        return hashlib.sha256(
            (endpoint + "\0" + bucket + "\0" + access + "\0" + secret).encode()
        ).hexdigest()

    @staticmethod
    def _request(url: str, token: str | None = None,
                 payload: dict[str, Any] | None = None,
                 basic: str | None = None) -> dict[str, Any]:
        parsed = urllib.parse.urlsplit(url)
        if (parsed.scheme != "https" or not NativeB2Provider._is_b2_host(parsed.hostname)
                or parsed.port not in (None, 443) or parsed.username is not None
                or parsed.password is not None):
            raise CleanupError("refusing non-B2 API host")
        headers = {"Accept": "application/json"}
        data = None
        if basic is not None:
            headers["Authorization"] = "Basic " + basic
        if token is not None:
            headers["Authorization"] = token
        if payload is not None:
            headers["Content-Type"] = "application/json"
            data = (json.dumps(payload, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
        request = urllib.request.Request(
            url, data=data, headers=headers, method="POST" if data is not None else "GET",
        )

        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, response_headers, newurl):
                return None

        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
            with opener.open(request, timeout=20) as response:
                final = urllib.parse.urlsplit(response.geturl())
                if (final.scheme != "https" or not NativeB2Provider._is_b2_host(final.hostname)):
                    raise CleanupError("B2 API redirected to an untrusted host")
                body = response.read(4 * 1024 * 1024 + 1)
        except (OSError, urllib.error.URLError, TimeoutError) as exc:
            raise CleanupError("Backblaze B2 request failed") from exc
        value = strict_json_bytes(body)
        if not isinstance(value, dict):
            raise CleanupError("unsupported B2 response schema")
        return value

    @classmethod
    def _authenticate(cls, settings: dict[str, Any]) -> tuple[str, str, str, str, str, str, str]:
        endpoint, bucket, access, secret = cls._configuration(settings)
        basic = base64.b64encode((access + ":" + secret).encode()).decode("ascii")
        auth = cls._request(cls.AUTH_URL, basic=basic)
        api_url, token = auth.get("apiUrl"), auth.get("authorizationToken")
        account_id = auth.get("accountId")
        if not isinstance(api_url, str) or not isinstance(token, str) or not isinstance(account_id, str):
            raise CleanupError("incomplete B2 authorization response")
        parsed = urllib.parse.urlsplit(api_url)
        if (parsed.scheme != "https" or not cls._is_b2_host(parsed.hostname)
                or parsed.port not in (None, 443) or parsed.username is not None
                or parsed.password is not None or parsed.path not in ("", "/")
                or parsed.query or parsed.fragment):
            raise CleanupError("untrusted B2 API URL")
        api_url = api_url.rstrip("/")
        listed = cls._request(
            api_url + "/b2api/v2/b2_list_buckets", token,
            {"accountId": account_id, "bucketName": bucket},
        )
        buckets = listed.get("buckets")
        if not isinstance(buckets, list) or len(buckets) != 1 or not isinstance(buckets[0], dict):
            raise CleanupError("cannot uniquely identify configured B2 bucket")
        if buckets[0].get("bucketName") != bucket:
            raise CleanupError("B2 returned an unexpected bucket name")
        bucket_id = buckets[0].get("bucketId")
        if not isinstance(bucket_id, str) or not bucket_id:
            raise CleanupError("invalid B2 bucket identity")
        return endpoint, bucket, access, secret, token, bucket_id, api_url

    def identity(self, settings: dict[str, Any]) -> dict[str, str]:
        endpoint, bucket, access, _, _, bucket_id, _ = self._authenticate(settings)
        return {"endpoint": endpoint, "bucket": bucket, "access_key_id": access,
                "bucket_id": bucket_id}

    def versions(self, settings: dict[str, Any], object_name: str) -> list[dict[str, Any]]:
        endpoint, bucket, _, _, token, bucket_id, api_url = self._authenticate(settings)
        del endpoint, bucket
        rows: list[dict[str, Any]] = []
        cursor: tuple[str, str] | None = None
        seen: set[tuple[str, str]] = set()
        file_ids: set[str] = set()
        examined_rows = 0
        for _ in range(100):
            request: dict[str, Any] = {
                "bucketId": bucket_id, "prefix": object_name, "maxFileCount": 1000,
            }
            if cursor:
                request["startFileName"], request["startFileId"] = cursor
            page = self._request(api_url + "/b2api/v2/b2_list_file_versions", token, request)
            files = page.get("files")
            if not isinstance(files, list):
                raise CleanupError("unsupported B2 file-version response")
            for row in files:
                examined_rows += 1
                if examined_rows > 10000:
                    raise CleanupError("B2 version listing exceeded row bound")
                if not isinstance(row, dict):
                    raise CleanupError("invalid B2 file-version row")
                file_name = row.get("fileName")
                if not isinstance(file_name, str) or not file_name.startswith(object_name):
                    raise CleanupError("B2 returned an object outside the requested prefix")
                if file_name != object_name:
                    continue
                if row.get("bucketId") != bucket_id:
                    raise CleanupError("B2 returned an unexpected bucket")
                file_id, action, timestamp = row.get("fileId"), row.get("action"), row.get("uploadTimestamp")
                if (not isinstance(file_id, str) or not file_id or len(file_id) > 256
                        or action not in ("upload", "hide")
                        or type(timestamp) is not int or timestamp < 0 or timestamp > 2**63 - 1
                        or file_id in file_ids):
                    raise CleanupError("invalid B2 immutable version identity")
                file_ids.add(file_id)
                content_length = row.get("contentLength")
                if action == "upload" and (type(content_length) is not int or content_length < 0):
                    raise CleanupError("B2 upload row lacks valid contentLength")
                rows.append({
                    "fileId": file_id, "fileName": file_name, "bucketId": bucket_id,
                    "action": action, "uploadTimestamp": timestamp,
                    "size": content_length if action == "upload" else None,
                })
            next_name, next_id = page.get("nextFileName"), page.get("nextFileId")
            if next_name is None and next_id is None:
                return rows
            if (not isinstance(next_name, str) or not next_name.startswith(object_name)
                    or not isinstance(next_id, str) or not next_id or len(next_id) > 256):
                raise CleanupError("invalid B2 version cursor")
            cursor = (next_name, next_id)
            if cursor in seen:
                raise CleanupError("B2 version listing repeated a cursor")
            seen.add(cursor)
        raise CleanupError("B2 version listing exceeded bound")


DEFAULT_PROVIDER = NativeB2Provider()