#!/usr/bin/env python3
"""Scan an immutable source distribution before uploading any public objects."""
import argparse
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

from publication_security import git, inspect, path_problem
from sync_public_repo import API, AUTHOR, make_request

EXTENSIONS = {".go", ".sh", ".py", ".mod", ".sum", ".yaml", ".yml",
              ".toml", ".md", ".txt", ".json"}
EXCLUDED = {".git", "data", "build", "node_modules", "__pycache__"}
IGNORES = """
# Runtime state and credentials never belong in a source distribution.
data/testnet/
data/mainnet/
data/devnet/
snapshots/
*.key
*.keyfile
*.pem
.env
.env.*
!.env.example
!.env.sample
aperod
cli
aperod-node
build/
__pycache__/
"""
MESSAGE = ("chore: maintain verified source distribution\n\n"
           "Co-authored-by: aperod-network <aperod-network@users.noreply.github.com>")


def run(*args, cwd=None):
    # Repository-controlled tests/builds must not inherit publication tokens,
    # SSH passwords, bot credentials or unrelated application secrets.
    allowed = {"PATH", "HOME", "TMPDIR", "LANG", "TZ", "GOTOOLCHAIN",
               "GOPATH", "GOMAXPROCS", "GOSUMDB", "GOCACHE", "GOPROXY",
               "CGO_ENABLED", "GOROOT", "LD_LIBRARY_PATH", "NIX_LD"}
    env = {k: v for k, v in os.environ.items() if k in allowed}
    subprocess.run(list(args), cwd=cwd, check=True, env=env)


def prepare(root, candidate, gate_only=False):
    source = root / "blockchain" if (root / "blockchain/go.mod").exists() else root
    # Only runtime artifacts in the disposable clone are removed. This tool
    # never opens a production data directory or performs a server checkout.
    for raw in git(candidate, "ls-files", "-s", "-z").split(b"\0"):
        if not raw:
            continue
        metadata, name = raw.split(b"\t", 1)
        mode = metadata.decode().split()[0]
        rel = name.decode()
        if path_problem(rel, mode):
            (candidate / rel).unlink()
    if gate_only:
        selected = [source / "deploy" / name for name in (
            "publication_security.py", "test_publication_security.py",
            "publish_public_repo.py", "test_publish_public_repo.py",
            "sync_public_repo.py", "test_sync_public_repo.py")]
        selected += [source / ".github/workflows/publication-security.yml"]
    else:
        selected = []
        for directory, dirs, files in os.walk(source, followlinks=False):
            dirs[:] = [d for d in dirs if d not in EXCLUDED
                       and not (Path(directory) / d).is_symlink()]
            for name in files:
                file = Path(directory) / name
                rel = file.relative_to(source)
                if (file.is_file() and not file.is_symlink()
                        and file.suffix in EXTENSIONS
                        and not path_problem(rel.as_posix())
                        and rel.name not in ("README.md", "README-public.md")):
                    selected.append(file)
    for file in selected:
        if not file.is_file() or file.is_symlink():
            raise RuntimeError("Missing or unsafe required publication source")
        rel = file.relative_to(source)
        dest = candidate / rel
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(file, dest)
    if not gate_only and (source / "deploy/README-public.md").exists():
        shutil.copyfile(source / "deploy/README-public.md", candidate / "README.md")
    if not gate_only:
        # Source/test removals must be atomic with additions. Keep public-only
        # documentation and branding; remove obsolete Go files bidirectionally.
        local_go = {f.relative_to(source).as_posix() for f in selected if f.suffix == ".go"}
        for raw in git(candidate, "ls-files", "-z").split(b"\0"):
            rel = raw.decode()
            if rel.endswith(".go") and rel not in local_go and (candidate / rel).exists():
                (candidate / rel).unlink()
    ignore = candidate / ".gitignore"
    text = ignore.read_text() if ignore.exists() else ""
    if "# Runtime state and credentials" not in text:
        ignore.write_text(text.rstrip() + "\n" + IGNORES)
    run("git", "add", "-A", cwd=candidate)
    return git(candidate, "write-tree").decode().strip()


def verify(candidate, tree, go_checks=True, with_tests=False):
    checked, failures = inspect(candidate, tree=tree)
    if failures:
        raise RuntimeError("Publication policy refused paths: " +
                           ", ".join(path for path, _ in failures))
    if not shutil.which("gitleaks"):
        raise RuntimeError("Secret scanner missing; publication refused")
    # Export exactly the frozen index. Ignored/untracked worktree contents
    # cannot diverge from the Git objects later uploaded.
    with tempfile.TemporaryDirectory(prefix="aperod-source-check-") as temp:
        export = Path(temp)
        run("git", "checkout-index", "--all", "--prefix=" + temp + "/", cwd=candidate)
        run("gitleaks", "dir", temp, "--redact=100", "--no-banner")
        for test in ("test_publication_security.py", "test_publish_public_repo.py",
                     "test_sync_public_repo.py"):
            run("python3", str(export / "deploy" / test), cwd=export)
        if go_checks:
            run("go", "build", "./...", cwd=export)
            run("go", "vet", "./...", cwd=export)
            run("go", "run", "golang.org/x/vuln/cmd/govulncheck@v1.1.4", "./...", cwd=export)
            if with_tests:
                run("go", "test", "./...", cwd=export)
    if git(candidate, "write-tree").decode().strip() != tree:
        raise RuntimeError("Candidate changed during verification")
    print(f"Verified immutable candidate: {checked} files.")


