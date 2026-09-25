#!/usr/bin/env python3
"""Generate bounded, original synthetic fixtures; no upstream media downloads."""

import argparse
import os
import subprocess
import sys
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("--force", action="store_true")
parser.add_argument("--output", default=".local/gateway/probes")
args = parser.parse_args()
root = Path(args.output)
root.mkdir(parents=True, exist_ok=True)
expected = [
    "baseline-360.mp4",
    "baseline-480.mp4",
    "main-720.mp4",
    "high-720.mp4",
    "high-1080.mp4",
    "aac.m4a",
    "aac.adts",
    "baseline.ts",
    "fragmented.mp4",
]
if not args.force and all(
    (root / name).is_file()
    and not (root / name).is_symlink()
    and 100 < (root / name).stat().st_size <= 8 << 20
    for name in expected
):
    print(
        f"Reusing {len(expected)} probe fixtures in {root}; pass --force to regenerate"
    )
    sys.exit(0)
base = ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y"]
video = [
    ("-360", "640x360", "baseline", "3.0"),
    ("-480", "854x480", "baseline", "3.1"),
    ("-720", "1280x720", "main", "3.1"),
    ("-720", "1280x720", "high", "3.1"),
    ("-1080", "1920x1080", "high", "4.0"),
]
for suffix, size, profile, level in video:
    target = root / (profile + suffix + ".mp4")
    if (
        not args.force
        and target.is_file()
        and not target.is_symlink()
        and 100 < target.stat().st_size <= 8 << 20
    ):
        continue
    temporary = target.with_suffix(".tmp.mp4")
    subprocess.run(
        base
        + [
            "-f",
            "lavfi",
            "-i",
            f"testsrc2=size={size}:rate=30",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=440:sample_rate=44100",
            "-t",
            "3",
            "-c:v",
            "libx264",
            "-threads",
            "2",
            "-preset",
            "veryfast",
            "-profile:v",
            profile,
            "-level:v",
            level,
            "-pix_fmt",
            "yuv420p",
            "-b:v",
            "1200k",
            "-maxrate",
            "1500k",
            "-bufsize",
            "3000k",
            "-c:a",
            "aac",
            "-b:a",
            "96k",
            "-ac",
            "2",
            "-movflags",
            "+faststart",
            str(temporary),
        ],
        check=True,
        timeout=60,
    )
    os.replace(temporary, target)
for name, options in [
    ("aac.m4a", ["-vn", "-c:a", "copy"]),
    ("aac.adts", ["-vn", "-c:a", "copy", "-f", "adts"]),
    ("baseline.ts", ["-c", "copy", "-f", "mpegts"]),
    (
        "fragmented.mp4",
        ["-c", "copy", "-movflags", "+frag_keyframe+empty_moov+default_base_moof"],
    ),
]:
    target = root / name
    if (
        not args.force
        and target.is_file()
        and not target.is_symlink()
        and 100 < target.stat().st_size <= 8 << 20
    ):
        continue
    temporary = target.with_name("tmp-" + name)
    subprocess.run(
        base + ["-i", str(root / "baseline-360.mp4")] + options + [str(temporary)],
        check=True,
        timeout=20,
    )
    os.replace(temporary, target)
print(f"Generated {len(expected)} probe fixtures in {root}")
