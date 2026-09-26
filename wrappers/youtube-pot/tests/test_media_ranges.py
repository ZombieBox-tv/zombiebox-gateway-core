import http.server
import sys
import threading
import unittest
import urllib.request
from pathlib import Path

# Add module parent to sys.path
WRAPPER_DIR = Path(__file__).resolve().parents[1]
if str(WRAPPER_DIR) not in sys.path:
    sys.path.insert(0, str(WRAPPER_DIR))

from media_ranges import (
    _NO_REDIRECT_OPENER,
    is_googlevideo_url,
    sample_range,
    validate_media_ranges,
)


class MediaRangesTests(unittest.TestCase):
    def test_googlevideo_url_validation(self):
        self.assertTrue(
            is_googlevideo_url(
                "https://rr1---sn-4g5ednls.googlevideo.com/videoplayback?expire=123"
            )
        )
        self.assertTrue(
            is_googlevideo_url(
                "https://manifest.googlevideo.com/api/manifest/hls_playlist"
            )
        )
        # Scheme checks
        self.assertFalse(is_googlevideo_url("http://rr1.googlevideo.com/videoplayback"))
        self.assertFalse(is_googlevideo_url("ftp://rr1.googlevideo.com/videoplayback"))
        # Port & credentials checks
        self.assertFalse(
            is_googlevideo_url("https://rr1.googlevideo.com:8443/videoplayback")
        )
        self.assertFalse(
            is_googlevideo_url("https://user:pass@rr1.googlevideo.com/videoplayback")
        )
        # Domain boundary checks
        self.assertFalse(is_googlevideo_url("https://attacker.com/videoplayback"))
        self.assertFalse(is_googlevideo_url("https://notgooglevideo.com/videoplayback"))
        self.assertFalse(
            is_googlevideo_url("https://googlevideo.com.attacker.com/videoplayback")
        )
        self.assertFalse(is_googlevideo_url("https://youtube.com/watch?v=12345678901"))
        self.assertFalse(is_googlevideo_url("https://googlevideo.com/videoplayback"))
        self.assertFalse(is_googlevideo_url("https://.googlevideo.com/videoplayback"))
        self.assertFalse(
            is_googlevideo_url("https://rr1..googlevideo.com/videoplayback")
        )
        # Oversized
        self.assertFalse(
            is_googlevideo_url("https://" + ("a" * 17000) + ".googlevideo.com/")
        )

    def test_sample_range_success(self):
        url = "https://rr1.googlevideo.com/videoplayback?clen=100000"

        def mock_fetch(req_url, headers):
            range_hdr = headers.get("Range", "")
            self.assertEqual(range_hdr, "bytes=0-1023")
            resp_headers = {"content-range": "bytes 0-1023/100000"}
            return 206, resp_headers, b"x" * 1024

        discovered, saw_403 = sample_range(
            url, 0, 1023, total_expected=100000, fetch_func=mock_fetch
        )
        self.assertEqual(discovered, 100000)
        self.assertFalse(saw_403)

    def test_sample_range_403_detected(self):
        url = "https://rr1.googlevideo.com/videoplayback"

        def mock_fetch(req_url, headers):
            return 403, {}, b"Forbidden"

        discovered, saw_403 = sample_range(url, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertTrue(saw_403)

    def test_sample_range_non_206_rejected(self):
        url = "https://rr1.googlevideo.com/videoplayback"

        def mock_fetch(req_url, headers):
            return 200, {}, b"OK but not 206 partial content"

        discovered, saw_403 = sample_range(url, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)

    def test_redirect_to_external_domain_rejected_without_connecting(self):
        initial_url = "https://rr1.googlevideo.com/stream1"
        external_url = "https://attacker.com/malicious_stream"
        external_queried = False

        def mock_fetch(req_url, headers):
            nonlocal external_queried
            if req_url == initial_url:
                return 302, {"location": external_url}, b""
            if req_url == external_url:
                external_queried = True
                return 206, {"content-range": "bytes 0-1023/10000"}, b"x" * 1024
            return 404, {}, b""

        discovered, saw_403 = sample_range(initial_url, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)
        self.assertFalse(external_queried, "Must not connect to external domain!")

    def test_redirect_to_internal_address_rejected_without_connecting(self):
        initial_url = "https://rr1.googlevideo.com/stream1"
        internal_url = "https://127.0.0.1:8091/private"
        internal_queried = False

        def mock_fetch(req_url, headers):
            nonlocal internal_queried
            if req_url == initial_url:
                return 302, {"location": internal_url}, b""
            if req_url == internal_url:
                internal_queried = True
                return 200, {}, b""
            return 404, {}, b""

        discovered, saw_403 = sample_range(initial_url, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)
        self.assertFalse(internal_queried, "Must not connect to internal loopback!")

    def test_redirect_to_non_https_rejected(self):
        initial_url = "https://rr1.googlevideo.com/stream1"
        http_url = "http://rr1.googlevideo.com/stream2"
        http_queried = False

        def mock_fetch(req_url, headers):
            nonlocal http_queried
            if req_url == initial_url:
                return 302, {"location": http_url}, b""
            if req_url == http_url:
                http_queried = True
                return 206, {"content-range": "bytes 0-1023/10000"}, b"x" * 1024
            return 404, {}, b""

        discovered, saw_403 = sample_range(initial_url, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)
        self.assertFalse(http_queried, "Must not connect to insecure HTTP scheme!")

    def test_redirect_loop_detected_and_rejected(self):
        url_a = "https://rr1.googlevideo.com/stream_a"
        url_b = "https://rr2.googlevideo.com/stream_b"

        def mock_fetch(req_url, headers):
            if req_url == url_a:
                return 302, {"location": url_b}, b""
            if req_url == url_b:
                return 302, {"location": url_a}, b""
            return 404, {}, b""

        discovered, saw_403 = sample_range(url_a, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)

    def test_self_redirect_cycle_detected(self):
        url_a = "https://rr1.googlevideo.com/stream_self"

        def mock_fetch(req_url, headers):
            return 302, {"location": url_a}, b""

        discovered, saw_403 = sample_range(url_a, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)

    def test_redirect_limit_exceeded_rejected(self):
        # 3 redirects when MAX_REDIRECTS is 2
        u1 = "https://rr1.googlevideo.com/s1"
        u2 = "https://rr2.googlevideo.com/s2"
        u3 = "https://rr3.googlevideo.com/s3"
        u4 = "https://rr4.googlevideo.com/s4"

        def mock_fetch(req_url, headers):
            if req_url == u1:
                return 302, {"location": u2}, b""
            if req_url == u2:
                return 302, {"location": u3}, b""
            if req_url == u3:
                return 302, {"location": u4}, b""
            if req_url == u4:
                return 206, {"content-range": "bytes 0-1023/10000"}, b"x" * 1024
            return 404, {}, b""

        discovered, saw_403 = sample_range(u1, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 0)
        self.assertFalse(saw_403)

    def test_valid_redirects_within_limit_succeed(self):
        # 2 redirects within MAX_REDIRECTS (2)
        u1 = "https://rr1.googlevideo.com/s1"
        u2 = "https://rr2.googlevideo.com/s2"
        u3 = "https://rr3.googlevideo.com/s3"

        def mock_fetch(req_url, headers):
            if req_url == u1:
                return 302, {"location": u2}, b""
            if req_url == u2:
                return 302, {"location": u3}, b""
            if req_url == u3:
                return 206, {"content-range": "bytes 0-1023/50000"}, b"x" * 1024
            return 404, {}, b""

        discovered, saw_403 = sample_range(u1, 0, 1023, fetch_func=mock_fetch)
        self.assertEqual(discovered, 50000)
        self.assertFalse(saw_403)

    def test_no_redirect_opener_does_not_auto_follow(self):
        # Spawns a local test server to verify urllib opener directly
        unsafe_hit = False

        class RedirHandler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                nonlocal unsafe_hit
                if self.path == "/origin":
                    self.send_response(302)
                    self.send_header("Location", "/unsafe")
                    self.end_headers()
                elif self.path == "/unsafe":
                    unsafe_hit = True
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(b"unwanted")

            def log_message(self, *args):
                pass

        server = http.server.HTTPServer(("127.0.0.1", 0), RedirHandler)
        port = server.server_address[1]
        t = threading.Thread(target=server.serve_forever, daemon=True)
        t.start()
        try:
            req = urllib.request.Request(f"http://127.0.0.1:{port}/origin")
            resp = _NO_REDIRECT_OPENER.open(req, timeout=2)
            # Response must be 302, not 200, and /unsafe must not have been requested
            self.assertEqual(getattr(resp, "status", getattr(resp, "code", 0)), 302)
            self.assertFalse(unsafe_hit)
        finally:
            server.shutdown()
            server.server_close()

    def test_validate_media_ranges_all_three_points(self):
        url = "https://rr1.googlevideo.com/videoplayback?clen=50000"
        sampled_starts = []

        def mock_fetch(req_url, headers):
            range_hdr = headers.get("Range", "")
            parts = range_hdr.replace("bytes=", "").split("-")
            start, end = int(parts[0]), int(parts[1])
            sampled_starts.append(start)
            resp_headers = {"content-range": f"bytes {start}-{end}/50000"}
            return 206, resp_headers, b"x" * (end - start + 1)

        is_valid, saw_403 = validate_media_ranges(
            url, declared_size=50000, fetch_func=mock_fetch
        )
        self.assertTrue(is_valid)
        self.assertFalse(saw_403)
        # Must sample head (0), mid (25000), and tail (48976)
        self.assertIn(0, sampled_starts)
        self.assertIn(25000, sampled_starts)
        self.assertIn(50000 - 1024, sampled_starts)

    def test_validate_media_ranges_tail_403_fails(self):
        url = "https://rr1.googlevideo.com/videoplayback?clen=50000"

        def mock_fetch(req_url, headers):
            range_hdr = headers.get("Range", "")
            if "bytes=0-" in range_hdr:
                return 206, {"content-range": "bytes 0-1023/50000"}, b"x" * 1024
            # Tail or mid returns 403
            return 403, {}, b"Forbidden"

        is_valid, saw_403 = validate_media_ranges(
            url, declared_size=50000, fetch_func=mock_fetch
        )
        self.assertFalse(is_valid)
        self.assertTrue(saw_403)


if __name__ == "__main__":
    unittest.main()
