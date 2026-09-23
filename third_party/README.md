# Upstream sources for adapters

`make references` restores sources/ to the full commits in upstreams.lock.json. These are shallow/partial clones, not submodules. Product Git tracks the lock and restore script, not upstream objects. FFmpeg uses a sparse documentation checkout to save space; other checkout scopes are recorded in the lock.

No upstream installers, builds or npm install commands are run. The restore script refuses modified worktrees or a different checked-out HEAD. To update a reference, review its URL/commit/license, change the lock and coordinate checkout explicitly; do not pull every upstream indiscriminately.

| Reference | Intended boundary |
|---|---|
| FFmpeg | ffprobe/remux/transcode processes |
| YouTube.js | YouTubeProvider behind a Node adapter |
| yt-cast-receiver | remote commands into PlaybackSession, no independent player |
| plexgo | PlexProvider SDK candidate |
| jellyfin.org | official API docs; generated Go OpenAPI client pending |
| stremio-addon-sdk | addon protocol reference |
| go-librespot | Spotify Connect process |
| oggvorbis / vorbis-go-decoder | Pinned MIT source for the licensed Spotify Vorbis replacement |
| UxPlay | AirPlay process |
| MediaMTX | live/mirroring relay |
| Threadfin | Full IPTV/EPG |
| Browservice | RebrowserProvider reference/candidate; upstream unmaintained |
| Serenity | Google TV D-pad, overscan and player reference; never runtime |

Rebrowser in the specification is our abstraction, not an external repository name. Do not confuse it with unrelated browser automation projects. The maintained Chromium/Puppeteer provider is selected in ADR 0021; Browservice remains reference-only.

Locked commits are reference snapshots, not approved runtime versions. Pin integrated dependencies in manifests/image digests as their wrappers are implemented. Review notices before copying code or distributing binaries.

Puppeteer Core and Playwright Docker sandbox documentation are pinned in the lock file. The adapted seccomp policy retains its Apache-2.0 notice in wrappers/rebrowser.
