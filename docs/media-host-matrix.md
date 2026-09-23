# Host media and receiver evidence

This matrix names exercised source/output combinations. A passing Fedora
FFmpeg/Go fixture does not establish Android decoder, iOS sender, network or
account compatibility.

| Case | Host evidence | Still unverified |
|---|---|---|
| Local H.264/AAC MP4 480p, H.264/AC3 MKV 720p, H.264/AAC MPEG-TS 1080p | Generated golden samples pass FFprobe through the bounded media adapter. Separate MKV remux/transcode test checks video packet preservation and compatible H.264/AAC output. | Actual TV decoder/output/AC3 behavior at each resolution. |
| Clear HLS TS/fMP4 and DASH template/static-list | Authenticated manifests and media segments convert to H.264/AAC outputs in REMUX and TRANSCODE modes. | Live upstream instability, DRM and device playback. |
| HLS and DASH alternate audio | English default and selected Spanish tracks survive remux/transcode and decoded output checks. | Player timing, language selection and switching on device. |
| Live MPEG-TS audio/video | Continuous conversion host fixture retains both streams; planning rejects seek for live. | Actual IPTV feed churn and transport latency. |
| SRT/VTT/ASS | Bounded parser, local sidecars, external text attachment and ASS simplification fixtures. | Subtitle overlay timing/readability on TV. |
| Spotify reception | Authenticated audio proxy, owned controls, paused metadata update, natural-end ticket revocation. | Real Connect account, device audio output and long-running recovery. |
| AirPlay reception | Authenticated HLS video→audio→idle replacement, segment proxy and stale-ticket revocation. | Real iOS discovery, audio/video, sync and handoff. |
| YouTube receiver and adaptive media | Leased TV Code command ownership and host mux/selection fixtures. | Real phone discovery, account restrictions, stream availability and TV playback. |
| YouTube account lists | Device-flow fixtures cover private refresh-token storage, expiry refresh, early HTTP 401 refresh with one retry, revoked grants and scoped subscriptions/playlists. | Real Google consent, quotas, account catalog contents and TV navigation. |
| RTSP | Bounded protocol OPTIONS endpoint diagnostic and Cast relay configuration. | Actual RTSP media exchange and target decoder; no full RTSP golden stream is claimed. |

The required physical test stage should collect provider/source, actual codec,
resolution, network path, selected planner mode, decoder probe result, media
timeline, audio/subtitle selection, failure/retry path and runtime memory. That
record belongs outside this source repository until the author runs the tests.
