# Language policy, text subtitles and state operations (dev.19)

Full and Edge use the same injected media implementation. `PUT /v1/device/preferences`
accepts up to eight ordered `audioLanguages` and `subtitleLanguages` values and
`subtitleMode` (`off`, `forced`, `auto`, `always`). English/Spanish defaults remain.
ISO 639 aliases and region variants match their base language. Auto uses full
subtitles when selected audio is outside preferred languages, otherwise only forced
tracks. Default dispositions break language ties; unsupported bitmap tracks stay
unselectable. Explicit DIRECT_PLAY/EXTERNAL_PLAYER policy bypasses automatic mapping.

AUTO probing selects audio and optional text subtitles. Non-first audio forces at
least REMUX; a measured fragmented-MP4 failure retains external fallback. Resuming
converted content uses TRANSCODE with a timeline offset instead of silently losing
position. Manual audio switching still creates a replacement owned session.

Local sidecars must be regular, non-symlink siblings named `Movie.srt`,
`Movie.es.srt`, `Movie.en.forced.ass`, etc. Discovery scans at most 4,096 directory
entries and exposes at most 16 sidecars of at most 2 MiB each. Opaque file-name IDs
survive directory reordering; foreign paths are never accepted. Embedded remote
text tracks use the credential-safe loopback bridge and bounded FFmpeg extraction.
Live streams, split YouTube inputs, bitmap burn-in and provider attachment URLs
are outside this increment. The client receives normalized cues, never upstream
URLs or FFmpeg selectors.

## IPTV

`iptv.epgMappings` maps a gateway item ID, original TVG ID or exact channel title
(in that priority order) to an XMLTV channel ID. Maximum 256 nonempty mappings,
200 characters per key/value. Example server configuration:

```json
{"iptv":{"enabled":true,"url":"https://example.org/list.m3u","epgUrl":"https://example.org/guide.xml","epgMappings":{"news-original":"news.xmltv"}}}
```

The client configuration accepts one `source = XMLTV ID` mapping per line. A
nonempty edit replaces the mapping set; blank preserves it. Server configuration
or an API patch with `epgMappings: {}` clears all mappings. Channel IDs do not change.
Retention is 48 hours/96 programmes per channel, bounded by 20,000 input programmes
and four cached feeds. Existing five-minute freshness/30-minute stale limits apply.

## Diagnostics and SQLite

`GET /v1/diagnostics` requires the paired device token and uses `Cache-Control:
no-store`. The allowlist includes platform/resources, probe results, module states,
owned active playback modes and up to 20 failed progress entries from the bounded
stored history. No fresh probe is run. URLs, tokens, installation IDs, device
fingerprints, media titles and session tickets are excluded. This is a diagnostic
snapshot, not account or device acceptance.

SQLite schema version is `PRAGMA user_version=1`. Startup atomically adopts legacy
version-zero records without rewriting their values. Unknown newer versions are
refused before application writes. Back up the stopped private state before future
format-changing upgrades; no automatic destructive downgrade is supported.
