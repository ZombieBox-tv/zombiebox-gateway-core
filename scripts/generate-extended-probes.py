#!/usr/bin/env python3
"""Generate bounded SDR/30fps fixtures on the gateway, never inside an APK."""

import argparse
import json
import os
import subprocess
from pathlib import Path


def valid_fixture(path, size, profile, codec, level):
    if (
        not path.is_file()
        or path.is_symlink()
        or not 100 < path.stat().st_size <= 8 << 20
    ):
        return False
    try:
        info = json.loads(
            subprocess.check_output(
                [
                    "ffprobe",
                    "-v",
                    "error",
                    "-select_streams",
                    "v:0",
                    "-show_entries",
                    "stream=codec_name,codec_tag_string,profile,level,width,height,color_transfer,color_primaries,r_frame_rate,avg_frame_rate,pix_fmt,duration",
                    "-of",
                    "json",
                    str(path),
                ],
                timeout=15,
            )
        )["streams"][0]
        return (
            f"{info['width']}x{info['height']}" == size
            and info.get("profile", "").lower() == profile
            and info.get("codec_name") == ("hevc" if codec == "libx265" else "h264")
            and info.get("codec_tag_string")
            == ("hvc1" if codec == "libx265" else "avc1")
            and info.get("level")
            == round(float(level) * (30 if codec == "libx265" else 10))
            and info.get("color_transfer") == "bt709"
            and info.get("color_primaries") == "bt709"
            and info.get("r_frame_rate") == "30/1"
            and info.get("avg_frame_rate") == "30/1"
            and info.get("pix_fmt") == "yuv420p"
            and 2.9 <= float(info.get("duration", "0")) <= 3.1
        )
    except (subprocess.SubprocessError, ValueError, KeyError, IndexError):
        return False


def generate(root):
    root.mkdir(parents=True, exist_ok=True)
    for name, size, codec, profile, level, bitrate in (
        ("high-2160.mp4", "3840x2160", "libx264", "high", "5.1", "12M"),
        ("hevc-1080.mp4", "1920x1080", "libx265", "main", "4.0", "4M"),
        ("hevc-2160.mp4", "3840x2160", "libx265", "main", "5.0", "12M"),
    ):
        target = root / name
        if valid_fixture(target, size, profile, codec, level):
            continue
        temporary = target.with_suffix(".tmp.mp4")
        params = ["-level:v", level]
        if codec == "libx265":
            params = [
                "-tag:v",
                "hvc1",
                "-x265-params",
                f"pools=2:frame-threads=1:level-idc={level}:high-tier=0:log-level=error",
            ]
        try:
            subprocess.run(
                [
                    "ffmpeg",
                    "-nostdin",
                    "-hide_banner",
                    "-loglevel",
                    "error",
                    "-y",
                    "-f",
                    "lavfi",
                    "-i",
                    f"testsrc2=size={size}:rate=30",
                    "-t",
                    "3",
                    "-an",
                    "-vf",
                    "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709",
                    "-c:v",
                    codec,
                    "-threads",
                    "2",
                    "-preset",
                    "veryfast",
                    "-profile:v",
                    profile,
                    "-pix_fmt",
                    "yuv420p",
                    "-color_trc",
                    "bt709",
                    "-color_primaries",
                    "bt709",
                    "-colorspace",
                    "bt709",
                    "-b:v",
                    bitrate,
                    "-maxrate",
                    bitrate,
                    "-bufsize",
                    bitrate,
                    *params,
                    "-movflags",
                    "+faststart",
                    str(temporary),
                ],
                check=True,
                timeout=180,
            )
            if not valid_fixture(temporary, size, profile, codec, level):
                raise ValueError(
                    "Extended fixture differs from its bounded SDR/30fps contract"
                )
            os.replace(temporary, target)
        finally:
            temporary.unlink(missing_ok=True)
    print(
        f"Extended SDR/30fps probes prepared in {root}; actual decoding remains unverified"
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    generate(parser.parse_args().output)
