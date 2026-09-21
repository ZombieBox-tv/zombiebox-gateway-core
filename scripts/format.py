#!/usr/bin/env python3
"""Format first-party code with pinned tools; never traverse reference checkouts."""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
CACHE = ROOT / ".tools/format"
KTFMT_VERSION = "0.59"
KTFMT_SHA256 = "a493271c137074436a133d72e9e323f91604c97e0c40555629ec39598efead54"
GO_TOOLS = {
    "goimports": "golang.org/x/tools/cmd/goimports@v0.36.0",
    "shfmt": "mvdan.cc/sh/v3/cmd/shfmt@v3.12.0",
}


def run(*args, **kwargs):
    subprocess.run([str(arg) for arg in args], cwd=ROOT, check=True, **kwargs)


def go_tool(name):
    binary = CACHE / name
    marker = CACHE / f"{name}.version"
    version = GO_TOOLS[name]
    if not binary.exists() or not marker.exists() or marker.read_text() != version:
        run(
            "go",
            "install",
            version,
            env={
                **os.environ,
                "GOBIN": str(CACHE),
                "GOMAXPROCS": "2",
                "GOFLAGS": "-p=2",
            },
        )
        marker.write_text(version)
    return binary


def kotlin_tool():
    jar = CACHE / f"ktfmt-{KTFMT_VERSION}.jar"
    if not jar.exists():
        url = f"https://repo.maven.apache.org/maven2/com/facebook/ktfmt/{KTFMT_VERSION}/ktfmt-{KTFMT_VERSION}-with-dependencies.jar"
        with urllib.request.urlopen(url, timeout=60) as response:
            temporary = jar.with_suffix(".tmp")
            temporary.write_bytes(response.read(80 << 20))
        temporary.replace(jar)
    if hashlib.sha256(jar.read_bytes()).hexdigest() != KTFMT_SHA256:
        raise SystemExit(f"Formatter checksum mismatch: {jar}")
    return jar


def prettier_tool():
    folder = ROOT / "tooling/format"
    marker = CACHE / "prettier.lock"
    checksum = hashlib.sha256((folder / "package-lock.json").read_bytes()).hexdigest()
    binary = folder / "node_modules/.bin/prettier"
    if not binary.exists() or not marker.exists() or marker.read_text() != checksum:
        run(
            "npm",
            "ci",
            "--prefix",
            folder,
            "--ignore-scripts",
            "--no-audit",
            "--no-fund",
        )
        marker.write_text(checksum)
    return binary


def sources():
    listing = subprocess.check_output(
        ["git", "ls-files", "-co", "--exclude-standard", "-z"], cwd=ROOT
    )
    return sorted(
        {
            Path(name)
            for name in listing.decode().split("\0")
            if name
            and Path(ROOT / name).is_file()
            and not name.startswith(("resources/", "third_party/"))
            and Path(name).name
            not in ("gradlew", "package-lock.json", "seccomp_profile.json")
        }
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument(
        "--language",
        choices=["go", "kotlin", "python", "javascript", "shell"],
        action="append",
    )
    args = parser.parse_args()
    languages = args.language or ["go", "kotlin", "python", "javascript", "shell"]
    CACHE.mkdir(parents=True, exist_ok=True)
    files = sources()
    for language in languages:
        if language == "go":
            paths = [path for path in files if path.suffix == ".go"]
            tool = go_tool("goimports")
            if args.check:
                dirty = subprocess.check_output(
                    [
                        str(tool),
                        "-local",
                        "zombiebox.local/gateway",
                        "-l",
                        *map(str, paths),
                    ],
                    cwd=ROOT,
                    text=True,
                )
                if dirty:
                    raise SystemExit("Go files need formatting:\n" + dirty)
            else:
                run(tool, "-local", "zombiebox.local/gateway", "-w", *paths)
        elif language == "kotlin":
            paths = [path for path in files if path.suffix in (".kt", ".kts")]
            options = ["--dry-run", "--set-exit-if-changed"] if args.check else []
            run(
                "java",
                "-Xmx512m",
                "-jar",
                kotlin_tool(),
                "--kotlinlang-style",
                *options,
                *paths,
            )
        elif language == "python":
            paths = [path for path in files if path.suffix == ".py"]
            run(
                "uvx",
                "--from",
                "ruff==0.13.2",
                "ruff",
                "format",
                *(["--check"] if args.check else []),
                *paths,
            )
        elif language == "javascript":
            paths = [
                path
                for path in files
                if path.suffix in (".mjs", ".js", ".json", ".yaml", ".yml", ".xml")
            ]
            run(
                prettier_tool(),
                "--config",
                "tooling/format/prettier.json",
                "--check" if args.check else "--write",
                *paths,
            )
        elif language == "shell":
            paths = [path for path in files if path.suffix == ".sh"]
            run(
                go_tool("shfmt"), "-i", "4", "-ci", "-d" if args.check else "-w", *paths
            )


if __name__ == "__main__":
    main()
