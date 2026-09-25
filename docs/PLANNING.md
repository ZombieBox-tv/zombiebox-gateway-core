# zombiebox-gateway-core: component work

## dev.63 live audio delivery and Spotify refusal containment

Fragment audio-only live AAC into bounded one-second fMP4 units and flush each
gateway stream write so the selected Client receives headers and media promptly.
The real-FFmpeg host integration measures first-byte delivery and verifies AAC
output; the selected Vizio still needs a fresh Apple Music sound/timeline test.
Keep the video conversion path unchanged. Spotify now records stop outcomes and
reissues a bounded stop when its pinned daemon refuses audio keys across Connect
reconnections. It reports unplayable state instead of false playback; it does not
resolve the upstream key refusal or establish sound. No product gate closes.

## dev.61 capability-aware media and receiver triage

Prefer native MPEG-TS HLS only when the current device's functional HLS and
matching codec probes passed; keep Gateway remux/transcode as the fallback.
Preserve a verified lower YouTube rendition while checking higher H.264/AAC
sources, and prevent a late superseded playback failure from clearing the
current manual-quality choice. Distinguish unconfigured IPTV from an unavailable
provider. AirPlay audio readiness now requires playable HLS segments and its
dedicated RTP conversion normalizes timestamps from decoded samples. Private
Spotify worker health classifies daemon errors without exposing account data;
the missing Premium track/audio remains under investigation. Host/race tests
pass. Native HLS, AirPlay, Spotify, IPTV and quality switching still require
separate physical or provider evidence; no product milestone closes.

## dev.60 playback evidence and receiver diagnostics

Expose the gateway's probe time and an authenticated Client version refresh so
new media measurements on devices with wrong clocks retain valid age without
reusing a prior APK's evidence. Reject undated PASS results for manual quality;
let a fresh 720p PASS outrank a low-memory hint while preserving source, output,
transport and audio gates. Return a retryable failure when the single YouTube
worker is busy rather than presenting a false empty related rail. The Spotify
bridge publishes stream headers before its first PCM frame and exposes only
bounded audio-flow counters through authenticated health for supervised QA.
Physical manual quality, related relevance and Spotify sound still require
device/phone tests; no product milestone closes.

## dev.57 YouTube physical-QA and Spotify readiness corrections

The anonymous YouTube tab and browse root request a bounded real exploration
shelf directly, avoiding an empty home call before the fallback. The resolver's
parent and worker memory budgets agree, and private video/audio resolution has
a longer bounded deadline. YouTube resolution retries only the single-flight
worker's transient 503 busy response within three seconds. For progressive MP4
without a declared byte length, the worker derives a bounded total from a finite
206 response and verifies head, middle and tail; the gateway then relays finite
ranges as a seekable direct stream with Content-Length and Content-Range. Signed
media origins remain private.

In the September 24 QA7 trial, the paired Vizio API 13 Client dev.50 opened the
YouTube hero video in its fullscreen player. The on-screen timeline advanced
from 0:06 to 2:28 of 3:35. A sanitized gateway capture recorded playback plan
HTTP 201, two stream GETs with HTTP 206, and a progress update; there was no
playback 502 in that attempt. ADB screenshots show a black SurfaceView area, so
visible decoded frames and audible sound have not been independently confirmed.
This one video and device do not establish general YouTube playback compatibility.
Real DIAL/TV-code handoff still needs a separate physical retest. No product
milestone closes.

The Spotify worker now reports its selected Zeroconf or device-authorization
mode and checks account session state before the gateway marks it ready. Its
bearer-only API may bind to the Docker host-gateway interface for host-network
mDNS discovery; the local source-built worker was probed there without exposing
that API on the physical LAN. The Full package owns mode selection and preserved
credentials. A phone selecting the receiver and producing audible playback still
need physical verification.

## dev.50 IPTV categories

