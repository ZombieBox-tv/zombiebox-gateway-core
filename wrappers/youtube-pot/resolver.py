"""YouTube video resolution orchestrator with PO token extraction and fallback."""

from __future__ import annotations

import json
import os
import re
import selectors
import signal
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Dict, Optional, Tuple

from cooldown import CooldownTracker
from formats import FormatSelector
from media_ranges import _NO_REDIRECT_OPENER

YOUTUBE_ID_PATTERN = re.compile(r"^[A-Za-z0-9_-]{11}$")
DEFAULT_EXTRACTION_TIMEOUT_SECONDS = 12
MAX_EXTRACTION_TIMEOUT_SECONDS = 30
MAX_EXTRACTION_STDOUT_BYTES = 4 * 1024 * 1024
MAX_EXTRACTION_STDERR_BYTES = 256 * 1024
PROCESS_READ_CHUNK_BYTES = 64 * 1024
MAX_UPSTREAM_RESPONSE_BYTES = 1024 * 1024  # 1 MiB
MANUAL_QUALITIES = {"1080p", "720p", "480p", "360p"}


class QualityUnavailableError(RuntimeError):
    """A requested exact resolution could not be validated and delivered."""

    def __init__(self) -> None:
        # Keep this message independent of extractor output and signed media URLs.
        super().__init__("Requested quality is unavailable")


def _quality_for_height(height: int) -> Optional[str]:
    """Map numeric pixel height to the highest standard tier it fully reaches."""
    for tier, minimum_height in (
        ("2160p", 2160),
        ("1440p", 1440),
        ("1080p", 1080),
        ("720p", 720),
        ("480p", 480),
        ("360p", 360),
    ):
        if height >= minimum_height:
            return tier
    return None


class _ProcessOutputLimitError(RuntimeError):
    """Raised when a subprocess exceeds its bounded stdout or stderr budget."""


def _terminate_process_group(process: subprocess.Popen) -> None:
    """Stop yt-dlp and any runtime child process it started."""
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass

    try:
        process.wait(timeout=0.5)
    except subprocess.TimeoutExpired:
        pass

    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass

    try:
        process.wait(timeout=1)
    except subprocess.TimeoutExpired:
        pass


def run_bounded_command(
    command: list[str],
    timeout_seconds: float,
    max_stdout_bytes: int = MAX_EXTRACTION_STDOUT_BYTES,
    max_stderr_bytes: int = MAX_EXTRACTION_STDERR_BYTES,
) -> subprocess.CompletedProcess:
    """Capture a bounded amount of output and stop a timed-out process tree."""
    process = subprocess.Popen(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    selector = selectors.DefaultSelector()
    output = {"stdout": bytearray(), "stderr": bytearray()}
    limits = {"stdout": max_stdout_bytes, "stderr": max_stderr_bytes}
    streams = {"stdout": process.stdout, "stderr": process.stderr}
    deadline = time.monotonic() + timeout_seconds

    try:
        for name, stream in streams.items():
            selector.register(stream, selectors.EVENT_READ, name)

        while selector.get_map():
            remaining_time = deadline - time.monotonic()
            if remaining_time <= 0:
                raise TimeoutError("yt-dlp extraction timed out")

            for key, _ in selector.select(remaining_time):
                name = key.data
                remaining_bytes = limits[name] - len(output[name])
                chunk = os.read(
                    key.fileobj.fileno(),
                    min(PROCESS_READ_CHUNK_BYTES, remaining_bytes + 1),
                )
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                if len(chunk) > remaining_bytes:
                    raise _ProcessOutputLimitError("yt-dlp output exceeded size limit")
                output[name].extend(chunk)

        remaining_time = deadline - time.monotonic()
        if remaining_time <= 0:
            raise TimeoutError("yt-dlp extraction timed out")
        try:
            return_code = process.wait(timeout=remaining_time)
        except subprocess.TimeoutExpired as exc:
            raise TimeoutError("yt-dlp extraction timed out") from exc
        return subprocess.CompletedProcess(
            command,
            return_code,
            stdout=bytes(output["stdout"]),
            stderr=bytes(output["stderr"]),
        )
    except Exception:
        _terminate_process_group(process)
        raise
    finally:
        selector.close()
        for stream in streams.values():
            if stream is not None:
                stream.close()


def run_ytdlp_extraction(
    video_id: str,
    bgutil_url: str,
    timeout_seconds: int = DEFAULT_EXTRACTION_TIMEOUT_SECONDS,
) -> Dict[str, Any]:
    """Run mweb extraction so BgUtils supplies its GVS PO token for format URLs."""
    video_url = f"https://www.youtube.com/watch?v={video_id}"
    cmd = [
        "yt-dlp",
        "--dump-single-json",
        "--no-warnings",
        "--no-playlist",
        "--skip-download",
        "--js-runtimes",
        "node",
        "--extractor-args",
        "youtube:player_client=mweb",
        "--extractor-args",
        f"youtubepot-bgutilhttp:base_url={bgutil_url}",
        video_url,
    ]

    timeout = min(
        MAX_EXTRACTION_TIMEOUT_SECONDS,
        max(1, int(timeout_seconds)),
    )
    try:
        proc = run_bounded_command(
            cmd,
            timeout_seconds=timeout,
        )
    except TimeoutError:
        raise
    except Exception as exc:
        if isinstance(exc, _ProcessOutputLimitError):
            raise RuntimeError("yt-dlp output exceeded size limit") from None
        raise RuntimeError("Failed to execute yt-dlp") from exc

    if proc.returncode != 0:
        stderr = (proc.stderr or b"").decode("utf-8", errors="replace")
        if "403" in stderr or "confirm you're not a bot" in stderr.lower():
            raise PermissionError("YouTube returned 403 or bot detection")
        raise RuntimeError(f"yt-dlp exited with code {proc.returncode}")

    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise RuntimeError("Invalid JSON output from yt-dlp") from exc


def fallback_to_upstream(
    video_id: str,
    upstream_url: str,
    token: str,
    timeout_seconds: int = 10,
    fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
) -> Dict[str, Any]:
    """Resolve the upstream baseline stream for automatic or unspecified quality."""
    target = f"{upstream_url.rstrip('/')}/resolve/{video_id}"

    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/json",
    }

    if fetch_func is not None:
        status, _, body = fetch_func(target, headers)
        if status != 200:
            raise RuntimeError(f"Upstream returned HTTP {status}")
        if len(body) > MAX_UPSTREAM_RESPONSE_BYTES:
            raise RuntimeError("Upstream response exceeded size limit")
        return json.loads(body.decode("utf-8"))

    req = urllib.request.Request(target, headers=headers, method="GET")
    with _NO_REDIRECT_OPENER.open(req, timeout=timeout_seconds) as resp:
        status = getattr(resp, "status", getattr(resp, "code", 0))
        if status != 200:
            raise RuntimeError(f"Upstream returned HTTP {status}")
        raw_body = resp.read(MAX_UPSTREAM_RESPONSE_BYTES + 1)
        if len(raw_body) > MAX_UPSTREAM_RESPONSE_BYTES:
            raise RuntimeError("Upstream response exceeded size limit")
        return json.loads(raw_body.decode("utf-8"))


