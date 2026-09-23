#!/usr/bin/env python3
"""Stage the pinned Spotify worker with the licensed Vorbis decoder patch."""

import argparse
import pathlib
import subprocess
import tarfile
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
UPSTREAM = "57d7278d94a9233060c2a6238f5926ffd1e72de4"
PATCH = ROOT / "wrappers/spotify/patches/licensed-vorbis.patch"


def stage(source: pathlib.Path, output: pathlib.Path) -> None:
    current = subprocess.check_output(
        ["git", "-C", str(source), "rev-parse", "HEAD"], text=True
    ).strip()
    if current != UPSTREAM:
        raise ValueError(f"Expected go-librespot {UPSTREAM}; got {current}")
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
            ["git", "-C", str(source), "archive", "HEAD", "-o", str(archive)],
            check=True,
        )
        output.mkdir(parents=True)
        try:
            with tarfile.open(archive) as contents:
                contents.extractall(output, filter="data")
            subprocess.run(
                ["patch", "--batch", "--forward", "-p1", "-i", str(PATCH)],
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
            (output / "ZOMBIE_PATCH").write_text(PATCH.name + "\n")
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
