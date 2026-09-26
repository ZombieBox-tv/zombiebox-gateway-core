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
    requested_quality: str = "",
) -> Dict[str, Any]:
    """Fall back to the upstream baseline YouTube worker.

    Always requests upstream's reliable baseline progressive 360p H.264/AAC stream.
    Even if a manual HD quality (e.g., '1080p' or '720p') was requested by the client,
    we do NOT forward that HD quality parameter to upstream. The upstream worker lacks
    per-video PO tokens and cannot reliably fulfill HD requests without failing (502)
    or returning unplayable/throttled streams.

    By querying upstream for its baseline without an HD quality parameter, the user's
    functional DIAL 360p playback path is preserved with zero interruption.
    """
    target = f"{upstream_url.rstrip('/')}/resolve/{video_id}"
    # Do NOT pass ?quality={requested_quality} for HD requests. The baseline endpoint
    # resolves the working 360p stream safely.

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
    """Resolve a YouTube video using per-video PO tokens, falling back to 360p baseline on error/403."""
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

    # If in 403 cooldown, skip extraction immediately and use upstream baseline
    if cooldown_tracker.is_in_cooldown():
        return fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=timeout_sec,
            fetch_func=upstream_fetch_func,
            requested_quality=quality,
        )

    extractor = extractor_func or run_ytdlp_extraction
    try:
        meta = extractor(video_id, bgutil_url, timeout_sec)
    except PermissionError:
        cooldown_tracker.record_403()
        return fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=timeout_sec,
            fetch_func=upstream_fetch_func,
            requested_quality=quality,
        )
    except Exception:
        # Provider unavailable, timeout, or parsing error -> fallback to upstream baseline
        return fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=timeout_sec,
            fetch_func=upstream_fetch_func,
            requested_quality=quality,
        )

    formats = meta.get("formats", [])
    selector = FormatSelector(formats, fetch_func=range_fetch_func)
    resolved, _, saw_403 = selector.determine_variants_and_resolve(quality)

    if saw_403:
        cooldown_tracker.record_403()
        return fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=timeout_sec,
            fetch_func=upstream_fetch_func,
            requested_quality=quality,
        )

    if not resolved or not resolved.get("url"):
        return fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=timeout_sec,
            fetch_func=upstream_fetch_func,
            requested_quality=quality,
        )

    return resolved