Normalize bounded M3U `group-title` and `EXTGRP` labels without changing channel
IDs. Catalog pages expose sorted categories and combinable category/favorite/search
filters. Host parser and catalog tests pass; user-playlist/EPG acceptance remains.

## dev.49 IPTV favorites

Persist up to 256 stable channel IDs in SQLite; only current-playlist channels
appear in favorite pages. Add a semantic item flag and authenticated actions
without copying stream URLs/credentials. Host tests cover filtering, restart and
removal. Other media/receiver cases and YouTube OAuth remain open.

The product milestones relevant to this repository are M0, M1, M3, M4, M5, M6, M7, M8, M9, M11.
The local registry is a component projection of the workspace plan. Closing a
component task does not close a product-wide milestone or a physical validation gate.

Current increment: independent repository/build/dependency boundaries with filtered
history. Remaining feature development follows the ordered workspace audit:
tracks/subtitles and lifecycle; provider navigation/virtualization; measured
capabilities/native-first health; remote media adaptation; receiver finishing;
Edge operations and reproducible releases. Implement only this component's part,
and evolve shared protocol contracts in their owning repository.

Keep a separate validation track for hardware/account/latency/memory evidence.
Use development checkpoint tags until complete exit gates are evidenced. Hosted
issues/milestones can be attached to the shared GitHub Project once remotes exist.

## dev.48 — AirPlay A/V receiver transition host coverage

A simulated authenticated AirPlay worker alternates video, audio-only metadata and
idle. The server must proxy each HLS manifest/segment through the gateway, replace
the owned plan when media kind changes, revoke the old stream ticket, and release
the audio ticket when the sender stops. This is host behavioral evidence only;
iOS/UxPlay, device playback and real-account validation remain open. The licensed
Full/Edge artifacts consume earlier Core source identities and are not relabeled
by this test-only checkpoint.

## dev.47 — licensed Spotify decoder staging

The exact go-librespot source now has a first-party, reproducible patch replacing
the unlicensed `xlab/vorbis-go` binding with pinned MIT `oggvorbis` and `vorbis`
modules. The source preparation refuses changed upstream checkouts, and the
Spotify Docker recipe requires the patch marker, includes both MIT license files
and omits the old binding. A stereo synthetic fixture checks Ogg metadata CRC,
gain, decoded samples, seek and closed-state behavior; the modified Full image
builds and its CLI starts on the host. Real Spotify account playback, Android/
Bionic performance and new binary/source publication remain open. The frozen Full
dev.46 optional Spotify image is not relabeled by this source change. No product
or physical gate closes.

## dev.11 increment

Hierarchical Plex/Jellyfin/Stremio browsing, progressive remote probing/conversion and paired YouTube adaptive mux; bounded injected adapters. Live/HLS adaptation and receiver completion remain open.
No product milestone or physical/account gate is completed by this checkpoint.

## dev.12 increment

Selected-client Spotify/AirPlay leases, fresh activity routing, owned controls and adaptive Cast encoder budgets. Account/OEM/physical gates remain open.

## dev.13 increment

Explicit retry positions and opt-in continuous live MPEG-TS adaptation; bounded conversion replacement. HLS/DASH conversion remains open.
No product milestone or physical/account gate closes with this checkpoint.

## dev.14 increment

Bounded clear HLS/DASH manifest adaptation, authenticated segment graph, remux/transcode and planner integration shared by Full/Edge.
The four requested block-1 changes are implemented; physical acceptance and broader product gates remain open.

## dev.16 increment

Revisioned operation/HLS probes, bounded persistent browse locators, YouTube hierarchy, stable IPTV IDs, receiver metadata/artwork and browser egress/pointer/recovery.
Product exit gates and physical/account acceptance remain open.

## dev.17 increment

Stable Home Hero, scoped deep-item history, bounded guide cache, receiver exclusion and native-inventory fields; all container recipes include GPL notices.

No product milestone or physical gate is closed.

## dev.18 increment

