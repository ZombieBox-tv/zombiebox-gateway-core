# Provider subtitle attachments

The gateway owns attachment URLs and provider headers. Clients receive the existing
semantic track inventory and bounded plain-text cues; no new protocol DTO or
client library is required. Full and Edge use the same implementation.

| Source | Attachment support |
|---|---|
| Plex | First selected media part's external text streams, including lazy metadata resolution and library browse. Credentialed URLs must stay on the configured server origin. |
| Jellyfin | Primary media source's external text streams, downloaded through its authenticated subtitle endpoint as SRT. Other editions are not mixed into the selected timeline. |
| Stremio HTTP stream | The selected stream's `subtitles` array; HTTP(S) SRT/VTT/ASS/SSA URLs, without provider credentials. Extensionless URLs are treated as SRT. Separate subtitle-addon discovery is not implemented. |
| Local/embedded | Existing embedded text tracks and same-basename local sidecars remain supported. |

At most 32 attachments are retained. IDs start at 1600000000, fit signed 32-bit
Android integers, and remain separate from FFmpeg and local-sidecar indexes.
Language/default/forced policy participates in Auto planning. Manual selection
uses the same owned, expiring playback session as embedded subtitles.

Each download has a 15-second timeout and 2-MiB limit, shares the bounded probe
pool, and inherits session cancellation. Redirects use the provider transport's
credential-stripping policy. SRT/VTT are normalized directly; ASS/SSA are downloaded
to private temporary files and converted by FFmpeg to plain cues. Provider URLs
and tokens never enter FFmpeg arguments. Styled/animated ASS is not preserved.
The response remains bounded to 1 MiB and 5,000 cues by the existing parser/API.

Live media, split YouTube inputs, bitmap subtitle burn-in and arbitrary subtitle
web pages are outside this attachment path. Missing probe/extraction evidence is
reported unavailable; it does not manufacture successful track support. Automatic
subtitle selection requires a successful Auto probe; explicitly requested direct
playback can still use the manual inventory when probing is available.

Host evidence includes provider fixtures, same-origin rejection, source limits,
cancelled/oversized downloads, real ASS-to-text FFmpeg conversion, automatic language
selection and cross-device API denial. Physical timing/rendering and real provider
accounts remain in the later acceptance track.
