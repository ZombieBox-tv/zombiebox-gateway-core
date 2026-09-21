# Extended decoder evidence

Full and Edge share an additive suite2 extension. Client requests
`/v1/probes?suite=2&extended=1`; only discovered `probeCandidates` in its hardware
report enable the matching extended asset. Old clients see their existing suite.
The three synthetic samples are H.264 High UHD30, HEVC Main1080p30 and MainUHD30,
8-bit yuv420p, BT.709 SDR, at most three seconds/eight MiB each. Generate them with:

```sh
python3 scripts/generate-extended-probes.py --output /path/to/probes
```

The generator validates encoded profile, dimensions, frame rate and color metadata
with ffprobe before reusing/serving an asset. CPU work is bounded to two codec
threads/pool workers and each encoder subprocess has a timeout. These are local
original fixtures, never embedded in an APK or downloaded provider content.

Client runs prerequisites in the same diagnostic session. Missing/failed prerequisite
produces UNKNOWN for the dependent test; it does not label a decoder broken. A
cancelled session does not publish stale callbacks. Actual progressing playback is
required by the existing player probe, and the advanced planner also requires at
least 500ms reported advancement without a stall.

The planner requires the current suite/fingerprint and a `testedAt` Unix timestamp
less than seven days old (no more than five minutes ahead of gateway time). Clock
skew produces conservative fallback, not an invented PASS. Known matching SDR
profile/level, pixel format, frame-rate metadata and dimensions are mandatory:

| Path | Envelope |
|---|---|
| H.264 High | UHD3840x2160, <=30fps, level<=5.1, <=12 Mbps |
| HEVC Main | MP4/hvc1, <=1080p30 level<=4.0 <=4 Mbps; UHD30 level<=5.0 <=12 Mbps |

Main10/HDR, 60fps, missing metadata, unsupported tags/containers and out-of-envelope
media keep the existing conversion/external fallback. No global HEVC MIME approval
is inferred from one sample. Legacy H.264 policy remains available without new
inventory fields. These limits are measured-sample policy, not native display/HDMI,
sustained decoder, bitrate-spike or Cast encoder certification. Extended remux/track
coverage, output inventory, overrides and physical validation remain open.
