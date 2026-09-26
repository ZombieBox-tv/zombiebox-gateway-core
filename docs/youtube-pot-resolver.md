# YouTube PO Token HD Resolver Architecture and Design Note

Status: Development Candidate (Opt-in)

Date: 2026-09-25

## 1. Context and Goal

The baseline ZombieBox YouTube integration (`wrappers/youtube`) uses YouTube.js to provide catalog browsing, channel feeds, and a validated baseline 360p progressive stream (combined H.264/AAC at 30 fps). YouTube's current enforcement varies by player client. Following the [yt-dlp PO Token Guide](https://github.com/yt-dlp/yt-dlp/wiki/Po-Token-Guide), the optional resolver first tries yt-dlp's default clients without plugins, then can select `mweb` and use the [BgUtils provider plugin](https://github.com/Brainicism/bgutil-ytdlp-pot-provider) for its GVS PO token flow.

This increment implements an optional, bounded YouTube HD resolver path for **Full** using `yt-dlp`, with a PO Token Provider as a separate fallback candidate. It supplements the existing 360p stream with validated H.264 (AVC) <= 30 fps video paired with AAC audio (720p and 1080p tiers) where the source permits, while preserving the YouTube.js catalog, DIAL, and baseline fallback paths. An explicit quality request must resolve to that exact validated tier; it must never receive a 360p response while being treated as HD. Auto and unspecified quality may use the baseline fallback. The provider does not guarantee access to formats, and host extraction or range checks do not prove TV playback. Do not claim above-360p support until the selected physical device completes playback at that quality.

---

## 2. Review of PO Token Provider Plugins

Per project requirements, both prominent `yt-dlp` PO Token plugins were evaluated:

### 2.1 Plugin A: `Brainicism/bgutil-ytdlp-pot-provider` (Selected Initial Implementation)

- **Release Identities:**
  - PyPI package: `bgutil-ytdlp-pot-provider==2.0.0`
  - Container image: `brainicism/bgutil-ytdlp-pot-provider:2.0.0@sha256:79b7d390e024f3308a0b8fe2041be7e4e4d6a8c30ad6052a89bc51d210c385f1`
  - Paired `yt-dlp`: `yt-dlp==2026.8.19`
  - The resolver image pins Alpine packages in its Dockerfile and all Python packages in `wrappers/youtube-pot/requirements.lock`, including `yt-dlp-ejs==0.8.0`. It enables the pinned Node.js runtime with `--js-runtimes node` for yt-dlp's challenge scripts.
- **License:** GNU General Public License v3.0 (`GPL-3.0-only` / `GPL-3.0-or-later`). This aligns directly with first-party ZombieBox code licensing under GPL-3.0-only.
- **Mechanism:**
  - Consists of two components:
    1. A lightweight Node.js/JavaScript HTTP daemon running LuanRT's Botguard interpreter on port 4416. It handles `POST /get_pot` requests with `content_binding` (video ID for video-bound player tokens or visitor data for GVS tokens).
    2. A Python provider plugin for `yt-dlp` that automatically queries this server via `--extractor-args "youtubepot-bgutilhttp:base_url=http://..."`.
  - The resolver first tries yt-dlp's default player clients with `--no-plugin-dirs`, allowing direct format extraction without a PO-token plugin. If that candidate fails exact-tier validation and the PO path is not in cooldown, it tries `youtube:player_client=mweb` with BgUtils. The PO path does not reuse the worker's static `poToken` or `visitorData` fields, and its 403 cooldown does not prevent a later default-client attempt.
- **Resource Profile:**
  - **Memory:** Not measured on this deployment. The Compose profile imposes a 256 MiB container limit; actual usage and failure under load require validation.
  - **CPU:** Not measured; challenge generation, yt-dlp extraction and validation each consume CPU and network time.
  - **Image Footprint:** Not measured for the selected image digest.
- **Security & Network Isolation:**
  - The HTTP server provides **no authentication**. Upstream explicitly warns that exposing this port to untrusted networks or binding to `0.0.0.0` on the host allows unauthenticated clients to generate tokens, exhaust CPU/memory, or expose remote code execution (RCE) vulnerabilities.
  - **Strict Constraint:** The `bgutil-provider` container must **never** publish ports to the host or LAN (`ports:` is prohibited). It is reachable by other services on the Compose network; `expose: [4416]` does not restrict access between those containers.

### 2.2 Plugin B: `coletdjnz/yt-dlp-getpot-wpc` (Documented Fallback Design)

- **Release Identities:**
  - PyPI package: `yt-dlp-getpot-wpc==1.1.2`
  - Requires `yt-dlp>=2025.09.26` and Chrome/Chromium.
- **License:** MIT License.
- **Mechanism:**
  - Uses `nodriver` (Python Chrome DevTools Protocol automation) to launch a real Chromium browser instance.
  - Navigates to YouTube's "WebPoClient" page in the browser to mint Proof-of-Origin tokens directly through the live browser environment.
- **Chromium Executable Reuse and Isolation:**
  - It can reuse the project's existing Chromium binary (e.g., `/usr/bin/chromium` from Alpine `chromium=149.0.7827.53-r0` packaged for Rebrowser) via:
    `--extractor-args "youtubepot-wpc:browser_path=/usr/bin/chromium"`
  - **ADR 0021 Boundary Protection:** ADR 0021 strictly forbids credential leakage, session sharing, remote JavaScript injection, or cross-device session transfer with the Rebrowser worker. Therefore:
    - WPC **cannot and must not** attach to or share the active Rebrowser container or profile.
    - If WPC were invoked, it would require its own independent, isolated, ephemeral user data directory (`--user-data-dir=/tmp/...`) with all sandbox flags enforced.
- **Why WPC is Rejected for Primary Runtime:**
  - **Resources:** A separate Chromium process adds memory, CPU and startup latency on the 8 GB host. These costs have not been benchmarked for this project.
  - **Stability:** Headless browser automation under the current container restrictions has not been verified.
- **Conclusion:** WPC is retained as a documented fallback architecture only. It is not bundled or routinely run.

---

## 3. Full vs. Edge Architecture Split

- **Gateway Full (Linux/Docker):**
  - Uses containerized microservices on a private bridge network.
  - End users require only Docker Engine and Docker Compose (ADR 0031); no host Python, Node.js, or FFmpeg is required.
  - The resolver stack (`youtube-pot` and `bgutil-provider`) runs in bounded containers with non-root users, read-only root filesystems, and strict memory/CPU limits.
- **Gateway Edge (Android/Bionic/Termux):**
  - **Status: UNVERIFIED** pending native Android/Bionic and Termux validation.
  - `bgutil-ytdlp-pot-provider` requires native compilation of `canvas`, which depends on `libvips`, `xorgproto`, and an Android NDK toolchain (`~/.gyp/include.gypi`). These are absent by default on Termux and Android Bionic environments.
  - Chromium cannot be run headless on Termux without complex external X11/VNC desktop shims and patched user namespaces.
  - Therefore, Edge claims are explicitly not made; Edge retains the standard tokenless YouTube.js 360p path until proven otherwise.

---

## 4. Resolver Implementation & Operational Guardrails

The resolver (`gateway-core/wrappers/youtube-pot`) is a bounded, private HTTP adapter providing the exact semantic wire contract expected by the Gateway:

```json
{
  "url": "https://...googlevideo.com/...",
  "audioUrl": "https://...googlevideo.com/...",
  "mimeType": "video/mp4",
  "variants": ["1080p", "720p", "480p", "360p"]
}
```

### 4.1 Strict Validation and Limits

1. **Identifier and Input Sanitization:**
   - Video ID must match `^[A-Za-z0-9_-]{11}$`.
   - Quality parameter must be one of `1080p`, `720p`, `480p`, `360p`, `auto`, or empty.

2. **Codec Constraints (Initial Baseline Compatibility Profile):**
   - The resolver enforces an initial conservative compatibility profile:
     - Video codec: H.264 / AVC (`avc1` or `h264`).
     - Frame rate: <= 30 fps (`fps <= 30`). 50fps and 60fps high-rate streams are dropped.
     - Audio codec: AAC (`mp4a` or `aac`). Opus and Vorbis are strictly rejected.
   - **Architectural Scope & Adaptive Devices:** This is an initial compatibility candidate for the selected legacy TV. Codec and frame-rate selection still require device-specific probe and playback evidence; these constraints do not establish smooth playback. The current worker applies this conservative profile to every device, so modern-device VP9, AV1, 60 fps and 4K selection remains an open implementation gap. Per `docs/development/adaptive-device-coverage.md`, later profiles must use device capability probes rather than a model or age rule.
   - **Resolution Tier Truthfulness:** `format_tier` accurately maps video heights up to 2160p (4K) without relabeling or over-advertising 1440p (2K) or 2160p (4K) streams as `1080p`.

3. **Stream Origin Validation & SSRF Boundary:**
   - Stream URLs must be HTTPS on standard port 443 with TLS certificate verification against trusted CAs, contain no user credentials, have no explicit port, and have hostnames strictly matching valid RFC 1123 subdomains of `googlevideo.com`.
   - **Why DNS/IP Pinning is Inappropriate:** Google CDN edge caches resolve dynamically to rotating global edge IPs across AS15169. Pinning IPs or resolving hostnames ahead of time in application code would break stream delivery or create fragile false negatives.
   - **Redirect Protection:** Automatic redirect following in urllib is completely disabled (`_NoRedirectHandler`). Every redirect `Location` must independently satisfy `is_googlevideo_url` before any connection is made. At most two (2) redirects are permitted, and cyclical loops are detected and rejected immediately. Unsafe redirects to external domains, intranet addresses, or non-HTTPS schemes fail without connecting.

4. **Three-Point Range Verification (Head, Mid, Tail) and URL-Bound Caching:**
   - A simple HTTP 200 or single-byte probe is insufficient because YouTube frequently returns 403 on subsequent chunks.
   - The resolver validates three 1 KiB byte ranges:
     - Head: bytes 0–1023
     - Mid: `floor(total / 2)` to `floor(total / 2) + 1023`
     - Tail: `total - 1024` to `total - 1`
   - All three probes must return HTTP 206 Partial Content with a valid `Content-Range` matching the requested boundaries. Any failure or HTTP 403 marks the format candidate as invalid.
   - Manual requests probe only the requested video tier and, for split video, one validated AAC pairing. Auto continues validating its full candidate inventory so its reported variants remain truthful.
   - Range validation cache in `FormatSelector` is keyed strictly by `(url, declared_size)`, never by `format_id` alone. This prevents reusing validation results across different signed URLs sharing the same itag.

5. **Single-Flight Concurrency (`concurrency: 1`):**
   - To prevent memory bloat and rate-limiting bursts, an internal mutex limits video resolution to exactly one active extraction at a time.
   - Concurrent requests immediately receive HTTP 503 `{"error": "busy"}`. The Gateway's existing client retries up to 5 times with exponential backoff.

6. **Default Client, PO Candidate, Cooldown & Quality-Truthful Fallback:**
   - Default yt-dlp player clients are attempted first with `--no-plugin-dirs`, and candidate formats still require the same codec and range validation.
   - The mweb/BgUtils PO path is attempted only when the default candidate cannot provide the requested result and the PO cooldown is inactive. A 403 from the PO path during extraction or range validation activates a 300-second cooldown for that path. A default-client 403 does not suppress the distinct PO candidate.
   - A later request always tries default clients even while the PO path is in cooldown.
   - **Automatic and Unspecified Quality:** During cooldown, extraction failures, or failed rendition validation, these requests may use the upstream YouTube.js worker's baseline stream (`http://youtube:8091`). The resolver does not forward a quality parameter to that baseline worker.
   - **Manual Exact Quality:** Requests for `1080p`, `720p`, `480p`, or `360p` return HTTP 502 with `{"error":"quality_unavailable"}` when the selected tier cannot be resolved and validated. They never silently switch to another tier. The error body contains no extraction details or signed stream URL. Successful selections include `actualHeight` and `actualQuality` when matching numeric height metadata is available; a quality label alone is not treated as proof of the actual height.

7. **Boundedness, Privacy, & Ephemeral Secrets:**
   - A single 18-second wall-clock budget covers default extraction, PO fallback, range validation and the automatic baseline fallback, leaving time inside the Gateway's 20-second HTTP client deadline. Each `yt-dlp` process also has a hard 30-second ceiling and receives only the remaining shared budget. Stdout is capped at 4 MiB and stderr at 256 KiB; overflow and timeout terminate the entire process group, including a Node.js challenge child. Range requests use the remaining budget as their network timeout. No subprocess output or signed media URL is written to logs.
   - **Bounded Reads:** Proxying `/catalog` and `/browse` responses caps incoming body size at 8 MiB (matching the Gateway's `providers/http.go` `8<<20` limit). Resolution fallback reads are capped at 1 MiB. Upstream error bodies are capped at 64 KiB.
   - **Safe Error Forwarding:** Upstream error bodies are parsed for JSON errors without echoing raw HTML backtraces, unexpected headers, or cookies.
   - **Log Sanitization:** Authentication bearer tokens and signed googlevideo URL query parameters (which embed client IP and signatures) are redacted from all logging and exception strings.

8. **Catalog and Browse Passthrough (Zero Catalog Fork):**
   - `/catalog` and `/browse` requests are transparently proxied to the upstream YouTube worker (`http://youtube:8091`). The resolver does not fork or duplicate catalog retrieval.

---

## 5. Third-Party Licenses and Notices

| Component | Upstream Origin | Version / Commit | License | Role |
| :--- | :--- | :--- | :--- | :--- |
| `bgutil-ytdlp-pot-provider` | Brainicism | 2.0.0 | GPL-3.0-or-later | PO Token plugin & JS server |
| `yt-dlp` | yt-dlp project | 2026.8.19 | Unlicense (Public Domain) | Extractor and format engine |
| `yt-dlp-getpot-wpc` | coletdjnz | 1.1.2 | MIT | Fallback reference only (not bundled) |
| `YouTube.js` | LuanRT | 18.0.0 | MIT | Existing catalog/browse & 360p worker |

All distribution artifacts must retain the matching license texts and source notices.
