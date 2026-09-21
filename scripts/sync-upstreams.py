#!/usr/bin/env python3
"""Restore exact reference commits; never install upstream dependencies or run code."""

import json
import pathlib
import re
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]


def git(*args, cwd=None):
    return subprocess.check_output(["git", *args], cwd=cwd, text=True).strip()


def main():
    manifest = json.loads((ROOT / "third_party/upstreams.lock.json").read_text())
    base = ROOT / "third_party/sources"
    base.mkdir(parents=True, exist_ok=True)
    for entry in manifest["repositories"]:
        name, commit, url = entry["name"], entry["commit"], entry["url"]
        if not re.fullmatch(r"[a-zA-Z0-9_-]+", name) or not re.fullmatch(
            r"[a-f0-9]{40}", commit
        ):
            raise ValueError(f"Invalid lock entry: {name}")
        dest = base / name
        if not dest.exists():
            dest.mkdir()
            git("init", "--quiet", cwd=dest)
            git("remote", "add", "origin", url, cwd=dest)
            git("config", "remote.origin.promisor", "true", cwd=dest)
            git("config", "remote.origin.partialclonefilter", "blob:none", cwd=dest)
            if entry.get("sparse"):
                git("sparse-checkout", "init", "--cone", cwd=dest)
                git("sparse-checkout", "set", *entry["sparse"], cwd=dest)
        if git("remote", "get-url", "origin", cwd=dest) != url:
            raise RuntimeError(f"Unexpected origin; preserving {dest}")
        if git("status", "--porcelain", cwd=dest):
            raise RuntimeError(f"Local modifications; preserving {dest}")
        current = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=dest, text=True, capture_output=True
        )
        if current.returncode == 0 and current.stdout.strip() != commit:
            raise RuntimeError(
                f"Different checkout; preserving {dest}. Review manually before changing revisions."
            )
        if current.returncode != 0:
            git(
                "fetch",
                "--quiet",
                "--depth=1",
                "--filter=blob:none",
                "origin",
                commit,
                cwd=dest,
            )
            git("checkout", "--quiet", "--detach", commit, cwd=dest)
        print(f"OK {name}: {commit}", flush=True)


if __name__ == "__main__":
    main()
