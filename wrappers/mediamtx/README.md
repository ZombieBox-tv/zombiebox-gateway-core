# MediaMTX boundary

Pinned runtime: 1.21.1, source commit 048255986f7e04b859b4c4efe651448ec785ecd4. Full uses the digest-pinned upstream image; Edge builds that source natively with `android.patch`. The patch excludes Raspberry Pi sources from Android build constraints and selects the existing unsupported-camera implementation. It is applied only to a disposable build copy.

The minimal config disables RTMP, WebRTC, SRT and MoQ. RTSP is TCP-only; HLS is MPEG-TS. Authentication is delegated to a gateway callback with a private key. Publishing and reading have separate per-cast credentials. API access uses the gateway's private operator identity; it is not exposed to Android clients.

MediaMTX 1.21.1 allocates a reader when fetching the HLS master playlist. The gateway retains the selected variant URL from readiness, including MediaMTX's private session query, and rewrites every client-visible URI to opaque tickets. Repeated client playlist requests must not allocate readers. The real integration smoke verifies this and tests publisher termination.

See [mirroring](../../docs/development/mirroring.md), [Edge](../../gateway-edge/README.md), [ADR 0019](../../docs/adr/0019-mirroring-relay-and-native-edge-packaging.md) and [upstream license](../../third_party/mediamtx-LICENSE.txt). No claim of AirPlay support follows from this relay.
