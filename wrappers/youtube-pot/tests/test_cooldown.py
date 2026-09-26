import sys
import time
import unittest
from pathlib import Path

WRAPPER_DIR = Path(__file__).resolve().parents[1]
if str(WRAPPER_DIR) not in sys.path:
    sys.path.insert(0, str(WRAPPER_DIR))

from cooldown import CooldownTracker


class CooldownTests(unittest.TestCase):
    def test_single_flight_lock(self):
        tracker = CooldownTracker()
        self.assertTrue(tracker.acquire_flight())
        # Second acquire while locked should fail (single flight)
        self.assertFalse(tracker.acquire_flight())
        tracker.release_flight()
        # After release, acquire should succeed again
        self.assertTrue(tracker.acquire_flight())
        tracker.release_flight()

    def test_403_cooldown_activation(self):
        tracker = CooldownTracker(default_cooldown_seconds=60)
        self.assertFalse(tracker.is_in_cooldown())
        self.assertEqual(tracker.remaining_cooldown(), 0.0)

        tracker.record_403()
        self.assertTrue(tracker.is_in_cooldown())
        self.assertGreater(tracker.remaining_cooldown(), 0.0)

        tracker.reset()
        self.assertFalse(tracker.is_in_cooldown())

    def test_custom_cooldown_duration_and_expiry(self):
        tracker = CooldownTracker()
        # Very short cooldown for test
        tracker.record_403(cooldown_seconds=1)
        self.assertTrue(tracker.is_in_cooldown())
        time.sleep(1.1)
        self.assertFalse(tracker.is_in_cooldown())


if __name__ == "__main__":
    unittest.main()
