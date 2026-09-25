# Hardware Capability and Playback Evidence Audit

This document audits the current capability discovery, registration, persistence, probe cache validation, and playback planning chain against `resources/hardware-rules/hardware-approachment.md` and ADR 0004. The native MPEG-TS HLS path described below is one implemented slice; the remaining criteria and physical device acceptance stay open.

---

## 1. Discovery, Persistence, and Planning Chain

The pipeline follows the strict principle:
**Detect → Probe → Persist → Validate Cache → Plan (Least-Cost) → Fallback.**

```
Client Scanner (PlatformInventory / HardwareScanner)
  │ [DECLARED hints: MediaCodecList, API, display, network type]
  ▼
Registration (POST /v1/devices/register)
  │ [Persisted in Gateway SQLite 'devices' bucket]
  ▼
Probe Suite Manifest (GET /v1/probes?suite=2&extended=1)
  │ [Signed tickets for bounded synthetic media fixtures]
  ▼
Client Active Probes (MediaProbePlayback via MediaPlayer)
  │ [FUNCTIONAL execution: prepare + advancement + completion]
  ▼
Capabilities Upload (PUT /v1/device/capabilities)
  │ [Validated against CacheKey = SHA256(Platform, ClientVersion, Fingerprint, Suite)]
  ▼
Authenticated Request (X-Zombie-Device + Token)
  │ [CurrentCapabilities validates suite version and fingerprint freshness]
  ▼
Playback Planner (playback.LocalMode & server.playbackMode)
  │ [Least-cost evidence-gated resolution: DIRECT_PLAY → REMUX → TRANSCODE → EXTERNAL_PLAYER]
```

### Evidence Classes
- **DECLARED**: System API declarations (e.g., `MediaCodecList`, `Build.VERSION.SDK_INT`, `systemAvailableFeatures`). Never sufficient on their own to grant direct playback.
- **FUNCTIONAL PASS**: Concrete execution evidence on the device's native media stack with confirmed timeline advancement and completion on synthetic test fixtures. A listed probe is only an available test; its result must be PASS on that device.
- **RUNTIME HEALTH**: Longitudinal metrics from real media sessions (decoder stalls, dropped frames, disconnects, circuit-breaker status).
- **UNKNOWN**: Missing evidence, untested capability, or invalidated/stale cache key.

---

## 2. Capability Evidence Matrix

