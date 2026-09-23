#!/usr/bin/env python3
"""Check the pinned Spotify patch with synthetic metadata, decode, gain and seek."""

import os
import pathlib
import shutil
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
PREPARE = ROOT / "scripts/prepare-spotify-source.py"
TEST = ROOT / "wrappers/spotify/patches/decoder_integration_test.go"
SOURCE = ROOT / "third_party/sources/go-librespot"


def main() -> None:
    if not SOURCE.is_dir():
        raise SystemExit("Run make references before testing the Spotify patch")
    with tempfile.TemporaryDirectory(prefix="zombie-spotify-patch-") as temporary:
        root = pathlib.Path(temporary)
        staged = root / "go-librespot"
        tone = root / "tone.ogg"
        subprocess.run(
            ["python3", str(PREPARE), "--source", str(SOURCE), "--output", str(staged)],
            check=True,
        )
        subprocess.run(
            [
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-f",
                "lavfi",
                "-i",
                "sine=frequency=440:duration=2",
                "-c:a",
                "libvorbis",
                "-ac",
                "2",
                "-y",
                str(tone),
            ],
            check=True,
        )
        shutil.copy2(TEST, staged / "vorbis/decoder_integration_test.go")
        environment = dict(
            os.environ, ZOMBIE_SPOTIFY_TEST_OGG=str(tone), GOMAXPROCS="2"
        )
        subprocess.run(
            ["go", "test", "-mod=readonly", "-p", "2", "./vorbis"],
            cwd=staged,
            env=environment,
            check=True,
        )
        modules = subprocess.check_output(
            ["go", "list", "-m", "all"], cwd=staged, env=environment, text=True
        )
        if "github.com/xlab/vorbis-go" in modules:
            raise RuntimeError("Unlicensed Vorbis binding remains in module graph")


if __name__ == "__main__":
    main()
