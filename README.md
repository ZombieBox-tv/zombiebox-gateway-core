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
