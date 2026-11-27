#!/usr/bin/env python3
"""Render the Grafana import dashboard for fixed-UID file provisioning."""

import json
import pathlib
import sys


def bind_datasource(value, datasource_uid):
    if isinstance(value, dict):
        if value.get("uid") == "${DS_PROMETHEUS}":
            value["uid"] = datasource_uid
        for child in value.values():
            bind_datasource(child, datasource_uid)
    elif isinstance(value, list):
        for child in value:
            bind_datasource(child, datasource_uid)


def main():
    if len(sys.argv) not in (3, 4):
        raise SystemExit("usage: render-grafana-dashboard.py INPUT.json OUTPUT.json [DATASOURCE_UID]")
    source = pathlib.Path(sys.argv[1])
    destination = pathlib.Path(sys.argv[2])
    datasource_uid = sys.argv[3] if len(sys.argv) == 4 else "prometheus"
    if not datasource_uid or any(ch not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-" for ch in datasource_uid):
        raise SystemExit("invalid Prometheus datasource UID")
    dashboard = json.loads(source.read_text(encoding="utf-8"))
    dashboard.pop("__inputs", None)
    dashboard["panels"] = [
        panel for panel in dashboard.get("panels", [])
        if panel.get("id") not in {20, 21, 22, 43, 50, 51, 52}
    ]
    bind_datasource(dashboard, datasource_uid)
    if "${DS_PROMETHEUS}" in json.dumps(dashboard):
        raise SystemExit("unresolved Prometheus datasource placeholder")
    if dashboard.get("uid") != "aperod-node-overview":
        raise SystemExit("unexpected dashboard UID")
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(dashboard, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
