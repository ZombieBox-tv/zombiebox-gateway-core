# zombiebox-gateway-core: component work

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
