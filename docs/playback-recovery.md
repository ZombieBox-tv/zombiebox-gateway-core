# Playback retry and continuous live adaptation

Dev.13 adds optional `positionMs` to the playback request. A supplied integer in
0–604800000 overrides stored history, including explicit zero and previously ended
items. Live plans ignore stored/requested VOD positions. Transcoded plans carry
the existing timeline offset, so the client reports positions on the source clock.
Credential resolution stays on the gateway; each retry gets a fresh owned ticket.

The remote media adapter now accepts continuous HTTP MPEG-TS marked live. Auto
retains DIRECT_PLAY; an explicit REMUX/TRANSCODE retry can convert through the
existing private loopback input. Output is fragmented MP4, live and non-seekable.
FFmpeg never receives provider URLs, credentials, arbitrary nested input paths or
additional protocol privileges. TS remux probes the first audio stream and applies
AAC ADTS→ASC only to AAC; other codecs do not receive that filter.

Replacement conversion waits at most two seconds for the previous process to
release the single conversion slot. Cancellation still stops/reaps the child;
waiting does not add concurrent conversion capacity. Existing probe, duration,
redirect and write limits remain in force.

HLS/DASH **conversion** remains unsupported: those manifests need a separately
bounded resource graph. Existing HLS relay remains available. A healthy worker,
synthetic TS conversion or an MP4 probe does not demonstrate Android decoder
compatibility, long-duration live behavior or A/V synchronization.

References: [FFmpeg AAC bitstream filter](https://www.ffmpeg.org/ffmpeg-bitstream-filters.html#aac_005fadtstoasc),
[fragmented MP4](https://ffmpeg.org/ffmpeg-formats.html#mov_002c-mp4_002c-ismv).
