# Provider browsing and remote media (dev.11)

The injected `catalog.Backend` and `catalog.Browser` own navigation/source retention;
HTTP handlers validate requests and configuration revisions. Provider adapters own
Plex XML, Jellyfin JSON and Stremio catalog/meta/stream translation. No provider URL,
credential or upstream node path is sent to the client. Full/Edge share this core.

`GET /v1/browse?provider=plex|jellyfin|stremio&parent=<browseId>&offset=0&q=...`
returns `BrowsePage`: title, at most 40 semantic items and the next offset (-1 ends).
Folder IDs are opaque, device-scoped, revision-checked and expire after 30 minutes;
4096 retained sources bound cache entries. Invalid/expired parents return 410.
Configuration changes invalidate old sources. Playback and artwork resolve issued
browse items through the same device-scoped cache.

- Plex: libraries, section items and metadata children (series/seasons/episodes).
- Jellyfin: user views and parent-scoped items, upstream offsets and search.
- Stremio: manifest catalogs, 100-item upstream skip windows exposed as <=40-item
  client pages, metadata seasons and ordered episode video IDs. Catalog search
  requires the addon's declared `search` extra. Root/season/episode search filters
  the corresponding names; it is not an all-addon search. HTTP streams only.

`RemoteMedia` is a consumer-owned server interface; `RemoteTools` receives the
shared bounded FFmpeg tools and streaming HTTP transport in `cmd/zombied`.
Progressive HTTP inputs are probed through an ephemeral random-ticket loopback
bridge. FFmpeg arguments contain only loopback URLs, not provider origins/tokens.
Each input retains its own headers; Go owns redirects, Range, idle deadlines and
cancellation. Restricted demuxers exclude manifests/concat and nested fetches.
Probes share two slots; conversions share one slot with local tracks/conversion.
A cancelled stream terminates its subprocess and upstream requests.

The detected container takes precedence over a provider's generic MIME label.
AUTO can remux compatible codecs or transcode to the existing Baseline/AAC profile.
Unknown network/probe failure is not decoder failure; ordinary progressive input
may fall back to direct relay, but separate A/V never becomes video-only playback.
HLS/DASH/live conversion, remote track selectors, bitrate adaptation and automated
hardware acceleration remain open. Converted output is fragmented MP4 and is not
seekable; transcode resume carries the existing timeline offset contract.

YouTube prefers combined H.264/AAC; otherwise its private worker resolves bounded
360p/480p/720p H.264 plus audio-only AAC URLs. Both CDN URLs are independently
validated and muxed by the core; no second URL reaches the Android protocol.
Known failed fragmented-MP4 probes reject automatic mux. There is no claim of
account/cipher availability, live YouTube A/V success or legacy decoder support.

Remaining navigation work includes provider account/library selection, richer
metadata/actions, playlists/channels, upstream progress writeback, persistent deep
item resolution after cache expiry/restart, retry/session integration and full EPG.
Existing Home summaries are not replaced by the browse hierarchy in this increment.

Reference protocols: [Plex PMS](https://developer.plex.tv/pms/),
[Stremio addon protocol](https://stremio.github.io/stremio-addon-sdk/protocol.html).
Pinned local upstream SDKs remain references, not product runtime dependencies.
