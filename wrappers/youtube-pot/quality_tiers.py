"""Shared dimension-aware 16:9 quality-tier classification.

`formats.py` uses strict tiers for manual selection and variant inventory, while
keeping a legacy nominal bucket for low-raster Auto fallback. `resolver.py` uses
strict classification for final actual-quality metadata. Panoramic rasters
(e.g. 1920x804) keep their real width/height; the nominal tier is derived from
the frame's 16:9-equivalent height, computed rationally to avoid premature
rounding.
"""

from __future__ import annotations

from fractions import Fraction
from typing import Optional, Tuple

# Ordered from highest to lowest; each tier's minimum height is the boundary
# a rendition must reach (inclusive) to be labeled at that tier.
STANDARD_TIER_MIN_HEIGHTS: Tuple[Tuple[str, int], ...] = (
    ("2160p", 2160),
    ("1440p", 1440),
    ("1080p", 1080),
    ("720p", 720),
    ("480p", 480),
    ("360p", 360),
)


def _tier_for_effective_height(
    effective_height: Fraction, *, floor_below_minimum: bool
) -> Optional[str]:
    for tier, minimum_height in STANDARD_TIER_MIN_HEIGHTS:
        if effective_height >= minimum_height:
            return tier
    if floor_below_minimum:
        # Preserve the legacy nominal bucket for internal low-raster fallback.
        return STANDARD_TIER_MIN_HEIGHTS[-1][0]
    return None


def _tier_for_dimensions(
    width: int, height: int, *, floor_below_minimum: bool
) -> Optional[str]:
    if not isinstance(width, int) or not isinstance(height, int):
        return None
    if width <= 0 or height <= 0:
        return None

    short_side = min(width, height)
    long_side = max(width, height)
    effective_height = max(Fraction(short_side), Fraction(long_side * 9, 16))
    return _tier_for_effective_height(
        effective_height, floor_below_minimum=floor_below_minimum
    )


def supported_tier_for_height(height: int) -> Optional[str]:
    """Classify a pixel height only when it reaches a supported tier minimum."""
    if not isinstance(height, int) or height <= 0:
        return None
    return _tier_for_effective_height(Fraction(height), floor_below_minimum=False)


def supported_tier_for_dimensions(width: int, height: int) -> Optional[str]:
    """Classify dimensions only when their rational 16:9-equivalent reaches a tier."""
    return _tier_for_dimensions(width, height, floor_below_minimum=False)


def nominal_tier_for_height(height: int) -> Optional[str]:
    """Classify height with legacy flooring for internal low-raster fallback."""
    if not isinstance(height, int) or height <= 0:
        return None
    return _tier_for_effective_height(Fraction(height), floor_below_minimum=True)


def nominal_tier_for_dimensions(width: int, height: int) -> Optional[str]:
    """Classify real width/height using a rational 16:9-equivalent height.

    effectiveHeight = max(shortSide, longSide * 9/16), compared exactly
    (no float rounding) against standard tier minimums. Examples:
    1920x804 -> 1080p, 1280x536 -> 720p, 640x480 -> 480p.
    """
    return _tier_for_dimensions(width, height, floor_below_minimum=True)
