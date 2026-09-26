"""Candidate selection, codec filtering, and truthful variant determination.

Initial Compatibility Profile:
This worker enforces an initial baseline compatibility profile:
- H.264 (AVC) video (vcodec starting with avc1 or h264)
- Safe frame rates <= 30 fps (dropping 50fps and 60fps high-rate streams)
- AAC audio (acodec starting with mp4a or aac)

This profile is designed specifically to guarantee smooth hardware playback on
legacy TV targets (such as Vizio Co-Star running Android API 13) and constrained
devices lacking VP9/AV1 hardware decoders.

Architectural Note on Modern Devices and Adaptive Profiles:
This conservative profile is NOT claimed to be a universal hardware ceiling for all
modern client devices. Rather, it represents the default compatibility profile
while the gateway API establishes dynamic per-device capability negotiation (see
`docs/development/adaptive-device-coverage.md`). High-resolution tiers (1440p, 2160p/4K)
and alternative codecs are accurately identified rather than mislabeled as 1080p,
preserving architectural headroom for future client capability probes.
"""

from __future__ import annotations

import re
import time
import urllib.parse
from typing import Any, Callable, Dict, List, Optional, Tuple

from media_ranges import validate_media_ranges

MAX_SAFE_FPS = 30
ALLOWED_TIERS = ("1080p", "720p", "480p", "360p")


def is_safe_fps(format_dict: Dict[str, Any]) -> bool:
    """Ensure the format frame rate does not exceed 30 fps in this compatibility profile."""
    fps = format_dict.get("fps")
    if fps is not None and fps > MAX_SAFE_FPS:
        return False

    for key in ("quality_label", "format_note", "format"):
        val = str(format_dict.get(key) or "")
        if re.search(r"(?:50|60)(?:fps)?$", val, re.IGNORECASE):
            return False

    return True


def is_h264_video(format_dict: Dict[str, Any]) -> bool:
    """Ensure video codec is H.264 / AVC."""
    vcodec = str(format_dict.get("vcodec") or "").lower()
    if not vcodec or vcodec == "none":
        return False
    return vcodec.startswith("avc1") or vcodec.startswith("h264")


def is_aac_audio(format_dict: Dict[str, Any]) -> bool:
    """Ensure audio codec is AAC."""
    acodec = str(format_dict.get("acodec") or "").lower()
    if not acodec or acodec == "none":
        return False
    return acodec.startswith("mp4a") or acodec.startswith("aac")


def is_direct_media_format(
    format_dict: Dict[str, Any], extensions: tuple[str, ...]
) -> bool:
    """Keep manifest formats out of the byte-ranged MP4 remux path."""
    if format_dict.get("protocol") not in (None, "https"):
        return False
    if format_dict.get("ext") not in (None, *extensions):
        return False
    url = format_dict.get("url")
    if not isinstance(url, str):
        return False
    try:
        return urllib.parse.urlsplit(url).path == "/videoplayback"
    except ValueError:
        return False


def format_tier(format_dict: Dict[str, Any]) -> Optional[str]:
    """Map a video format to a standard resolution tier without relabeling 2K/4K as 1080p."""
    height = format_dict.get("height")
    if isinstance(height, int) and height > 0:
        if height > 1440:
            return "2160p"
        if height > 1080:
            return "1440p"
        if height > 720:
            return "1080p"
        if height > 480:
            return "720p"
        if height > 360:
            return "480p"
        return "360p"

    label = str(
        format_dict.get("quality_label") or format_dict.get("format_note") or ""
    ).lower()

    if "2160p" in label or "4k" in label:
        return "2160p"
    if "1440p" in label or "2k" in label:
        return "1440p"
    for tier in ALLOWED_TIERS:
        if tier in label:
            return tier

    return None


