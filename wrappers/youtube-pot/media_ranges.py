"""Validation of GoogleVideo HTTPS stream URLs and HTTP range sampling.

Ensures candidate media streams are authentic, non-truncated, and actually
deliverable before advertising them or returning them to Gateway/FFmpeg.

Network Security & SSRF Protection Boundary:
Google CDN edge caches reside across Google's global autonomous system (AS15169)
under dynamic `*.googlevideo.com` subdomains that rotate dynamically. Static IP
pinning or pre-resolving DNS in application space would break playback availability
or create fragile false negatives. Security against Server-Side Request Forgery
(SSRF) and intranet traversal is enforced at the network boundary by:
1. Strict HTTPS requirement on standard port 443 with TLS certificate verification
   against trusted root certificates.
2. Strict hostname matching restricted exclusively to RFC 1123 subdomains of
   `googlevideo.com` (rejecting root domain, empty labels, and non-conforming characters).
3. Prohibition of user credentials (userinfo) and explicit port specifications.
4. Complete disabling of automatic urllib redirect following via a custom
   NoRedirectHandler; every redirect target is validated against `is_googlevideo_url`
   before any connection is established.
5. Strict limit of at most two (2) redirects, with immediate detection and rejection
   of redirect loops / cycles.
6. Enforcing strict read bounds (end - start + 2) and short network timeouts (2.5s)
   on all range probes.
"""

from __future__ import annotations

import re
import urllib.error
import urllib.parse
import urllib.request
from typing import Callable, Optional, Set, Tuple

MAX_MEDIA_BYTES = 32 * 1024 * 1024 * 1024  # 32 GiB
RANGE_SAMPLE_BYTES = 1024
MAX_REDIRECTS = 2
SAMPLE_TIMEOUT_SECONDS = 2.5

CONTENT_RANGE_PATTERN = re.compile(r"^bytes\s+(\d+)-(\d+)/(\d+)$", re.IGNORECASE)
SUBDOMAIN_LABEL_PATTERN = re.compile(r"^[a-z0-9_-]+$", re.IGNORECASE)


