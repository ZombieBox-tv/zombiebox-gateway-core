# Clear HLS/DASH media adaptation (dev.14)

Declared HLS (`.m3u8`/MPEGURL MIME) and DASH (`.mpd`/DASH MIME) may enter the same
REMUX/TRANSCODE pipeline as progressive input. The planner probes codecs and
chooses fragmented MP4 remux or bounded H.264/AAC conversion. Known fragmented-MP4
failure retains external fallback. Live TS Auto remains direct; explicit direct
HLS retains the existing client relay. Probe/network failures are not decoder
failure evidence. Live conversion rejects seeking; VOD transcode accepts position.

`internal/media/manifest` owns parsing, URI resolution and a per-operation
loopback server. `RemoteTools` chooses progressive or manifest input; `Tools` owns
bounded process execution. Both HTTP and process dependencies remain injected.

Supported input: clear HLS media/master playlists, TS or fragmented MP4 segments,
initialization maps and byte ranges; DASH inherited BaseURL, SegmentTemplate
(Number/Time, RepresentationID/Bandwidth), SegmentTimeline and static SegmentList.
The adapter refreshes manifests rather than caching their contents. All media
URLs exposed to FFmpeg identify registered resources. Original-origin headers
stay in Go; cross-origin resources do not inherit them. Existing redirect policy
still applies. Processes cannot open local files or arbitrary protocols.

Bounds: 1 MiB manifest, 16 KiB HLS lines, 10,000 XML nodes/depth32, graph depth8,
2,048 resources with segment eviction, eight simultaneous fetches, 30-second
per-resource timeout, 128 MiB segment limit, two probes and one conversion job.
Cancellation closes upstream fetches and the loopback server and reaps FFmpeg.

Unsupported: encrypted/DRM content, XML external references, content steering,
DASH multiple BaseURLs and dynamic SegmentList. Fail explicitly instead of
passing an unrewritten manifest through the process boundary. Dynamic DASH
SegmentTemplate remains available; live source seeking is not introduced.

Evidence: synthetic A/V fixtures cover HLS TS/fMP4 and DASH template/static list,
each with remux and transcode; proxy tests cover cancellation, cross-origin
credentials, byte ranges, refresh and rejected external references. Full runs the
media test binary with packaged FFmpeg and external networking disabled. Physical
players and Termux/Bionic remain unverified. This is not adaptive bitrate selection
or complete remote track/subtitle support.
