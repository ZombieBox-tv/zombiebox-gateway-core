# Processed artwork cache

The gateway fetches provider artwork using its own credentials, accepts bounded
JPEG/PNG inputs, resizes without stretching/upscaling and encodes JPEG at quality
75. Only authenticated local `/v1/artwork/{id}` URLs reach the client. No provider
posters, thumbnails or Hero backgrounds are embedded in either APK.

Two concurrent image jobs, a five-second request deadline, a four-MiB input limit,
four-million-pixel decode limit and 256-KiB output limit bound the pipeline.
Concurrent requests for the same derivative share the in-flight result. An
optional image failure leaves the client's solid placeholder usable.

## Persistence and eviction

- Memory: 32 processed images, at most eight MiB of encoded payload, least recently
  used eviction. Disk: 64 MiB by default and at most 1,024 entries, also evicted by
  last access. Filesystem metadata/block overhead is additional to the byte budget.
- Absolute freshness: 24 hours from processing, including across restarts. Access
  does not renew expiry. Reads discard expired files; startup and writes prune
  expired/corrupt files and enforce the disk budget. There is no idle cleanup daemon.
- Keys hash the source URL, all provider headers, semantic image revision, profile
  and pipeline version. Credential/revision/profile changes cannot reuse an old
  derivative. Files contain an expiry header and processed JPEG, never source URLs,
  credentials or original images. Private content can remain until expiry/eviction;
  changing credentials prevents reuse but does not immediately erase old files.
- The dedicated directory is mode 0700 and files are 0600. Writes use temporary
  files and atomic rename. One gateway process owns each cache directory. Cache I/O
  failure degrades to memory; it does not prevent gateway startup or image delivery.
- Authenticated responses use private five-minute HTTP caching, an ETag and
  conditional 304 responses. Authorization is required even for cache hits.

`-artwork-cache` overrides the directory; otherwise it is `artwork` beside the
SQLite state file. `-artwork-cache-mb` sets the disk limit (0 disables persistence,
maximum 512). Full defaults to persistent `/data/artwork`; Edge uses the directory
beside its configured SQLite database. The shared Go implementation is identical.
Stop the gateway before clearing its dedicated cache directory for a full purge.

## Device and layout budgets

The server combines registered memory/display information with the requested role
(`size=hero`, `size=poster`, or the default landscape thumbnail). Unknown devices,
physical memory at most 768 MiB, memory class at most 96 MiB, or displays at most
960 pixels wide use the conservative tier. Arbitrary client dimensions are ignored.

| Role | Conservative maximum | Standard maximum |
| --- | --- | --- |
| Landscape thumbnail | 240 × 135 | 320 × 180 |
| Poster | 180 × 270 | 320 × 480 |
| Hero | 640 × 360 | 960 × 540 |

These are bounding boxes, not exact stretched outputs. Views crop for their layout.
The current Hero budgets are deliberately below the spec's larger 720/1080 asset
targets until real-device decoding/memory evidence supports increasing them.
The original aspect ratio is preserved. JPEG/PNG support does not imply WebP/AVIF.

Android additionally keeps a two-MiB encoded cache on low-memory devices and four
MiB otherwise, expiring after five minutes. It survives Home rerenders but clears
when the gateway/session changes or its Activity closes. Views own sampled RGB565
bitmaps. A shared decoded-bitmap pool and physical scroll/memory validation remain
separate work; the gateway cache does not prove those goals.

Automated coverage includes derivative bounds, credential isolation, restart reuse,
expiry/corruption, disk eviction, and Android cache navigation/expiry/reset behavior.
