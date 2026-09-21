# zombiebox-gateway-core

The only Go core and provider/process adapters used by Full and Edge.

This is an independent repository in the Zombie Box workspace. Remotes and hosted
releases are not configured yet; local commits/tags and dependency pins are real.

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