class _NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Urllib redirect handler that suppresses automatic redirect following.

    Returns the response object directly for 3xx responses so that redirect
    Location headers can be inspected and validated before connecting.
    """

    def http_error_300(self, req, fp, code, msg, headers):
        return fp

    def http_error_301(self, req, fp, code, msg, headers):
        return fp

    def http_error_302(self, req, fp, code, msg, headers):
        return fp

    def http_error_303(self, req, fp, code, msg, headers):
        return fp

    def http_error_307(self, req, fp, code, msg, headers):
        return fp

    def http_error_308(self, req, fp, code, msg, headers):
        return fp


_NO_REDIRECT_OPENER = urllib.request.build_opener(_NoRedirectHandler)


def is_googlevideo_url(raw: str) -> bool:
    """Validate that the given stream URL is a safe, bounded GoogleVideo HTTPS URL."""
    if not isinstance(raw, str) or len(raw) > 16384:
        return False
    try:
        parsed = urllib.parse.urlsplit(raw)
        if parsed.scheme != "https":
            return False
        if parsed.username or parsed.password:
            return False
        if parsed.port is not None:
            return False
        hostname = (parsed.hostname or "").lower().rstrip(".")
        if not hostname.endswith(".googlevideo.com"):
            return False
        subdomain = hostname[: -len(".googlevideo.com")]
        if not subdomain:
            return False
        labels = subdomain.split(".")
        for label in labels:
            if not label or not SUBDOMAIN_LABEL_PATTERN.match(label):
                return False
        return True
    except Exception:
        return False


def sample_range(
    url: str,
    start: int,
    end: int,
    total_expected: int = 0,
    fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
) -> Tuple[int, bool]:
    """Sample an HTTP byte range.

    Returns (discovered_total, saw_403). If invalid or failed, discovered_total is 0.
    Permits at most MAX_REDIRECTS (2), re-validating every target with is_googlevideo_url
    before connecting. Unsafe redirects and loops are rejected immediately.
    """
    if not is_googlevideo_url(url):
        return 0, False

    current_url = url
    visited_urls: Set[str] = {current_url}
    redirects = 0

    while True:
        if not is_googlevideo_url(current_url):
            return 0, False

        headers = {
            "Range": f"bytes={start}-{end}",
            "Accept-Encoding": "identity",
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
        }

        try:
            if fetch_func is not None:
                status, resp_headers, body = fetch_func(current_url, headers)
            else:
                req = urllib.request.Request(current_url, headers=headers, method="GET")
                with _NO_REDIRECT_OPENER.open(
                    req, timeout=SAMPLE_TIMEOUT_SECONDS
                ) as response:
                    status = getattr(response, "status", getattr(response, "code", 0))
                    resp_headers = {k.lower(): v for k, v in response.headers.items()}
                    if 300 <= status < 400:
                        body = b""
                    else:
                        body = response.read(end - start + 2)
        except urllib.error.HTTPError as err:
            if err.code == 403:
                return 0, True
            return 0, False
        except Exception:
            return 0, False

        # Redirect handling
        if 300 <= status < 400:
            redirects += 1
            if redirects > MAX_REDIRECTS:
                return 0, False
            location = resp_headers.get("location")
            if not location:
                return 0, False
            next_url = urllib.parse.urljoin(current_url, location)
            if not is_googlevideo_url(next_url):
                return 0, False
            if next_url in visited_urls:
                # Cycle / loop detected
                return 0, False
            visited_urls.add(next_url)
            current_url = next_url
            continue

        if status == 403:
            return 0, True

        if status != 206:
            return 0, False

        content_range = resp_headers.get("content-range", "")
        match = CONTENT_RANGE_PATTERN.match(content_range)
        if not match:
            return 0, False

        first = int(match.group(1))
        last = int(match.group(2))
        discovered = int(match.group(3))

        expected_bytes = last - first + 1
        if (
            discovered <= 0
            or discovered > MAX_MEDIA_BYTES
            or first != start
            or last < first
            or last != min(end, discovered - 1)
            or last >= discovered
            or (total_expected and discovered != total_expected)
            or len(body) != expected_bytes
        ):
            return 0, False

        return discovered, False


def validate_media_ranges(
    url: str,
    declared_size: int = 0,
    fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
) -> Tuple[bool, bool]:
    """Validate head, mid, and tail ranges of a GoogleVideo stream.

    Returns (is_valid, saw_403).
    """
    if not is_googlevideo_url(url):
        return False, False

    try:
        parsed = urllib.parse.urlsplit(url)
        clen_param = urllib.parse.parse_qs(parsed.query).get("clen", [None])[0]
        from_url = int(clen_param) if clen_param and clen_param.isdigit() else 0
    except Exception:
        from_url = 0

    total = declared_size or from_url
    if total < 0 or total > MAX_MEDIA_BYTES:
        return False, False

    # 1. Head sample (0 .. min(total-1, 1023))
    head_end = (
        (total - 1) if (0 < total < RANGE_SAMPLE_BYTES) else (RANGE_SAMPLE_BYTES - 1)
    )
    discovered, saw_403 = sample_range(
        url, 0, head_end, total_expected=total, fetch_func=fetch_func
    )
    if saw_403:
        return False, True
    if not discovered:
        return False, False

    total = discovered

    # 2. Mid sample and Tail sample
    starts = [total // 2, max(0, total - RANGE_SAMPLE_BYTES)]
    for start in set(starts):
        if start == 0:
            continue
        end = min(total - 1, start + RANGE_SAMPLE_BYTES - 1)
        sub_discovered, sub_403 = sample_range(
            url, start, end, total_expected=total, fetch_func=fetch_func
        )
        if sub_403:
            return False, True
        if not sub_discovered:
            return False, False

    return True, False
