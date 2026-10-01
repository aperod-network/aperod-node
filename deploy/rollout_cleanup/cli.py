"""Command-line interface for the rollout cleanup service."""

from __future__ import annotations

import argparse
import json
import sys

from . import (
    CleanupError,
    STATE_DIR,
    backup_abort,
    backup_begin,
    backup_pin,
    backup_publish,
    begin,
    complete,
    mark,
    scan,
    state_lock,
)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="aperod-rollout-cleanup")
    commands = parser.add_subparsers(dest="command", required=True)

    start = commands.add_parser("begin")
    start.add_argument("--release", required=True)
    start.add_argument("--data-dir", required=True)
    start.add_argument("--config", required=True)
    start.add_argument("--service", required=True)
    start.add_argument("--api-url", required=True)

    add_mark = commands.add_parser("mark")
    add_mark.add_argument("--id", required=True)
    add_mark.add_argument("--artifact", required=True)
    add_mark.add_argument("--kind", required=True, choices=("stopped-copy", "candidate"))

    finish = commands.add_parser("complete")
    finish.add_argument("--id", required=True)
    finish.add_argument("--expected-binary-sha256", required=True)

    check = commands.add_parser("scan")
    check.add_argument("--apply", action="store_true", help="delete only artifacts proven safe for removal")

    backup_start = commands.add_parser("backup-begin")
    backup_start.add_argument("--data-dir", required=True)
    backup_start.add_argument("--config", required=True)
    backup_start.add_argument("--service", required=True)
    backup_start.add_argument("--api-url", required=True)
    backup_start.add_argument("--anchors-output", required=True)
    backup_start.add_argument("--context-output", required=True)

    pin = commands.add_parser("backup-pin")
    pin.add_argument("--context", required=True)
    pin.add_argument("--settings", required=True)
    pin.add_argument("--remote-object", required=True)

    publish = commands.add_parser("backup-publish")
    publish.add_argument("--context", required=True)
    publish.add_argument("--proof", required=True)
    publish.add_argument("--settings", required=True)
    publish.add_argument("--remote-object", required=True)
    publish.add_argument("--archive-sha256", required=True)
    publish.add_argument("--archive-size", required=True, type=int)
    publish.add_argument("--tip-height", required=True, type=int)
    publish.add_argument("--tip-hash", required=True)

    abort = commands.add_parser("backup-abort")
    abort.add_argument("--context", required=True)
    return parser


def _dispatch(args: argparse.Namespace) -> dict:
    if args.command == "begin":
        with state_lock():
            return begin(args.release, args.data_dir, args.config, args.service, args.api_url)
    if args.command == "mark":
        with state_lock():
            return mark(args.id, args.artifact, args.kind)
    if args.command == "complete":
        with state_lock():
            return complete(args.id, args.expected_binary_sha256)
    if args.command == "scan":
        return scan(args.apply)
    if args.command == "backup-begin":
        return backup_begin(args.data_dir, args.config, args.service, args.api_url,
                            args.anchors_output, args.context_output)
    if args.command == "backup-pin":
        with state_lock():
            return backup_pin(args.context, args.settings, args.remote_object)
    if args.command == "backup-publish":
        with state_lock():
            return backup_publish(
                args.context, args.proof, args.settings, args.remote_object,
                args.archive_sha256, args.archive_size, args.tip_height, args.tip_hash,
            )
    if args.command == "backup-abort":
        return backup_abort(args.context)
    raise CleanupError("unknown command")


def main(argv: list[str] | None = None) -> int:
    try:
        args = _parser().parse_args(argv)
        result = _dispatch(args)
        sys.stdout.write(json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n")
        return 0
    except CleanupError as exc:
        sys.stdout.write(json.dumps({"error": str(exc)}, sort_keys=True, separators=(",", ":")) + "\n")
        return 1
    except (OSError, ValueError, KeyError, TypeError) as exc:
        # Deliberately avoid emitting exception details from credential/network code.
        sys.stdout.write(json.dumps({"error": "operation refused: " + type(exc).__name__}, separators=(",", ":")) + "\n")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())