Dev.18: bounded persistent artwork derivatives, restart reuse, private cache keys, device/layout profiles and conditional HTTP caching. No physical milestone closes.

## dev.21 increment

Measured LAN bitrate planning, bounded federated provider search with Plex preview resolution and Stremio search catalogs, and read-only state inspection/consistent snapshot staging.
No physical, account or product milestone closes.

## dev.22 increment

Device-scoped receiver replacement, readiness-gated Cast handoff with target consent, revoked YouTube command/source fencing and receiver-bound playback resolution.
No product or physical acceptance gate closes.

Verification: Go vet/race and 49 contract fixtures pass. Regression coverage includes failed replacement, readiness/consent rechecks, retired streams, and late YouTube poll/resolution rejection.

## dev.23 increment

Bounded credential-free UDP discovery, discovery-only CLI and pinned capture research references.
Product exit gates and deferred physical acceptance remain open.


## dev.24 increment

Target-scoped QR/code consent, durable grants, proof-before-token reconnection, bounded remote commands/receipts and scoped Cast authorization/revocation. QR rendering uses pinned MIT Go code; optional services remain independent.
Full visual/capture policy, extended Remote, HEVC/4K and other product gates remain open; physical acceptance stays deferred.

## dev.25 increment

Bounded HEVC Main and H.264 UHD30 playback planning, advertised extended probes, fresh advancing evidence and synthetic SDR fixtures. Shared by Full/Edge; physical acceptance remains open.

## dev.27 increment

Opt-in 1080p Cast grants require fresh advancing receiver H.264/HLS probes. Older senders retain their original ceiling; companion negotiation remains bound to the approved TV. Full and Edge share this implementation. Physical runtime and receiver/OEM gates remain open.

## dev.29 increment

Explicit SCREEN/AUDIO Cast negotiation, AAC/HLS receiver policy, mode-aware audio plans and unchanged consent/lease/revocation boundaries. Shared by Full and Edge.
Product milestones and physical acceptance remain open.

## dev.30 increment

Bounded companion media uploads, private temporary storage, actual stream probing/planning, consented VOD handoff and cleanup. Shared by Full and Edge; no MediaMTX dependency for files.
Product milestones and deferred physical gates remain open.

## dev.31 increment

Bounded declarative decoder/encoder profiles and display modes, strict inventory validation and privacy-scoped diagnostic export shared by Full/Edge. Inventory does not create successful probes or select an OEM backend.
Product milestone and physical/public distribution gates remain open.

## dev.32 increment

Phone Media now admits bounded AVI, FLV and ASF/WMV containers. Real host upload/probe/transcode tests produce H.264/AAC and revoke finished streams; this is not physical Android codec acceptance.
Product milestones, physical validation and public distribution remain open.

## dev.34 implementation checkpoint

Five-minute single-use QR consent, online target selection with local six-digit approval, opt-in 24-hour request suppression and focus-scoped remote text. Shared by Full/Edge; no deployment or physical acceptance.
Product exit gates and deferred physical acceptance remain open.

## dev.35 checkpoint

Bounded public URL queues, continuous measured downgrade planning, universal receiver listening with private-worker epoch fencing, and first hosted source configuration.
Product milestone completion still requires its recorded acceptance gates.


## dev.37 implementation checkpoint

Provider text subtitle attachments, bounded extraction and language planning; idempotent YouTube natural queue completion. Full/Edge share behavior; product and physical gates remain open.

## dev.39 navigation increment

Dev.39: server-owned Stremio search catalog continuations retain query scope and reach upstream skip pages. Wider provider navigation and other V1 packages remain open.

## dev.40 reception and diagnostics increment

Explicit bounded HTTP/UDP/RTSP endpoint diagnostics with injected dialing, cancellation and credential-free reports. Reachability never grants media/account capabilities. Product milestones remain open.

## dev.41 guide and audio selection increment

