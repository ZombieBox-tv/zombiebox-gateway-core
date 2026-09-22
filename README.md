# zombiebox-gateway-core

The only Go core and provider/process adapters used by Full and Edge.

This is an independent repository in the Zombie Box workspace.
[Source and milestones](https://github.com/ZombieBox-tv/zombiebox-gateway-core) are hosted on GitHub.
Development checkpoints are not stable releases or physical compatibility claims.

- `gateway/cmd`: executable composition roots.
- `gateway/internal`: HTTP transport, feature policy, injected adapters and SQLite.
- `wrappers`: pinned optional Node/media/browser integrations and worker Dockerfiles.
- `third_party`: reference lock/notices; ignored clones restored with `make references`.

```sh
make check build
make wrappers-check
make references  # only when upstream source references are needed
```

Full/Edge own deployment and configuration. Do not fork the Go core. Wrapper
Dockerfiles for Spotify/AirPlay/Threadfin consume the named BuildKit `sources`
context exported from locked upstream commits by Full. No process starts when
running the checks above. Go policy/adapters use manual constructor injection;
SQLite is the only V1 database. Media processing stays bounded and cancellable.

## Development rules

Run `make format` and `make format-check`. Formatters are pinned and downloaded
on first use. See [AGENTS.md](AGENTS.md), [history provenance](docs/history.md),
[component work](docs/PLANNING.md) and [local milestone registry](docs/milestones.json).
The central workspace owns product-wide ADRs, the original specification, the UI
reference, M0–M11 exit gates and the complete development/validation gap audit.
Physical devices over USB/ADB are the default; automated checks do not establish
legacy runtime or end-to-end account/media compatibility.

Dev.10 adds [local media tracks](docs/media-tracks.md): authenticated inventory,
legacy audio switching with timeline offsets and bounded text subtitles through
the injected FFmpeg adapter. Live/manifest adaptation and automatic language selection
remain development work.

Dev.11 adds [hierarchical browse and progressive remote adaptation](docs/browsing-remote-media.md),
including a paired adaptive YouTube resolver. Live/HLS conversion and complete
provider/account workflows remain open.

Dev.12 adds [selected-client Spotify/AirPlay reception](docs/media-receiver.md),
leased controls and receiver-aware Cast budgets. These share existing playback
sessions; account, OEM and physical A/V evidence remains separate.

Dev.13: Explicit retry positions and opt-in continuous live MPEG-TS adaptation; bounded conversion replacement. HLS/DASH conversion remains open.

## dev.14 increment

Bounded clear HLS/DASH manifest adaptation, authenticated segment graph, remux/transcode and planner integration shared by Full/Edge.
The four requested block-1 changes are implemented; physical acceptance and broader product gates remain open.

Dev.16: Revisioned operation/HLS probes, bounded persistent browse locators, YouTube hierarchy, stable IPTV IDs, receiver metadata/artwork and browser egress/pointer/recovery.

## License

First-party code: [GPL-3.0-only](LICENSE). See [NOTICE](NOTICE) for third-party scope.

Dev.18 adds [persistent artwork caching](docs/artwork-cache.md), shared by Full and Edge.

Dev.19: Automatic language/track policy, remote text tracks, local sidecars, 48-hour mapped EPG, allowlisted diagnostics and atomic SQLite schema versioning.

Dev.20: Automatic Spotify/AirPlay handoff with confirmed-activity selection and replacement preservation; fixed low-bandwidth conversion profile.

Dev.21: Measured LAN bitrate planning, bounded federated provider search with Plex preview resolution and Stremio search catalogs, and read-only state inspection/consistent snapshot staging.

## dev.22 increment

Device-scoped receiver replacement, readiness-gated Cast handoff with target consent, revoked YouTube command/source fencing and receiver-bound playback resolution.
No product or physical acceptance gate closes.

## dev.23 increment

Bounded credential-free UDP discovery and optional discovery-only process; locked scrcpy/sndcpy research references.
No product or physical acceptance gate closes.


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

## dev.34 increment

Five-minute single-use QR consent, online target selection with local six-digit approval, opt-in 24-hour request suppression and focus-scoped remote text. Shared by Full/Edge; no deployment or physical acceptance.

## dev.35 increment

Bounded public URL queues, continuous measured downgrade planning, universal receiver listening with private-worker epoch fencing, and first hosted source configuration.


## dev.37 increment

[Provider subtitle attachments](docs/provider-subtitles.md) use owned semantic tracks and bounded plain-text extraction. The YouTube worker advances natural queue completion once, with receiver-epoch fencing. Product milestones and physical acceptance remain open.

## dev.39 navigation increment

Dev.39: server-owned Stremio search catalog continuations retain query scope and reach upstream skip pages. Wider provider navigation and other V1 packages remain open.

## dev.40 reception and diagnostics increment

Explicit bounded HTTP/UDP/RTSP endpoint diagnostics with injected dialing, cancellation and credential-free reports. Reachability never grants media/account capabilities. Product milestones remain open.

[Endpoint diagnostic usage and limits](docs/endpoint-diagnostics.md).

## dev.41 guide and audio selection increment

Capability-aware explicit audio replacement: selected-stream policy, remux at zero, accurate resumed conversion, failed-output/absent-adapter rejection and preservation of the old session. See docs/audio-selection.md. Product and physical gates remain open.

Dev.42: automatic VOD planning evaluates the preferred audio before codec policy, maps incompatible alternate tracks out through remux and preserves source inventory, failures and explicit modes. Local/remote/HLS/DASH planner fixtures and real selected-output coverage; no physical or product gate closes.
