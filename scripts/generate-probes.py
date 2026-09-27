#!/usr/bin/env python3
"""Generate bounded, original synthetic fixtures; no upstream media downloads."""

import argparse
import os
import subprocess
import sys
import tempfile
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
    "mpegts-aac.ts",
    "mp3.mp3",
    "baseline.ts",
    "fragmented.mp4",
] + [f"hls-event-{index:02d}.ts" for index in range(7)]
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
    ("mpegts-aac.ts", ["-vn", "-c:a", "copy", "-f", "mpegts"]),
    ("mp3.mp3", ["-vn", "-c:a", "libmp3lame", "-b:a", "128k", "-f", "mp3"]),
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

event_segments = [root / f"hls-event-{index:02d}.ts" for index in range(7)]
if args.force or any(
    not segment.is_file()
    or segment.is_symlink()
    or not 100 < segment.stat().st_size <= 8 << 20
    for segment in event_segments
):
    with tempfile.TemporaryDirectory(prefix=".hls-event-", dir=root) as temporary_dir:
        temporary_pattern = str(Path(temporary_dir) / "hls-event-%02d.ts")
        subprocess.run(
            base
            + [
                "-f",
                "lavfi",
                "-i",
                "testsrc2=size=1280x720:rate=30",
                "-f",
                "lavfi",
                "-i",
                "sine=frequency=660:sample_rate=48000",
                "-t",
                "28",
                "-c:v",
                "libx264",
                "-threads",
                "2",
                "-preset",
                "veryfast",
                "-profile:v",
                "high",
                "-level:v",
                "3.1",
                "-pix_fmt",
                "yuv420p",
                "-g",
                "120",
                "-keyint_min",
                "120",
                "-sc_threshold",
                "0",
                "-force_key_frames",
                "expr:gte(t,n_forced*4)",
                "-b:v",
                "1800k",
                "-maxrate",
                "2200k",
                "-bufsize",
                "4400k",
                "-c:a",
                "aac",
                "-b:a",
                "128k",
                "-ac",
                "2",
                "-ar",
                "48000",
                "-f",
                "segment",
                "-segment_time",
                "4",
                "-segment_time_delta",
                "0.05",
                "-segment_format",
                "mpegts",
                "-reset_timestamps",
                "0",
                "-segment_start_number",
                "0",
                temporary_pattern,
            ],
            check=True,
            timeout=120,
        )
        generated_segments = [
            Path(temporary_dir) / f"hls-event-{index:02d}.ts" for index in range(7)
        ]
        for temporary, target in zip(generated_segments, event_segments, strict=True):
            if (
                not temporary.is_file()
                or temporary.is_symlink()
                or not 100 < temporary.stat().st_size <= 8 << 20
            ):
                raise RuntimeError(f"invalid HLS EVENT probe segment: {temporary.name}")
        for temporary, target in zip(generated_segments, event_segments, strict=True):
            os.replace(temporary, target)
print(f"Generated {len(expected)} probe fixtures in {root}")
