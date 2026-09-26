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
from media_ranges import (
    _NO_REDIRECT_OPENER,
    RANGE_SAMPLE_BYTES,
    SAMPLE_TIMEOUT_SECONDS,
)

YOUTUBE_ID_PATTERN = re.compile(r"^[A-Za-z0-9_-]{11}$")
DEFAULT_EXTRACTION_TIMEOUT_SECONDS = 12
MAX_EXTRACTION_TIMEOUT_SECONDS = 30
MAX_EXTRACTION_STDOUT_BYTES = 4 * 1024 * 1024
MAX_EXTRACTION_STDERR_BYTES = 256 * 1024
PROCESS_READ_CHUNK_BYTES = 64 * 1024
MAX_UPSTREAM_RESPONSE_BYTES = 1024 * 1024  # 1 MiB
MAX_RESOLUTION_BUDGET_SECONDS = 18.0
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


class _ResolutionBudget:
    """Share a hard wall-clock deadline across extraction and range validation."""

    def __init__(
        self,
        deadline: float,
        fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
    ):
        self.deadline = deadline
        self.fetch_func = fetch_func
        self.expired = False

    def remaining_seconds(self) -> float:
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            self.expired = True
            return 0.0
        return remaining

    def range_fetch(self, url: str, headers: dict) -> Tuple[int, dict, bytes]:
        """Fetch one validated range with no more than the remaining budget."""
        remaining = self.remaining_seconds()
        if remaining <= 0:
            raise TimeoutError("YouTube resolution budget expired")

        if self.fetch_func is not None:
            result = self.fetch_func(url, headers)
        else:
            request = urllib.request.Request(url, headers=headers, method="GET")
            with _NO_REDIRECT_OPENER.open(
                request,
                timeout=min(SAMPLE_TIMEOUT_SECONDS, remaining),
            ) as response:
                status = getattr(response, "status", getattr(response, "code", 0))
                response_headers = {
                    key.lower(): value for key, value in response.headers.items()
                }
                body = (
                    b""
                    if 300 <= status < 400
                    else response.read(RANGE_SAMPLE_BYTES + 1)
                )
                result = status, response_headers, body

        if self.remaining_seconds() <= 0:
            raise TimeoutError("YouTube resolution budget expired")
        return result


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


def _run_ytdlp_json(cmd: list[str], timeout_seconds: int) -> Dict[str, Any]:
    """Execute yt-dlp with shared timeout, output, and error handling limits."""
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
    return _run_ytdlp_json(cmd, timeout_seconds)


def run_default_ytdlp_extraction(
    video_id: str,
    timeout_seconds: int = DEFAULT_EXTRACTION_TIMEOUT_SECONDS,
) -> Dict[str, Any]:
    """Extract with yt-dlp's default clients, without loading PO-token plugins."""
    video_url = f"https://www.youtube.com/watch?v={video_id}"
    cmd = [
        "yt-dlp",
        "--no-plugin-dirs",
        "--dump-single-json",
        "--no-warnings",
        "--no-playlist",
        "--skip-download",
        "--js-runtimes",
        "node",
        video_url,
    ]
    return _run_ytdlp_json(cmd, timeout_seconds)


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
    default_extractor_func: Optional[Callable[[str, int], Dict[str, Any]]] = None,
) -> Dict[str, Any]:
    """Try default YouTube clients, then PO extraction, without degrading manual quality."""
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
    budget_seconds = min(MAX_RESOLUTION_BUDGET_SECONDS, timeout_sec * 1.5)
    budget = _ResolutionBudget(time.monotonic() + budget_seconds, range_fetch_func)

    def extraction_timeout() -> Optional[int]:
        remaining = budget.remaining_seconds()
        if remaining < 1.0:
            return None
        return min(timeout_sec, max(1, int(remaining)))

    def fallback_or_fail() -> Dict[str, Any]:
        if quality in MANUAL_QUALITIES:
            raise QualityUnavailableError() from None
        remaining = budget.remaining_seconds()
        if remaining <= 0:
            raise TimeoutError("YouTube resolution budget expired")
        result = fallback_to_upstream(
            video_id,
            upstream_url,
            token,
            timeout_seconds=remaining,
            fetch_func=upstream_fetch_func,
        )
        if budget.remaining_seconds() <= 0:
            raise TimeoutError("YouTube resolution budget expired")
        return result

    def resolve_candidate(meta: Any) -> Tuple[Optional[Dict[str, Any]], bool]:
        """Validate one extractor's formats and enforce numeric manual-tier truth."""
        if budget.remaining_seconds() <= 0:
            return None, False
        if not isinstance(meta, dict):
            return None, False

        formats = meta.get("formats", [])
        if not isinstance(formats, list):
            return None, False
        selector = FormatSelector(
            formats,
            fetch_func=budget.range_fetch,
            deadline=budget.deadline,
        )
        try:
            resolved, _, saw_403 = selector.determine_variants_and_resolve(quality)
        except Exception:
            # A malformed or unreachable rendition can be retried through the other
            # extractor, while manual quality still fails closed if neither works.
            return None, False

        if selector.budget_expired or budget.remaining_seconds() <= 0:
            return None, saw_403

        if not resolved or not resolved.get("url"):
            return None, saw_403

        actual_quality = None
        matching_heights = {
            candidate.get("height")
            for candidate in formats
            if isinstance(candidate, dict) and candidate.get("url") == resolved["url"]
        }
        if len(matching_heights) == 1:
            height = next(iter(matching_heights))
            if type(height) is int and height > 0:
                # Report resolution only from numeric metadata. A quality label alone
                # is not proof of the actual selected rendition.
                resolved["actualHeight"] = height
                actual_quality = _quality_for_height(height)
                if actual_quality:
                    resolved["actualQuality"] = actual_quality

        if quality in MANUAL_QUALITIES and actual_quality != quality:
            return None, saw_403

        return resolved, saw_403

    default_meta = None
    default_timeout = extraction_timeout()
    if default_timeout is not None:
        default_extractor = default_extractor_func or run_default_ytdlp_extraction
        try:
            default_meta = default_extractor(video_id, default_timeout)
        except Exception:
            # The default clients do not use BgUtils PO tokens. Their 403 state must
            # not suppress the separate mweb/PO candidate.
            default_meta = None

    default_resolved, _ = resolve_candidate(default_meta)
    if default_resolved:
        return default_resolved

    # This cooldown applies to the PO/mweb path. Always try default clients first,
    # including during cooldown, and only suppress the PO candidate itself.
    if cooldown_tracker.is_in_cooldown():
        return fallback_or_fail()

    extractor = extractor_func or run_ytdlp_extraction
    po_timeout = extraction_timeout()
    if po_timeout is None:
        return fallback_or_fail()
    try:
        po_meta = extractor(video_id, bgutil_url, po_timeout)
    except PermissionError:
        cooldown_tracker.record_403()
        return fallback_or_fail()
    except Exception:
        return fallback_or_fail()

    po_resolved, saw_403 = resolve_candidate(po_meta)
    if po_resolved:
        return po_resolved
    if saw_403:
        cooldown_tracker.record_403()
    return fallback_or_fail()
