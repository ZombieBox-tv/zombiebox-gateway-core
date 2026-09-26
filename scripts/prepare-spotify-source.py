#!/usr/bin/env python3
"""Stage the pinned Spotify worker with the licensed decoder and key-refusal patches."""

import argparse
import pathlib
import subprocess
import tarfile
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
UPSTREAM = "6a3e25019de8d2893b3fa26b0273d8cc376241c5"
PATCHES = [
    ROOT / "wrappers/spotify/patches/licensed-vorbis.patch",
    ROOT / "wrappers/spotify/patches/stop-key-refusal-skip.patch",
]


def stage(source: pathlib.Path, output: pathlib.Path) -> None:
    resolved = subprocess.check_output(
        ["git", "-C", str(source), "rev-parse", f"{UPSTREAM}^{{commit}}"],
        text=True,
    ).strip()
    if resolved != UPSTREAM:
        raise ValueError(f"Expected go-librespot commit {UPSTREAM}; got {resolved}")
    dirty = subprocess.check_output(
        ["git", "-C", str(source), "status", "--porcelain"], text=True
    ).strip()
    if dirty:
        raise ValueError("Upstream checkout is modified")
    if output.exists():
        raise ValueError(f"Output already exists: {output}")

    with tempfile.TemporaryDirectory() as temporary:
        archive = pathlib.Path(temporary) / "upstream.tar"
        subprocess.run(
            ["git", "-C", str(source), "archive", UPSTREAM, "-o", str(archive)],
            check=True,
        )
        output.mkdir(parents=True)
        try:
            with tarfile.open(archive) as contents:
                contents.extractall(output, filter="data")
            for patch in PATCHES:
                subprocess.run(
                    ["patch", "--batch", "--forward", "-p1", "-i", str(patch)],
                    cwd=output,
                    check=True,
                    stdout=subprocess.DEVNULL,
                )
            if "github.com/xlab/vorbis-go" in (output / "go.mod").read_text():
                raise ValueError("Unlicensed Vorbis binding remains in go.mod")
            for code in (output / "vorbis").glob("*.go"):
                if "github.com/xlab/vorbis-go" in code.read_text():
                    raise ValueError(f"Unlicensed Vorbis binding remains in {code}")
            (output / "ZOMBIE_UPSTREAM_COMMIT").write_text(UPSTREAM + "\n")
            (output / "ZOMBIE_PATCH").write_text(PATCHES[0].name + "\n")
            (output / "ZOMBIE_PATCHES").write_text(
                "\n".join(patch.name for patch in PATCHES) + "\n"
            )
        except BaseException:
            import shutil

            shutil.rmtree(output)
            raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    arguments = parser.parse_args()
    stage(arguments.source, arguments.output)


if __name__ == "__main__":
    main()
