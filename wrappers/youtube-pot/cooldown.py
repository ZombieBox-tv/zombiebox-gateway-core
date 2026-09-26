"""Single-flight concurrency locking and 403 cooldown / rate limiting tracker."""

from __future__ import annotations

import threading
import time
from typing import Optional


class CooldownTracker:
    """Tracks 403 Forbidden cooldown state and enforces single-flight execution."""

    def __init__(self, default_cooldown_seconds: int = 300):
        self.default_cooldown_seconds = default_cooldown_seconds
        self._lock = threading.Lock()
        self._flight_lock = threading.Lock()
        self._cooldown_until: float = 0.0
        self._last_403_timestamp: float = 0.0

    def acquire_flight(self) -> bool:
        """Attempt to acquire the single-flight execution lock.

        Returns True if acquired (caller must call release_flight).
        Returns False if another resolution is currently in progress.
        """
        return self._flight_lock.acquire(blocking=False)

    def release_flight(self) -> None:
        """Release the single-flight execution lock."""
        try:
            self._flight_lock.release()
        except RuntimeError:
            pass

    def record_403(self, cooldown_seconds: Optional[int] = None) -> None:
        """Record an observed 403 Forbidden from YouTube and enter cooldown."""
        with self._lock:
            now = time.time()
            duration = cooldown_seconds or self.default_cooldown_seconds
            self._last_403_timestamp = now
            self._cooldown_until = now + duration

    def is_in_cooldown(self) -> bool:
        """Check whether the resolver is currently in 403 cooldown."""
        with self._lock:
            return time.time() < self._cooldown_until

    def remaining_cooldown(self) -> float:
        """Return the remaining seconds in cooldown, or 0.0 if not in cooldown."""
        with self._lock:
            remaining = self._cooldown_until - time.time()
            return max(0.0, remaining)

    def reset(self) -> None:
        """Clear the cooldown state (used for testing)."""
        with self._lock:
            self._cooldown_until = 0.0
            self._last_403_timestamp = 0.0