Capability-aware explicit audio replacement: selected-stream policy, remux at zero, accurate resumed conversion, failed-output/absent-adapter rejection and preservation of the old session. See docs/audio-selection.md. Product and physical gates remain open.

## dev.42 navigation and preferred-audio increment

Dev.42: automatic VOD planning evaluates the preferred audio before codec policy, maps incompatible alternate tracks out through remux and preserves source inventory, failures and explicit modes. Local/remote/HLS/DASH planner fixtures and real selected-output coverage; no physical or product gate closes.

## dev.43 navigation and functional media increment

Dev.43: bounded local software pipeline diagnostic generates, remuxes, transcodes and decodes synthetic audio/video. Missing tools/cancellation remain distinct; no receiver/network capability is granted. Full/Edge share the same implementation.

## dev.44 authenticated HLS audio coverage

A real FFmpeg fixture covers an authenticated HLS master playlist with English and
Spanish audio renditions. It verifies default English playback and explicit Spanish
selection through both remux and transcode by decoding the resulting audio. This
adds host media evidence without changing runtime policy or claiming device playback.
The remaining media/receiver combinations and physical gates stay open.

## dev.45 — Edge Node runtime compatibility

The YouTube catalog and TV receiver wrappers now admit Node 24.18+ LTS in addition
to secure Node 22.22.2+. Both locked npm installs contain only portable JS/WASM,
and all 14 wrapper tests pass under Node 24.18.0 on Linux. The Full dev.46 image
continues to use its frozen Node 22.22.2 digest. This expands the candidate for
Termux's official prebuilt Node LTS; actual Android execution and the Edge module
bundle remain separate gates.
## dev.51 YouTube account implementation

The shared Core adds a gateway-owned Google device authorization and read-only
subscriptions/playlists route, private refresh/revoke state, bounded device-scoped
browse roots and host tests. TV Code remains independent. Real Google credentials,
account consent, quotas and Android/TV acceptance are not verified; no product
milestone closes. See [account boundary](youtube-account.md).
The same checkpoint adds a Spotify metadata/pause/natural-end receiver fixture:
its active stream survives a paused track change, then revokes the ticket on
natural completion. Real Spotify Connect and output remain device/account gates.
## dev.52 local golden media profiles

Generate/probe representative 480p H.264/AAC MP4, 720p H.264/AC3 MKV and
1080p H.264/AAC MPEG-TS host fixtures. The existing HLS/DASH, live conversion,
subtitle, Spotify, AirPlay and YouTube host cases are catalogued in
[the media matrix](media-host-matrix.md). Real RTSP media, account/sender/device
behavior and product exit gates remain open.

## dev.55 paired AirPlay PIN retrieval

The private AirPlay worker exposes its configured four-digit receiver PIN only
through its token-protected `/pairing` route. Core validates that response and
serves it to an authenticated, paired Client through `/v1/airplay/pairing` with
`Cache-Control: no-store`; it never appears in generic status or provider
configuration. The operator code is separate. Host tests cover anonymous
denial, disabled service, private worker authentication and paired retrieval.
An updated Full/Edge worker and a physical iPad PIN/playback trial are still
required; this does not close M6 or any product/physical gate.

## dev.56 YouTube exploration shelf

When the anonymous YouTube home feed is empty, the scoped TV Home route falls
back to real YouTube browse results for a broad `popular` query. It retains
device-scoped playback sources and caches the shelf for two minutes; a worker
failure leaves Home available without invented media. The Vizio's YouTube
tab, thumbnail art, playback and response time still need physical verification
with an updated Full candidate.

## dev.64 live AAC remux candidate

Audio-only live HLS AAC uses progressive ADTS for the Gateway receiver stream.
Host tests cover first-byte output, decoding, cancellation and the paired
receiver plan's MIME. The Vizio AirPlay sender/player and timeline remain
unverified after this change; Spotify audio-key refusal remains unresolved.
