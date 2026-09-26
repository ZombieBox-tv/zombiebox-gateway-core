"""HTTP server adapter for optional YouTube PO Token HD resolver."""

from __future__ import annotations

import hmac
import http.server
import json
import logging
import os
import re
import signal
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict

from cooldown import CooldownTracker
from media_ranges import _NO_REDIRECT_OPENER
from resolver import resolve_video

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    handlers=[logging.StreamHandler(sys.stdout)],
)
logger = logging.getLogger("youtube-pot")

YOUTUBE_ID_PATTERN = re.compile(r"^[A-Za-z0-9_-]{11}$")
ALLOWED_QUALITIES = {"1080p", "720p", "480p", "360p", "auto", ""}

MAX_CATALOG_BODY_BYTES = 8 * 1024 * 1024  # 8 MiB (matches Gateway providers/http.go)
MAX_ERROR_BODY_BYTES = 64 * 1024  # 64 KiB


def sanitize_log_message(msg: str) -> str:
    """Sanitize message strings to prevent leaking tokens, credentials, or signed URL parameters."""
    if not isinstance(msg, str):
        msg = str(msg)
    # Redact Authorization bearer tokens
    cleaned = re.sub(
        r"Bearer\s+[A-Za-z0-9_\-\.]+", "Bearer [REDACTED]", msg, flags=re.IGNORECASE
    )
    # Strip query parameters from URLs
    cleaned = re.sub(
        r"https?://[^\s\"'>]+", lambda m: m.group(0).split("?")[0], cleaned
    )
    return cleaned


def load_config() -> Dict[str, Any]:
    """Load wrapper configuration from file or environment."""
    config_path = os.environ.get("ZOMBIE_YOUTUBE_POT_CONFIG", "/config/pot.json")
    if not os.path.exists(config_path):
        config_path = ".local/youtube-pot/pot.json"

    if os.path.exists(config_path):
        with open(config_path, "r", encoding="utf-8") as f:
            return json.load(f)

    token = os.environ.get("ZOMBIE_YOUTUBE_WORKER_TOKEN", "")
    return {
        "token": token,
        "upstream_url": os.environ.get(
            "ZOMBIE_UPSTREAM_YOUTUBE_URL", "http://youtube:8091"
        ),
        "bgutil_url": os.environ.get(
            "ZOMBIE_BGUTIL_URL", "http://bgutil-provider:4416"
        ),
        "cooldown_seconds": 300,
        "timeout_seconds": 12,
    }


