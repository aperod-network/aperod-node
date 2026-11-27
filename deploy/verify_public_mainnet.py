#!/usr/bin/env python3
"""Read-only rollout check for both public Aperod block APIs."""

import json
import sys
import urllib.error
import urllib.request


HOSTS = ("api.aperod.com", "explorer.aperod.com")
GENESIS_HEADER = "c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f"
GENESIS_INDEX = "da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1"
STALE_GENESIS = "ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb"
STALE_FIRST = "61457b6a59a321588cd501365ab4d3bbbe4ab9ed7e03c326cdeda86adcb28809"
FIRST_INDEX = "056476ad6cf64f8c47a7ec0f0033128cc7759d2bdfd699a319d44d6146a94654"
SNAPSHOT_HEIGHT = 4076315
SNAPSHOT_TIP = "48f2668a598b9fe9111bed594ce7fcf646bdb15385eac91542188dd5a82aea70"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def fetch(host, block):
    base = host.rstrip("/") if "://" in host else f"https://{host}"
    request = urllib.request.Request(
        f"{base}/api/v1/blocks/{block}",
        headers={
            "Cache-Control": "no-cache",
            "Accept": "application/json",
            "User-Agent": "Aperod operator read-only check",
        },
    )
    try:
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=8) as response:
            body = response.read(2 * 1024 * 1024 + 1)
            if len(body) > 2 * 1024 * 1024:
                raise ValueError("public block response exceeds the verification limit")
            return response.status, json.loads(body)
    except urllib.error.HTTPError as error:
        with error:
            return error.code, None


def verify_host(host, get=fetch):
    checks = (
        ("0", 0, GENESIS_INDEX, "0" * 64),
        (GENESIS_INDEX, 0, GENESIS_INDEX, "0" * 64),
        ("1", 1, FIRST_INDEX, GENESIS_INDEX),
        (str(SNAPSHOT_HEIGHT), SNAPSHOT_HEIGHT, SNAPSHOT_TIP, None),
    )
    errors = []
    for block, height, expected_hash, parent in checks:
        try:
            status, data = get(host, block)
            if status != 200 or not isinstance(data, dict):
                errors.append(f"{host} {block}: expected block HTTP 200, got {status}")
                continue
            if data.get("height") != height or data.get("hash") != expected_hash:
                errors.append(f"{host} {block}: unexpected block height or hash")
            if parent is not None and data.get("prev_hash") != parent:
                errors.append(f"{host} {block}: unexpected parent hash")
            if height <= 1 and data.get("pruned") is True:
                errors.append(f"{host} {block}: early block was served from a pruned row")
        except (OSError, ValueError) as error:
            errors.append(f"{host} {block}: {error}")
    try:
        for label, stale in (("genesis", STALE_GENESIS), ("first block", STALE_FIRST)):
            status, _ = get(host, stale)
            if status != 404:
                errors.append(f"{host} stale {label}: expected HTTP 404, got {status}")
    except (OSError, ValueError) as error:
        errors.append(f"{host} stale block lookup: {error}")
    return errors


def main():
    hosts = tuple(sys.argv[1:]) or HOSTS
    errors = [error for host in hosts for error in verify_host(host)]
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"All {len(hosts)} API endpoint(s) match the inspected Mainnet index anchors")
    return 0


if __name__ == "__main__":
    sys.exit(main())