class FormatSelector:
    """Evaluates format candidates, performs range checks, and produces truthful variants."""

    def __init__(
        self,
        formats: List[Dict[str, Any]],
        fetch_func: Optional[Callable[..., Tuple[int, dict, bytes]]] = None,
        deadline: Optional[float] = None,
    ):
        self.formats = formats or []
        self.deadline = deadline
        self.budget_expired = False
        self._raw_fetch_func = fetch_func
        self.fetch_func = fetch_func
        if fetch_func is not None and deadline is not None:
            self.fetch_func = self._fetch_with_deadline
        self._checked: Dict[Tuple[str, int], bool] = {}
        self.saw_403 = False

    def _fetch_with_deadline(self, url: str, headers: dict) -> Tuple[int, dict, bytes]:
        """Stop starting range requests after the shared resolution deadline."""
        if self.deadline is not None and time.monotonic() >= self.deadline:
            self.budget_expired = True
            raise TimeoutError("YouTube resolution budget expired")

        try:
            result = self._raw_fetch_func(url, headers)
        except Exception:
            if self.deadline is not None and time.monotonic() >= self.deadline:
                self.budget_expired = True
            raise

        if self.deadline is not None and time.monotonic() >= self.deadline:
            self.budget_expired = True
            raise TimeoutError("YouTube resolution budget expired")
        return result

    def validate_format(self, fmt: Dict[str, Any]) -> bool:
        """Validate format URL with head, mid, tail range checks, cached by exact URL and size."""
        if self.deadline is not None and time.monotonic() >= self.deadline:
            self.budget_expired = True
            return False

        url = fmt.get("url")
        if not url:
            return False

        declared_size = int(fmt.get("filesize") or fmt.get("filesize_approx") or 0)
        cache_key = (url, declared_size)
        if cache_key in self._checked:
            return self._checked[cache_key]

        is_valid, saw_403 = validate_media_ranges(
            url, declared_size=declared_size, fetch_func=self.fetch_func
        )
        if self.deadline is not None and time.monotonic() >= self.deadline:
            self.budget_expired = True
            is_valid = False
        if saw_403:
            self.saw_403 = True

        self._checked[cache_key] = is_valid
        return is_valid

    def find_validated_audio(self) -> Optional[Dict[str, Any]]:
        """Find the best validated AAC audio candidate."""
        audio_candidates = []
        for fmt in self.formats:
            vcodec = str(fmt.get("vcodec") or "none")
            if vcodec != "none":
                continue
            if not is_aac_audio(fmt) or not is_direct_media_format(fmt, ("m4a", "mp4")):
                continue
            audio_candidates.append(fmt)

        # Sort audio by bitrate descending
        audio_candidates.sort(
            key=lambda f: float(f.get("abr") or f.get("tbr") or 0), reverse=True
        )

        for candidate in audio_candidates:
            if self.validate_format(candidate):
                return candidate

        return None

    def determine_variants_and_resolve(
        self, target_quality: str = ""
    ) -> Tuple[Optional[Dict[str, Any]], List[str], bool]:
        """Determine truthful variants and resolve the desired stream.

        Returns (resolved_result, variants_list, saw_403).
        If resolution fails, resolved_result is None.
        """
        # Separate into tiers
        tier_combined: Dict[str, List[Dict[str, Any]]] = {t: [] for t in ALLOWED_TIERS}
        tier_video: Dict[str, List[Dict[str, Any]]] = {t: [] for t in ALLOWED_TIERS}

        for fmt in self.formats:
            if (
                not is_h264_video(fmt)
                or not is_safe_fps(fmt)
                or not is_direct_media_format(fmt, ("mp4",))
            ):
                continue

            tier = format_tier(fmt)
            if not tier or tier not in ALLOWED_TIERS:
                continue

            acodec = str(fmt.get("acodec") or "none")
            if acodec != "none" and is_aac_audio(fmt):
                tier_combined[tier].append(fmt)
            elif acodec == "none":
                tier_video[tier].append(fmt)

        requested = (
            target_quality if target_quality and target_quality != "auto" else ""
        )

        if requested:
            if requested not in ALLOWED_TIERS:
                return None, [], self.saw_403

            # Manual selection only probes its requested tier. A validated combined
            # stream needs no second audio request; split video gets one valid AAC pair.
            for candidate in tier_combined[requested]:
                if self.validate_format(candidate):
                    return (
                        {
                            "url": candidate["url"],
                            "mimeType": "video/mp4",
                            "variants": [requested],
                        },
                        [requested],
                        self.saw_403,
                    )

            validated_audio = self.find_validated_audio()
            if validated_audio:
                for candidate in tier_video[requested]:
                    if self.validate_format(candidate):
                        return (
                            {
                                "url": candidate["url"],
                                "audioUrl": validated_audio["url"],
                                "mimeType": "video/mp4",
                                "variants": [requested],
                            },
                            [requested],
                            self.saw_403,
                        )

            return None, [], self.saw_403

        # Auto inventories all compatible tiers so its menu remains truthful. It
        # prefers the validated combined 360p stream, then the highest validated tier.
        validated_audio = self.find_validated_audio()
        validated_tier_combined: Dict[str, Dict[str, Any]] = {}
        validated_tier_video: Dict[str, Dict[str, Any]] = {}
        truthful_variants: List[str] = []

        for tier in ALLOWED_TIERS:
            for candidate in tier_combined[tier]:
                if self.validate_format(candidate):
                    validated_tier_combined[tier] = candidate
                    break

            if validated_audio:
                for candidate in tier_video[tier]:
                    if self.validate_format(candidate):
                        validated_tier_video[tier] = candidate
                        break

            if tier in validated_tier_combined or tier in validated_tier_video:
                truthful_variants.append(tier)

        resolved: Optional[Dict[str, Any]] = None
        if "360p" in validated_tier_combined:
            resolved = {
                "url": validated_tier_combined["360p"]["url"],
                "mimeType": "video/mp4",
                "variants": truthful_variants,
            }
        else:
            for tier in ALLOWED_TIERS:
                if tier in validated_tier_combined:
                    resolved = {
                        "url": validated_tier_combined[tier]["url"],
                        "mimeType": "video/mp4",
                        "variants": truthful_variants,
                    }
                    break
                if tier in validated_tier_video and validated_audio:
                    resolved = {
                        "url": validated_tier_video[tier]["url"],
                        "audioUrl": validated_audio["url"],
                        "mimeType": "video/mp4",
                        "variants": truthful_variants,
                    }
                    break

        return resolved, truthful_variants, self.saw_403
