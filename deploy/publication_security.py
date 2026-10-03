#!/usr/bin/env python3
"""Fail closed on forbidden artifacts in a complete publication candidate."""
import argparse
import re
import subprocess
import sys
from pathlib import Path, PurePosixPath


PRIVATE_BLOCK = re.compile(
    rb"-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----"
)
MAX_SOURCE_BYTES = 5 * 1024 * 1024


def path_problem(path, mode="100644"):
    p = PurePosixPath(path)
    if not path or any(ord(c) < 32 for c in path) or p.is_absolute() or ".." in p.parts or "\\" in path:
        return "unsafe publication path"
    if path == "deploy/sync-public-repo.sh":
        return "private publication wrapper"
    if mode in ("120000", "160000"):
        return "symlinks and submodules require separate security approval"
    name = p.name.lower()
    if name.endswith((".key", ".keyfile", ".pem", ".p12", ".pfx", ".keystore")):
        return "key or credential file"
    if name == ".env" or (name.startswith(".env.") and not name.endswith((".example", ".sample"))):
        return "environment credential file"
    if path.startswith(("data/testnet/", "data/mainnet/", "data/devnet/", "snapshots/")):
        return "node runtime data"
    if any(part in ("chain.db", "node_modules", ".git", "__pycache__") for part in p.parts):
        return "runtime, dependency or repository-internal data"
    if name.endswith((".db", ".sqlite", ".sqlite3", ".ldb", ".sst", ".snapshot", ".snap")):
        return "database or snapshot"
    if path in ("aperod", "cli", "node", "aperod-node") or name.endswith((".exe", ".dll", ".so")):
        return "compiled executable"
    return None


def content_problem(path, content):
    if len(content) > MAX_SOURCE_BYTES:
        return "oversized source-distribution artifact"
    if content.startswith((b"\x7fELF", b"MZ", b"SQLite format 3\x00",
                           b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xcf")):
        return "executable or database content under a disguised filename"
    if PRIVATE_BLOCK.search(content):
        return "private-key PEM block"
    if re.fullmatch(rb"[0-9a-fA-F]{64}|[0-9a-fA-F]{128}", content.strip()):
        return "unlabelled key-sized hex material"
    if len(content) in (32, 64):
        try:
            content.decode("utf-8")
        except UnicodeDecodeError:
            return "raw key-sized binary"
    return None


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args])


def inspect(repo, tree="HEAD", worktree=False):
    failures = []
    checked = 0
    if worktree:
        tracked = git(repo, "ls-files", "-z").split(b"\0")
        untracked = git(repo, "ls-files", "--others", "--exclude-standard", "-z").split(b"\0")
        entries = [(p.decode("utf-8"), None, None) for p in sorted(set(tracked + untracked)) if p]
    else:
        entries = []
        for item in git(repo, "ls-tree", "-rz", "--full-tree", tree).split(b"\0"):
            if item:
                metadata, path = item.split(b"\t", 1)
                mode, kind, oid = metadata.decode("ascii").split()
                entries.append((path.decode("utf-8"), mode, oid if kind == "blob" else None))
    for path, mode, oid in entries:
        if worktree:
            target = repo / path
            # An overlay can contain explicit deletions from the candidate.
            if not target.exists() and not target.is_symlink():
                continue
            if target.is_symlink():
                mode = "120000"
        checked += 1
        reason = path_problem(path, mode)
        if reason:
            failures.append((path, reason))
            continue
        content = (repo / path).read_bytes() if worktree else git(repo, "cat-file", "blob", oid)
        reason = content_problem(path, content)
        if reason:
            failures.append((path, reason))
    return checked, failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path("."))
    parser.add_argument("--tree", default="HEAD")
    parser.add_argument("--worktree", action="store_true")
    args = parser.parse_args()
    try:
        checked, failures = inspect(args.repo.resolve(), args.tree, args.worktree)
    except (OSError, UnicodeError, subprocess.CalledProcessError) as error:
        print(f"Publication security could not complete: {type(error).__name__}", file=sys.stderr)
        return 2
    for path, reason in failures:
        # Never print file contents, matches, seeds or credential values.
        print(f"REFUSED: {path}: {reason}", file=sys.stderr)
    print(f"Publication policy checked {checked} files; {len(failures)} forbidden artifacts.")
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())