| Area | Item / Capability | Scanner / Detection | Probe Available | Gateway Persistence | Planner Integration | Evidence Class | Status |
|---|---|---|---|---|---|---|---|
| **System** | Android API & Release | `Build.VERSION` | N/A | SQLite `devices` | Excluded from decision | DECLARED | Present |
| **System** | CPU ABI & Cores | `Build` + `/proc/cpuinfo` | N/A | SQLite `devices` | Fingerprint hashing | DECLARED | Present |
| **System** | RAM (Physical & Class) | `/proc/meminfo` + `ActivityManager` | N/A | SQLite `devices` | Low-memory constraints | DECLARED | Present |
| **System** | Display & Refresh Rate | `DisplayDiscovery` (API 17/23) | N/A | SQLite `devices` | Resolution ceiling | DECLARED | Present |
| **System** | OpenGL ES Version | `ConfigurationInfo.reqGlEsVersion` | N/A | SQLite `devices` | Not yet used by render policy | DECLARED | Present |
| **Video Codec** | H.264 Baseline (360p/480p) | `PlatformInventory` | `h264-baseline-360/480` | `domain.Capabilities` | `LocalMode` (videoCandidate) | FUNCTIONAL PASS | Present |
| **Video Codec** | H.264 Main 720p | `PlatformInventory` | `h264-720-main` | `domain.Capabilities` | `LocalMode` (videoCandidate) | FUNCTIONAL PASS | Present |
| **Video Codec** | H.264 High 720p/1080p | `PlatformInventory` | `h264-720/1080-high` | `domain.Capabilities` | `LocalMode` (videoCandidate) | FUNCTIONAL PASS | Present |
| **Video Codec** | H.264 UHD 4K (2160p) | `PlatformInventory` | `h264-2160-high` (extended) | `domain.Capabilities` | `extendedVideoCandidate` | FUNCTIONAL PASS | Present |
| **Video Codec** | HEVC Main 1080p/2160p | `PlatformInventory` | `hevc-1080/2160-main` (extended) | `domain.Capabilities` | `extendedVideoCandidate` | FUNCTIONAL PASS | Present |
| **Video Codec** | VP8 | `PlatformInventory` | Missing | Stored in Decoders | Not planned (forces transcode) | DECLARED | Missing probe |
| **Video Codec** | VP9 | `PlatformInventory` | Missing | Stored in Decoders | Not planned (forces transcode) | DECLARED | Missing probe |
| **Audio Codec** | AAC-LC | `PlatformInventory` | `aac` (`aac.m4a`) | `domain.Capabilities` | `LocalMode` / `SelectedAudioMode` | FUNCTIONAL PASS | Present |
| **Audio Codec** | MP3 | Missing | Missing | Not probed | `LocalMode` (codec hint only) | DECLARED | Missing probe |
| **Audio Codec** | AC3 / EAC3 | `PlatformInventory` | Missing | Stored in Decoders | Not planned (forces transcode) | DECLARED | Missing probe |
| **Audio Codec** | Opus / Vorbis | `PlatformInventory` | Missing | Stored in Decoders | Not planned (forces transcode) | DECLARED | Missing probe |
| **Streaming** | HTTP Progressive MP4 | Network type | `h264-*` fixtures | `domain.Capabilities` | `LocalMode` (`http-progressive`) | FUNCTIONAL PASS | Present |
| **Streaming** | HLS (H.264/AAC MPEG-TS) | Manifest inspection | `hls-h264-aac` | `domain.Capabilities` | `LocalMode` (`nativeHLSCandidate`) | FUNCTIONAL PASS | **Implemented** |
| **Streaming** | HLS (fMP4 segments) | `isFMP4` detection | Missing | N/A | Fallback to Gateway REMUX | UNKNOWN | Missing probe |
| **Streaming** | DASH (Clear) | Manifest inspection | Missing | N/A | Fallback to Gateway REMUX | UNKNOWN | Missing probe |
| **Streaming** | RTSP | Endpoint diagnostic | Missing | Diagnostic report only | Not planned for embedded player | UNKNOWN | Missing probe |
| **Container** | MP4 | Format inspection | `baseline-*.mp4` | N/A | `LocalMode` (Direct Play) | FUNCTIONAL PASS | Present |
| **Container** | MPEG-TS | Format inspection | `mpegts-h264-aac` | `domain.Capabilities` | Direct via HLS, Remux otherwise | FUNCTIONAL PASS | Present |
| **Container** | Fragmented MP4 (fMP4) | Format inspection | `http-fmp4` | `domain.Capabilities` | Conversion gating | FUNCTIONAL PASS | Present |
| **Container** | MKV / WebM | Format inspection | Missing | N/A | Fallback to Gateway REMUX | UNKNOWN | Missing probe |
| **Container** | AVI / FLV / ASF | Format inspection | Missing | N/A | Fallback to Gateway REMUX | UNKNOWN | Missing probe |
| **Network** | Ethernet / Wi-Fi | `ConnectivityManager` | Connectivity state | SQLite `devices` | Display / diagnostic hint | DECLARED | Present |
| **Network** | Gateway Latency | `GatewayProbeRepository` | Round-trip timestamp | Registration payload | Planner quality headroom | FUNCTIONAL PASS | Present |
| **Network** | Throughput / Bandwidth | Rolling window | Runtime sampling | In-memory session | `bandwidth.go` budgeting | RUNTIME HEALTH | Partial |
| **Network** | Multicast / SSDP | Active local socket loopback | `multicast-loopback`; no LAN/SSDP peer test | `domain.Capabilities` | Discovery still needs LAN evidence | FUNCTIONAL PASS for local loopback only | Partial |
| **Lifecycle** | Seek / Resume | `MediaProbePlayback` | `seek`, `pause-resume` | `domain.Capabilities` | Diagnostic confirmation | FUNCTIONAL PASS | Present |
| **Lifecycle** | Surface Detach/Reattach | `MediaProbePlayback` | `surface-reattach` | `domain.Capabilities` | Background playback safety | FUNCTIONAL PASS | Present |
| **Health** | Runtime Circuit Breaker | Session monitoring | Real-world failure count | Partial (in-memory sessions) | Strategy degradation | RUNTIME HEALTH | Partial |

---

## 3. Native HLS Vertical Slice Implementation

### What Native HLS Direct Play Means
Native HLS Direct Play does **not** bypass the Gateway or leak provider secrets to the client. The client never receives upstream provider tokens, cookies, or raw URLs. Instead:
1. The Gateway issues an authenticated playback ticket for `/v1/streams/{session}?ticket={ticket}` with MIME `application/vnd.apple.mpegurl`.
2. When the device fetches this URL, the Gateway proxies the upstream playlist through `rewritePlaylist`.
3. Upstream variant playlists and media segment URLs are rewritten into opaque, session-scoped relay endpoints (`/v1/streams/{session}/{key}?ticket=...`).
4. The client's native Android `MediaPlayer` decodes and renders the stream directly from the Gateway proxy without invoking FFmpeg conversion processes.

