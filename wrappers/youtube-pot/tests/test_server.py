import http.server
import json
import socket
import sys
import threading
import unittest
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

WRAPPER_DIR = Path(__file__).resolve().parents[1]
if str(WRAPPER_DIR) not in sys.path:
    sys.path.insert(0, str(WRAPPER_DIR))

from cooldown import CooldownTracker
from server import (
    MAX_CATALOG_BODY_BYTES,
    make_handler,
    sanitize_log_message,
)


def get_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class ServerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.token = "test-worker-token-secret-1234567890abcdef"
        cls.upstream_port = get_free_port()
        cls.server_port = get_free_port()

        # Dummy upstream server
        class UpstreamHandler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                if "?mode=oversized" in self.path:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b"x" * (MAX_CATALOG_BODY_BYTES + 100))
                elif "?mode=corrupt" in self.path:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b"NOT VALID JSON <HTML>")
                elif "?mode=html-error" in self.path:
                    self.send_response(500)
                    self.send_header("Content-Type", "text/html")
                    self.end_headers()
                    self.wfile.write(
                        b"<html>Internal Server Error: /root/secret/pass</html>"
                    )
                elif self.path.startswith("/catalog"):
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b'{"items":[{"id":"dQw4w9WgXcQ","title":"Test"}]}')
                elif self.path.startswith("/browse"):
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b'{"items":[]}')
                elif self.path.startswith("/resolve/dQw4w9WgXcQ"):
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(
                        b'{"url":"https://rr1.googlevideo.com/baseline","mimeType":"video/mp4","variants":["360p"]}'
                    )
                else:
                    self.send_response(404)
                    self.end_headers()

            def log_message(self, format, *args):
                pass

        cls.upstream = http.server.HTTPServer(
            ("127.0.0.1", cls.upstream_port), UpstreamHandler
        )
        cls.upstream_thread = threading.Thread(
            target=cls.upstream.serve_forever, daemon=True
        )
        cls.upstream_thread.start()

        # Wrapper server
        cls.cooldown_tracker = CooldownTracker()
        cls.config = {
            "token": cls.token,
            "upstream_url": f"http://127.0.0.1:{cls.upstream_port}",
            "bgutil_url": "http://127.0.0.1:4416",
            "cooldown_seconds": 300,
            "timeout_seconds": 2,
        }
        handler_class = make_handler(cls.config, cls.cooldown_tracker)
        cls.server = http.server.HTTPServer(
            ("127.0.0.1", cls.server_port), handler_class
        )
        cls.server_thread = threading.Thread(
            target=cls.server.serve_forever, daemon=True
        )
        cls.server_thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.upstream.shutdown()
        cls.upstream.server_close()

    def request(self, path, token=None):
        url = f"http://127.0.0.1:{self.server_port}{path}"
        headers = {}
        if token is not None:
            headers["Authorization"] = f"Bearer {token}"
        req = urllib.request.Request(url, headers=headers, method="GET")
        try:
            with urllib.request.urlopen(req, timeout=3) as resp:
                return resp.status, json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as err:
            with err:
                body = err.read().decode("utf-8")
                try:
                    return err.code, json.loads(body)
                except Exception:
                    return err.code, body

    def test_health(self):
        status, data = self.request("/health")
        self.assertEqual(status, 200)
        self.assertEqual(data.get("status"), "ok")
        self.assertEqual(data.get("provider"), "youtube-pot")
        self.assertFalse(data.get("busy"))
        self.assertFalse(data.get("cooldown"))

    def test_auth_rejection(self):
        # Missing token
        status, data = self.request("/catalog")
        self.assertEqual(status, 401)
        # Invalid token
        status, data = self.request("/catalog", token="wrong-token")
        self.assertEqual(status, 401)

    def test_catalog_and_browse_proxied_to_upstream(self):
        status, data = self.request("/catalog?q=nature", token=self.token)
        self.assertEqual(status, 200)
        self.assertIn("items", data)
        self.assertEqual(data["items"][0]["id"], "dQw4w9WgXcQ")

        status, data = self.request("/browse?parent=channel", token=self.token)
        self.assertEqual(status, 200)
        self.assertIn("items", data)

    def test_proxy_upstream_oversized_response(self):
        status, data = self.request("/catalog?mode=oversized", token=self.token)
        self.assertEqual(status, 502)
        self.assertEqual(data.get("error"), "provider_response_too_large")

    def test_proxy_upstream_corrupt_response(self):
        status, data = self.request("/catalog?mode=corrupt", token=self.token)
        self.assertEqual(status, 502)
        self.assertEqual(data.get("error"), "invalid_browse_response")

    def test_proxy_upstream_html_error_does_not_leak_secrets(self):
        status, data = self.request("/catalog?mode=html-error", token=self.token)
        self.assertEqual(status, 500)
        self.assertEqual(data.get("error"), "provider_unavailable")
        self.assertNotIn("secret", json.dumps(data).lower())

    def test_log_sanitization_masks_tokens_and_urls(self):
        msg = "Handling Bearer secret-worker-token-xyz for https://rr1.googlevideo.com/videoplayback?expire=999&sig=sensitive"
        clean = sanitize_log_message(msg)
        self.assertNotIn("secret-worker-token-xyz", clean)
        self.assertIn("Bearer [REDACTED]", clean)
        self.assertNotIn("expire=999", clean)
        self.assertNotIn("sig=sensitive", clean)
        self.assertIn("https://rr1.googlevideo.com/videoplayback", clean)

    def test_resolve_invalid_id(self):
        status, data = self.request("/resolve/short", token=self.token)
        self.assertEqual(status, 400)
        self.assertEqual(data.get("error"), "invalid_id")

    def test_resolve_invalid_quality(self):
        status, data = self.request("/resolve/dQw4w9WgXcQ?quality=4k", token=self.token)
        self.assertEqual(status, 400)
        self.assertEqual(data.get("error"), "invalid_quality")

    def test_resolve_single_flight_busy(self):
        # Hold the flight lock
        self.assertTrue(self.cooldown_tracker.acquire_flight())
        try:
            status, data = self.request("/resolve/dQw4w9WgXcQ", token=self.token)
            self.assertEqual(status, 503)
            self.assertEqual(data.get("error"), "busy")
        finally:
            self.cooldown_tracker.release_flight()


if __name__ == "__main__":
    unittest.main()
