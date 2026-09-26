import json
import subprocess
import sys
import time
import unittest
import urllib.parse
from pathlib import Path
from unittest.mock import patch

WRAPPER_DIR = Path(__file__).resolve().parents[1]
if str(WRAPPER_DIR) not in sys.path:
    sys.path.insert(0, str(WRAPPER_DIR))

from cooldown import CooldownTracker
from resolver import (
    MAX_UPSTREAM_RESPONSE_BYTES,
    fallback_to_upstream,
    resolve_video,
    run_bounded_command,
    run_ytdlp_extraction,
)


class ResolverTests(unittest.TestCase):
    def setUp(self):
        self.config = {
            "token": "test-token-at-least-32-chars-long-abcdef",
            "upstream_url": "http://youtube.upstream:8091",
            "bgutil_url": "http://bgutil.upstream:4416",
            "cooldown_seconds": 300,
            "timeout_seconds": 5,
        }
        self.cooldown_tracker = CooldownTracker()

    def test_extractor_enables_bundled_ejs_runtime(self):
        completed = subprocess.CompletedProcess(
            [], 0, stdout=b'{"formats": []}', stderr=b""
        )
        with patch("resolver.run_bounded_command", return_value=completed) as run:
            run_ytdlp_extraction("dQw4w9WgXcQ", "http://bgutil.upstream:4416")

        command = run.call_args.args[0]
        self.assertEqual(
            command[command.index("--js-runtimes") + 1],
            "node",
        )
        extractor_args = [
            command[index + 1]
            for index, value in enumerate(command[:-1])
            if value == "--extractor-args"
        ]
        self.assertIn("youtube:player_client=mweb", extractor_args)
        self.assertIn(
            "youtubepot-bgutilhttp:base_url=http://bgutil.upstream:4416",
            extractor_args,
        )
        self.assertEqual(run.call_args.kwargs["timeout_seconds"], 12)

    def test_extraction_timeout_is_capped(self):
        completed = subprocess.CompletedProcess(
            [], 0, stdout=b'{"formats": []}', stderr=b""
        )
        with patch("resolver.run_bounded_command", return_value=completed) as run:
            run_ytdlp_extraction(
                "dQw4w9WgXcQ", "http://bgutil.upstream:4416", timeout_seconds=500
            )

        self.assertEqual(run.call_args.kwargs["timeout_seconds"], 30)

    def test_bounded_command_rejects_oversized_output(self):
        command = [
            sys.executable,
            "-c",
            "import sys,time; sys.stdout.write('x' * 1024); sys.stdout.flush(); time.sleep(5)",
        ]
        started = time.monotonic()
        with self.assertRaisesRegex(RuntimeError, "size limit"):
            run_bounded_command(
                command,
                timeout_seconds=3,
                max_stdout_bytes=32,
                max_stderr_bytes=32,
            )
        self.assertLess(time.monotonic() - started, 2)

    def test_bounded_command_stops_a_timed_out_process(self):
        command = [sys.executable, "-c", "import time; time.sleep(5)"]
        started = time.monotonic()
        with self.assertRaisesRegex(TimeoutError, "timed out"):
            run_bounded_command(command, timeout_seconds=0.2)
        self.assertLess(time.monotonic() - started, 2)

    def test_bounded_command_stops_when_pipes_close_before_process_exits(self):
        command = [
            sys.executable,
            "-c",
            "import os,time; os.close(1); os.close(2); time.sleep(5)",
        ]
        started = time.monotonic()
        with self.assertRaisesRegex(TimeoutError, "timed out"):
            run_bounded_command(command, timeout_seconds=0.2)
        self.assertLess(time.monotonic() - started, 2)

    def test_invalid_video_id_raises_value_error(self):
        with self.assertRaises(ValueError):
            resolve_video("invalid_id", "auto", self.config, self.cooldown_tracker)
        with self.assertRaises(ValueError):
            resolve_video(
                "too_long_video_id_12345", "auto", self.config, self.cooldown_tracker
            )

    def test_successful_hd_resolution(self):
        def mock_extractor(video_id, bgutil_url, timeout_sec):
            self.assertEqual(video_id, "dQw4w9WgXcQ")
            return {
                "formats": [
                    {
                        "format_id": "136",
                        "url": "https://rr1.googlevideo.com/videoplayback?itag=136&clen=50000",
                        "vcodec": "avc1.4d401f",
                        "acodec": "none",
                        "fps": 30,
                        "height": 720,
                        "filesize": 50000,
                    },
                    {
                        "format_id": "140",
                        "url": "https://rr1.googlevideo.com/videoplayback?itag=140&clen=15000",
                        "vcodec": "none",
                        "acodec": "mp4a.40.2",
                        "abr": 128,
                        "filesize": 15000,
                    },
                ]
            }

        def mock_range_fetch(url, headers):
            range_hdr = headers.get("Range", "")
            parts = range_hdr.replace("bytes=", "").split("-")
            start, end = int(parts[0]), int(parts[1])
            parsed = urllib.parse.urlsplit(url)
            clen_list = urllib.parse.parse_qs(parsed.query).get("clen", ["50000"])
            total = int(clen_list[0])
            return (
                206,
                {"content-range": f"bytes {start}-{end}/{total}"},
                b"x" * (end - start + 1),
            )

        res = resolve_video(
            "dQw4w9WgXcQ",
            "720p",
            self.config,
            self.cooldown_tracker,
            extractor_func=mock_extractor,
            range_fetch_func=mock_range_fetch,
        )

        self.assertIsNotNone(res)
        self.assertIn("itag=136", res["url"])
        self.assertIn("itag=140", res["audioUrl"])
        self.assertEqual(res["mimeType"], "video/mp4")
        self.assertEqual(res["variants"], ["720p"])

    def test_extractor_403_activates_cooldown_and_falls_back(self):
        def mock_extractor(video_id, bgutil_url, timeout_sec):
            raise PermissionError("YouTube 403 Forbidden")

        def mock_upstream_fetch(url, headers):
            # Upstream fallback fetch
            if "youtube.upstream:8091/resolve/dQw4w9WgXcQ" in url:
                body = json.dumps(
                    {
                        "url": "https://rr1.googlevideo.com/videoplayback?itag=18&fallback=true",
                        "mimeType": "video/mp4",
                        "variants": ["360p"],
                    }
                ).encode("utf-8")
                return 200, {}, body
            return 404, {}, b""

        self.assertFalse(self.cooldown_tracker.is_in_cooldown())
        res = resolve_video(
            "dQw4w9WgXcQ",
            "auto",
            self.config,
            self.cooldown_tracker,
            extractor_func=mock_extractor,
            upstream_fetch_func=mock_upstream_fetch,
        )

        self.assertTrue(self.cooldown_tracker.is_in_cooldown())
        self.assertIn("fallback=true", res["url"])
        self.assertEqual(res["variants"], ["360p"])

    def test_manual_hd_request_falls_back_to_baseline_without_forwarding_hd_quality(
        self,
    ):
        # Client requests 1080p, but PO resolver hits 403 or failure.
        # Fallback to upstream MUST NOT pass ?quality=1080p, preserving baseline 360p.
        captured_upstream_url = ""

        def mock_extractor(video_id, bgutil_url, timeout_sec):
            raise PermissionError("YouTube 403 Forbidden")

        def mock_upstream_fetch(url, headers):
            nonlocal captured_upstream_url
            captured_upstream_url = url
            # Upstream baseline response
            body = json.dumps(
                {
                    "url": "https://rr1.googlevideo.com/videoplayback?itag=18&baseline=true",
                    "mimeType": "video/mp4",
                    "variants": ["360p"],
                }
            ).encode("utf-8")
            return 200, {}, body

        res = resolve_video(
            "dQw4w9WgXcQ",
            "1080p",
            self.config,
            self.cooldown_tracker,
            extractor_func=mock_extractor,
            upstream_fetch_func=mock_upstream_fetch,
        )

        # Verify that quality=1080p was NOT appended to upstream request
        self.assertNotIn("quality=1080p", captured_upstream_url)
        self.assertEqual(
            captured_upstream_url,
            "http://youtube.upstream:8091/resolve/dQw4w9WgXcQ",
        )
        self.assertIsNotNone(res)
        self.assertIn("baseline=true", res["url"])
        self.assertEqual(res["variants"], ["360p"])

    def test_in_cooldown_bypasses_extractor_directly_to_fallback(self):
        self.cooldown_tracker.record_403(cooldown_seconds=300)
        self.assertTrue(self.cooldown_tracker.is_in_cooldown())

        extractor_called = False

        def mock_extractor(video_id, bgutil_url, timeout_sec):
            nonlocal extractor_called
            extractor_called = True
            return {}

        def mock_upstream_fetch(url, headers):
            body = json.dumps(
                {
                    "url": "https://rr1.googlevideo.com/videoplayback?itag=18&cooldown=true",
                    "mimeType": "video/mp4",
                    "variants": ["360p"],
                }
            ).encode("utf-8")
            return 200, {}, body

        res = resolve_video(
            "dQw4w9WgXcQ",
            "auto",
            self.config,
            self.cooldown_tracker,
            extractor_func=mock_extractor,
            upstream_fetch_func=mock_upstream_fetch,
        )

        self.assertFalse(extractor_called)
        self.assertIn("cooldown=true", res["url"])

    def test_range_check_403_activates_cooldown_and_falls_back(self):
        def mock_extractor(video_id, bgutil_url, timeout_sec):
            return {
                "formats": [
                    {
                        "format_id": "18",
                        "url": "https://rr1.googlevideo.com/videoplayback?itag=18",
                        "vcodec": "avc1.42001E",
                        "acodec": "mp4a.40.2",
                        "fps": 30,
                        "height": 360,
                    }
                ]
            }

        def mock_range_fetch(url, headers):
            return 403, {}, b"Forbidden"

        def mock_upstream_fetch(url, headers):
            return (
                200,
                {},
                json.dumps(
                    {
                        "url": "https://rr1.googlevideo.com/videoplayback?fallback=baseline",
                        "mimeType": "video/mp4",
                        "variants": ["360p"],
                    }
                ).encode("utf-8"),
            )

        res = resolve_video(
            "dQw4w9WgXcQ",
            "auto",
            self.config,
            self.cooldown_tracker,
            extractor_func=mock_extractor,
            range_fetch_func=mock_range_fetch,
            upstream_fetch_func=mock_upstream_fetch,
        )

        self.assertTrue(self.cooldown_tracker.is_in_cooldown())
        self.assertIn("fallback=baseline", res["url"])

    def test_upstream_oversized_response_rejected(self):
        def mock_fetch(url, headers):
            # Exceed MAX_UPSTREAM_RESPONSE_BYTES (1 MiB)
            return 200, {}, b"x" * (MAX_UPSTREAM_RESPONSE_BYTES + 100)

        with self.assertRaises(RuntimeError) as ctx:
            fallback_to_upstream(
                "dQw4w9WgXcQ",
                self.config["upstream_url"],
                self.config["token"],
                fetch_func=mock_fetch,
            )
        self.assertIn("size limit", str(ctx.exception).lower())


if __name__ == "__main__":
    unittest.main()
