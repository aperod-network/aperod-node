#!/usr/bin/env python3
"""Shared GitHub transport; legacy unverified publication is disabled."""
import hashlib
import json
import urllib.error
import urllib.parse
import urllib.request

API = "https://api.github.com/repos/aperod-network/aperod-node"
AUTHOR = {"name": "aperod-network", "email": "aperod-network@users.noreply.github.com"}


def git_blob_sha1(content):
    return hashlib.sha1(f"blob {len(content)}\0".encode() + content).hexdigest()


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlsplit(newurl)
        if (target.scheme != "https" or target.hostname != "api.github.com"
                or target.port not in (None, 443)
                or target.username or target.password):
            raise RuntimeError("GitHub API redirect refused")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def make_request(token):
    def request(method, url, data=None):
        parsed = urllib.parse.urlsplit(url)
        if (parsed.scheme != "https" or parsed.hostname != "api.github.com"
                or parsed.port not in (None, 443)
                or parsed.username or parsed.password):
            raise RuntimeError("Refuse authenticated request outside GitHub API")
        headers = {
            "Authorization": "Bearer " + token,
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        if data is not None:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(
            url, data=json.dumps(data).encode() if data is not None else None,
            headers=headers, method=method)
        try:
            opener = urllib.request.build_opener(SafeRedirect())
            with opener.open(req, timeout=120) as response:
                return json.loads(response.read()) if response.status != 204 else {}
        except urllib.error.HTTPError as error:
            # Do not echo response payloads or request bodies from credential APIs.
            raise RuntimeError(f"GitHub request failed with HTTP {error.code}") from None
    return request


def sync(*args, **kwargs):
    raise RuntimeError("Legacy publication disabled; use publish_public_repo.py")


def main():
    raise SystemExit("Legacy publication disabled; use publish_public_repo.py (token from environment)")


if __name__ == "__main__":
    main()