def resolve_video(
    video_id: str,
    quality: str,
    config: Dict[str, Any],
    cooldown_tracker: CooldownTracker,
    extractor_func: Optional[Callable[[str, str, int], Dict[str, Any]]] = None,
    range_fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
    upstream_fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
) -> Dict[str, Any]:
    """Resolve a YouTube stream without silently degrading explicit quality requests."""
    if not YOUTUBE_ID_PATTERN.match(video_id):
        raise ValueError("Invalid YouTube video ID")

    upstream_url = config.get("upstream_url", "http://youtube:8091")
    bgutil_url = config.get("bgutil_url", "http://bgutil-provider:4416")
    token = config.get("token", "")
    timeout_sec = min(
        MAX_EXTRACTION_TIMEOUT_SECONDS,
        max(
            1,
            int(config.get("timeout_seconds", DEFAULT_EXTRACTION_TIMEOUT_SECONDS)),
        ),
    )

    def fallback_or_fail() -> Dict[str, Any]:
        if quality in MANUAL_QUALITIES:
            raise QualityUnavailableError() from None
        return fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=timeout_sec,
            fetch_func=upstream_fetch_func,
        )

    # A manual tier must be served exactly; an unrelated 360p fallback is not success.
    if cooldown_tracker.is_in_cooldown():
        return fallback_or_fail()

    extractor = extractor_func or run_ytdlp_extraction
    try:
        meta = extractor(video_id, bgutil_url, timeout_sec)
    except PermissionError:
        cooldown_tracker.record_403()
        return fallback_or_fail()
    except Exception:
        # Provider unavailable, timeout, or parsing error -> baseline only for Auto.
        return fallback_or_fail()

    if not isinstance(meta, dict):
        return fallback_or_fail()

    formats = meta.get("formats", [])
    if not isinstance(formats, list):
        return fallback_or_fail()
    selector = FormatSelector(formats, fetch_func=range_fetch_func)
    try:
        resolved, _, saw_403 = selector.determine_variants_and_resolve(quality)
    except Exception:
        # A malformed or unreachable rendition has the same exact-tier semantics.
        return fallback_or_fail()

    if not resolved or not resolved.get("url"):
        if saw_403:
            cooldown_tracker.record_403()
        return fallback_or_fail()

    actual_quality = None
    matching_heights = {
        candidate.get("height")
        for candidate in formats
        if isinstance(candidate, dict) and candidate.get("url") == resolved["url"]
    }
    if len(matching_heights) == 1:
        height = next(iter(matching_heights))
        if type(height) is int and height > 0:
            # Report resolution only from yt-dlp's numeric format metadata. Labels
            # alone are not sufficient evidence for the actual selected rendition.
            resolved["actualHeight"] = height
            actual_quality = _quality_for_height(height)
            if actual_quality:
                resolved["actualQuality"] = actual_quality

    if quality in MANUAL_QUALITIES and actual_quality != quality:
        return fallback_or_fail()

    return resolved