def make_handler(config: Dict[str, Any], cooldown_tracker: CooldownTracker):
    """Factory to create an HTTP request handler with injected dependencies."""
    worker_token = config.get("token", "")

    class YouTubePotHandler(http.server.BaseHTTPRequestHandler):
        server_version = "ZombieBoxYouTubePOT/0.1.0"

        def log_message(self, format: str, *args: Any) -> None:
            # Redact query strings and tokens from logs to protect ephemeral signed URLs and secrets
            clean_args = []
            for arg in args:
                s = str(arg)
                if "?" in s:
                    s = s.split("?")[0]
                clean_args.append(sanitize_log_message(s))
            logger.info("%s - - " + format, self.address_string(), *clean_args)

        def reply_json(self, status: int, data: Dict[str, Any]) -> None:
            body = json.dumps(data).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def proxy_upstream(self, path_with_query: str) -> None:
            upstream = config.get("upstream_url", "http://youtube:8091").rstrip("/")
            target = f"{upstream}{path_with_query}"
            headers = {
                "Authorization": self.headers.get("Authorization", ""),
                "Accept": "application/json",
            }

            try:
                req = urllib.request.Request(target, headers=headers, method="GET")
                with _NO_REDIRECT_OPENER.open(req, timeout=10) as resp:
                    resp_body = resp.read(MAX_CATALOG_BODY_BYTES + 1)
                    if len(resp_body) > MAX_CATALOG_BODY_BYTES:
                        return self.reply_json(
                            502, {"error": "provider_response_too_large"}
                        )

                    # Ensure payload is valid JSON before forwarding
                    try:
                        parsed = json.loads(resp_body.decode("utf-8"))
                    except Exception:
                        return self.reply_json(
                            502, {"error": "invalid_browse_response"}
                        )

                    status = getattr(resp, "status", getattr(resp, "code", 200))
                    self.reply_json(status, parsed)
            except urllib.error.HTTPError as err:
                err_body = err.read(MAX_ERROR_BODY_BYTES + 1)
                if len(err_body) > MAX_ERROR_BODY_BYTES:
                    return self.reply_json(err.code, {"error": "provider_unavailable"})
                try:
                    parsed_err = json.loads(err_body.decode("utf-8"))
                    if isinstance(parsed_err, dict) and "error" in parsed_err:
                        self.reply_json(err.code, {"error": str(parsed_err["error"])})
                    else:
                        self.reply_json(err.code, {"error": "provider_unavailable"})
                except Exception:
                    self.reply_json(err.code, {"error": "provider_unavailable"})
            except Exception:
                self.reply_json(502, {"error": "provider_unavailable"})

        def do_GET(self) -> None:
            parsed = urllib.parse.urlsplit(self.path)
            path = parsed.path

            if path == "/health":
                in_cooldown = cooldown_tracker.is_in_cooldown()
                busy = False
                if cooldown_tracker.acquire_flight():
                    cooldown_tracker.release_flight()
                else:
                    busy = True
                return self.reply_json(
                    200,
                    {
                        "status": "ok",
                        "provider": "youtube-pot",
                        "busy": busy,
                        "cooldown": in_cooldown,
                    },
                )

            # Check authorization for other endpoints
            auth = self.headers.get("Authorization", "")
            expected_auth = f"Bearer {worker_token}"
            if not worker_token or not hmac.compare_digest(auth, expected_auth):
                return self.reply_json(401, {"error": "unauthorized"})

            # Transparently proxy catalog and browse to upstream without catalog fork
            if path in ("/catalog", "/browse"):
                full_path = self.path
                return self.proxy_upstream(full_path)

            if not path.startswith("/resolve/"):
                return self.reply_json(404, {"error": "not_found"})

            video_id = path[len("/resolve/") :]
            if not YOUTUBE_ID_PATTERN.match(video_id):
                return self.reply_json(400, {"error": "invalid_id"})

            query_params = urllib.parse.parse_qs(parsed.query)
            quality = query_params.get("quality", [""])[0]
            if quality not in ALLOWED_QUALITIES:
                return self.reply_json(400, {"error": "invalid_quality"})

            # Enforce single-flight concurrency
            if not cooldown_tracker.acquire_flight():
                return self.reply_json(503, {"error": "busy"})

            try:
                resolved = resolve_video(video_id, quality, config, cooldown_tracker)
                self.reply_json(200, resolved)
            except Exception as exc:
                logger.warning(
                    "Resolution failed for %s: %s",
                    video_id,
                    sanitize_log_message(str(exc)),
                )
                self.reply_json(502, {"error": "provider_unavailable"})
            finally:
                cooldown_tracker.release_flight()

    return YouTubePotHandler


def run_server(port: int = 8097) -> None:
    """Start the YouTube POT resolver HTTP daemon."""
    config = load_config()
    cooldown_tracker = CooldownTracker(
        default_cooldown_seconds=int(config.get("cooldown_seconds", 300))
    )
    handler_class = make_handler(config, cooldown_tracker)

    server = http.server.ThreadingHTTPServer(("0.0.0.0", port), handler_class)
    logger.info("YouTube POT resolver listening on port %d", port)

    def shutdown_signal(sig, frame):
        logger.info("Received shutdown signal, terminating...")
        server.shutdown()
        server.server_close()
        sys.exit(0)

    signal.signal(signal.SIGINT, shutdown_signal)
    signal.signal(signal.SIGTERM, shutdown_signal)

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    port_str = os.environ.get("PORT", "8097")
    run_server(int(port_str))