def publish(candidate, tree, expected_head, request, with_tests=False):
    # Independently revalidate everything before the first upload; callers
    # cannot bypass verification by invoking this Python API directly.
    verify(candidate, tree, with_tests=with_tests)
    if request("GET", f"{API}/git/ref/heads/main")["object"]["sha"] != expected_head:
        raise RuntimeError("Public main changed; prepare and verify again")
    changed = git(candidate, "diff", "--cached", "--name-only", "-z").split(b"\0")
    if not any(changed):
        return None
    frozen = {}
    for row in git(candidate, "ls-tree", "-rz", tree).split(b"\0"):
        if row:
            meta, path = row.split(b"\t", 1)
            mode, kind, oid = meta.decode().split()
            frozen[path.decode()] = (mode, oid)
    entries = []
    for raw in changed:
        if not raw:
            continue
        rel = raw.decode()
        if rel not in frozen:
            entries.append({"path": rel, "mode": "100644", "type": "blob", "sha": None})
            continue
        mode, oid = frozen[rel]
        content = git(candidate, "cat-file", "blob", oid)
        blob = request("POST", f"{API}/git/blobs", {
            "content": base64.b64encode(content).decode(), "encoding": "base64"})
        if blob["sha"] != oid:
            raise RuntimeError("Uploaded object integrity mismatch")
        entries.append({"path": rel, "mode": mode, "type": "blob", "sha": oid})
    base = request("GET", f"{API}/git/commits/{expected_head}")["tree"]["sha"]
    remote_tree = request("POST", f"{API}/git/trees", {"base_tree": base, "tree": entries})
    if remote_tree["sha"] != tree:
        raise RuntimeError("Remote publication tree differs from verified snapshot")
    commit = request("POST", f"{API}/git/commits", {
        "message": MESSAGE, "tree": tree, "parents": [expected_head], "author": AUTHOR})
    branch = "verified-publication/" + commit["sha"][:16]
    request("POST", f"{API}/git/refs", {"ref": "refs/heads/" + branch, "sha": commit["sha"]})
    pr = request("POST", f"{API}/pulls", {
        "title": "chore: maintain verified source distribution",
        "head": branch, "base": "main",
        "body": "Complete immutable source tree passed artifact policy, redacted secret scanning, "
                "Go build, vet and dependency vulnerability checks before object upload. "
                "No production runtime state is changed."})
    print("Verified publication awaiting required checks:", pr["html_url"])
    return pr


def await_merge(pr, request, timeout=900):
    deadline = time.monotonic() + timeout
    required = {"Whole-tree publication security", "go build & vet & test"}
    while time.monotonic() < deadline:
        pull = request("GET", f"{API}/pulls/{pr['number']}")
        if pull.get("merged"):
            print("Verified publication merged:", pr["html_url"])
            return
        if pull["state"] != "open":
            raise RuntimeError("Publication PR closed without merging")
        sha = pull["head"]["sha"]
        checks = request("GET", f"{API}/commits/{sha}/check-runs?per_page=100")["check_runs"]
        trusted = {c["name"]: c for c in sorted(checks, key=lambda c: c.get("id", 0))
                   if c.get("app", {}).get("id") == 15368}
        for name in required:
            check = trusted.get(name)
            if check and check["status"] == "completed" and check["conclusion"] != "success":
                raise RuntimeError("Required publication check failed: " + name)
        if all(name in trusted and trusted[name]["status"] == "completed"
               and trusted[name]["conclusion"] == "success" for name in required):
            result = request("PUT", f"{API}/pulls/{pr['number']}/merge", {
                "sha": sha, "merge_method": "merge",
                "commit_title": "chore: maintain verified source distribution",
                "commit_message": "Co-authored-by: aperod-network <aperod-network@users.noreply.github.com>"})
            if not result.get("merged"):
                raise RuntimeError("Required branch policy refused the verified merge")
            print("Verified publication merged:", pr["html_url"])
            return
        time.sleep(10)
    raise RuntimeError("Publication checks did not complete; main was not updated")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("."))
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--gate-only", action="store_true")
    parser.add_argument("--with-tests", action="store_true")
    parser.add_argument("--no-wait", action="store_true")
    parser.add_argument("--check-drift", action="store_true")
    args = parser.parse_args()
    token = os.environ.get("PUBLIC_GITHUB_TOKEN")
    if not args.dry_run and not args.check_drift and not token:
        raise SystemExit("PUBLIC_GITHUB_TOKEN must be provided through the environment")
    with tempfile.TemporaryDirectory(prefix="aperod-publication-") as temp:
        candidate = Path(temp) / "repo"
        run("git", "clone", "--depth", "1", "-q",
            "https://github.com/aperod-network/aperod-node", str(candidate))
        head = git(candidate, "rev-parse", "HEAD").decode().strip()
        tree = prepare(args.root.resolve(), candidate, args.gate_only)
        if args.check_drift:
            changed = [x for x in git(candidate, "diff", "--cached", "--name-only", "-z").split(b"\0") if x]
            print(f"Complete source-distribution drift: {len(changed)} files.")
            if changed:
                raise SystemExit(1)
        elif args.dry_run:
            verify(candidate, tree, with_tests=args.with_tests)
            print("Dry-run verified; no objects uploaded.")
        else:
            request = make_request(token)
            pr = publish(candidate, tree, head, request, args.with_tests)
            if pr and not args.no_wait:
                await_merge(pr, request)


if __name__ == "__main__":
    main()