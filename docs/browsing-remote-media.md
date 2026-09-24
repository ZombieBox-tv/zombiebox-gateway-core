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

## dev.39 Stremio search continuation

A searched addon catalog with additional results contributes a folder to the
aggregate search preview. Opening it begins that catalog's matching results at
page zero and follows the existing forty-item pages / upstream `skip` boundary.
The original query is stored in the server-owned opaque browse locator; the client
still receives semantic IDs, never addon paths or query syntax. A nonempty explicit
search in that folder overrides the original query. Exhausted catalogs do not
advertise a continuation. Search fan-out remains capped at four advertised search
catalogs, and the existing six-second federated-search timeout still applies.

Provider fixtures verify special-character queries, page offsets 0/40/80/100,
termination, explicit scope replacement and exhausted catalogs. This does not add
unlimited addon aggregation or claim real-account/device acceptance.

## dev.40 Relevant YouTube feed and related pagination

The YouTube fullscreen player rail requests bounded, semantic related items keyed
to the currently playing video via:

`GET /v1/youtube/related?video=<videoId>&cursor=<cursor>&offset=<offset>&limit=<limit>`
(or path format `GET /v1/youtube/related/{video}`).

The response follows the `RelatedPage` domain model:
- `apiVersion`: Integer protocol version (currently `1`).
- `videoId`: The 11-character seed video ID.
- `currentVideo`: Metadata of the playing video (`id`, `title`, `subtitle`, `description`,
  `durationMs`, `playable`, `artworkUrl`) if resolved, omitted if unavailable.
- `items`: Bounded array of semantic related video items (max 40 items per page;
  default page size is 20).
- `nextCursor`: Opaque base64url token for retrieving subsequent pages.
- `hasMore`: Boolean indicating if further pages are available.
- `total`: Total count of related items returned in the current response.

The seed video is explicitly excluded from `items`, and duplicate items are deduplicated
by stable `youtube-<id>` identifier across pages.

Related query cursors encode the target video ID, offset, and query context. Cursors are
length-bounded (<= 512 bytes), verified against the query video ID, and offsets are clamped
between 0 and 360 to prevent worker memory exhaustion. Bad or mismatched cursors return
HTTP 400. Inactive or unconfigured YouTube returns HTTP 409. An empty response is returned
with `items: []`, `nextCursor: ""`, and `hasMore: false`.

Repeated requests are cached in memory for up to 2 minutes, bounded to 64 active entries.
All upstream requests carry an 8-second timeout and observe client request cancellation.

### Home YouTube Feed Hierarchy

The Home YouTube section selects content following a strict 3-tier hierarchy:
1. **Signed-in profile feed**: Subscriptions and playlists retrieved via OAuth Data API
   when an account is connected.
2. **Recent search/view context**: Device-scoped recent search query or viewed video
   context (preserved in memory, bounded to 64 devices with 15-minute TTL).
3. **Generic anonymous popular fallback**: Only queried if the prior sources are empty,
   unconfigured, or unavailable. Retries once if the initial upstream fetch is empty.

Privacy is preserved: when disconnected or unauthenticated, no user identity or account
presence is implied or manufactured. Feed caches are immediately invalidated upon
account connection or disconnection.