### Evidence Gating Rules
Native HLS (`DIRECT_PLAY`) is strictly gated on:
- Valid probe suite revision (`SuiteVersion == 2`) and matching active cache key (`CacheKey == ProbeCacheKey(device)`).
- A positive `hls-h264-aac` probe result with `Status == "PASS"`, `Stalled == false`, and verified advancement (`PositionMS >= 500` or `Completed == true`).
- Probe timestamp freshness within 7 days (`TestedAt > now - 7*24*60*60` and `TestedAt <= now + 300`).
- Stream codec compatibility: video must be H.264 with a matching functional profile/resolution probe (the HLS fixture itself certifies Baseline 360p only); audio must be AAC with no active probe failure.
- Segment container compatibility: standard MPEG-TS segments. Segmented fragmented MP4 (`fMP4`) or alternative containers are not implied by the MPEG-TS HLS probe and fall back to Gateway remux.
- Independence from `http-progressive`: an HTTP progressive download failure does not disqualify native HLS streaming.
- Any missing probe, probe failure (`FAIL` or `UNKNOWN`), stale fingerprint, or codec incompatibility falls back to the existing bounded Gateway conversion path (`REMUX` for compatible codecs, `TRANSCODE` for unsupported codecs, or `EXTERNAL_PLAYER` if fragmented MP4 output is unsupported).

---

## 4. Remaining Phases for Wider Protocol and Benchmark Support

The following phases define the remaining work required to achieve full coverage of the 59 capability criteria without fabricating physical results:

### Phase 1: Wider Video Codec Probing (VP8, VP9, AV1)
- Add synthetic `vp8-720p.webm` and `vp9-720p.webm` probe assets to Gateway `probeAssets` under Suite 3.
- Gate probe asset dispatch on matching declared decoder names in `d.Registration.Hardware.Decoders`.
- Run VP8/VP9 decode tests only where a candidate decoder is present, then trust the playback result rather than Android version.
- Update `LocalMode` to allow `DIRECT_PLAY` for WebM containers when VP8/VP9 functional PASS evidence exists.

### Phase 2: Audio Codec and Passthrough Probing (AC3, EAC3, Opus)
- Add synthetic multi-channel and stereo AC3 (`ac3-stereo.mp4`) and Opus (`opus-stereo.webm`) probe fixtures.
- Test both software decode capability and HDMI/optical passthrough indicators (`AudioTrack` / `AudioManager` flags).
- In `audio_selection.go`, enable direct passthrough planning when AC3 PASS evidence is confirmed, eliminating unnecessary audio transcoding.

### Phase 3: DASH Manifest Probing and Native Client Support
- Add clear synthetic DASH MPD probe fixture (`dash-h264-aac`) to Gateway probe suite.
- Evaluate whether the target device's native media framework (or OEM MediaCodec stack) can parse and decode multi-period or dynamic DASH MPD natively.
- Retain Gateway loopback DASH-to-MP4 remuxing as the primary fallback when native DASH evidence is absent.

### Phase 4: RTSP Streaming Probing
- Implement bounded synthetic RTSP server fixture in Gateway diagnostics.
- Probe client RTSP playback capability via `rtsp://` loopback URL.
- Differentiate low-latency direct RTSP play from Gateway RTP-to-HLS/progressive relay.

### Phase 5: Active Network and Multicast Probes
- Extend the existing `multicast-loopback` probe with a bounded LAN peer/SSDP response test. Loopback PASS alone does not verify IGMP snooping, Wi-Fi multicast delivery or DIAL discovery.
  - Active Gateway throughput probe: timed fetch of a bounded synthetic asset to establish dynamic bitrate ceiling.

### Phase 6: Runtime Learning and Strategy Health Loop
- Connect real playback lifecycle events (`progress.go` with `stalled`, `droppedFrames`, `bufferUnderrun`) to persistent `StrategyHealth` records in SQLite.
- Implement automatic quality tier downscaling and strategy circuit-breaking:
  - If native HLS experiences >2 decoder stalls in a session, temporarily degrade strategy to Gateway remux MP4 for a 1-hour TTL.
  - After TTL expiration, permit retry of native HLS to prevent temporary network glitches from becoming permanent disqualifications.
