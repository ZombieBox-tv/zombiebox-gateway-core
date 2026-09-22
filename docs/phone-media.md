# Phone media handoff (dev.30)

A paired companion can upload one user-selected audio/video file and hand it to
its approved Client. Full and Edge use this same injected store and media planner;
MediaMTX is not required. `-media-tools` enables the path when the temporary store
can be opened. FFprobe/FFmpeg availability and actual receiver playback remain
separate from registration. `mediaAvailable` is an additive companion status hint.

## Transport and ownership

- `PUT /v1/companion/media/{mediaId}` accepts a raw fixed-length body, 1 byte through
  256 MiB. The sender generates a 32-character lowercase hex ID. No URL, filesystem
  path or receiver ID is accepted. Upload success returns `UPLOADED`, not playback.
- `POST /v1/companion/media/{mediaId}/play` accepts an optional title (120 characters,
  no control characters). Probe actual streams, apply the existing capability and
  recent-bandwidth planner, then recheck consent/ownership. Existing receiver
  playback is retired only after a replacement session is staged. Repeating a
  successful start is idempotent while the session exists.
- `GET /v1/companion/media` reports only that companion's accepted media. It returns
  no stream ticket, local path, provider secret or another phone's media.
- `DELETE /v1/companion/media/{mediaId}` cancels the owner's pending/active file.
  Revocation, receiver stop/completion, receiver disappearance, expiry and shutdown
  release the session and temporary file. A different companion cannot read,
  start or delete the file even if it knows its ID.

The TV reads the existing authenticated `/v1/cast/active` plan and ticketed stream.
Files are VOD, with range/seek support for direct play; remux/transcode remains
progressive fragmented MP4 without arbitrary seeking. Audio files have audio item
semantics. No sender heartbeat is needed after handoff: the TV owns playback for
up to six hours, with the existing 60-second receiver-offline cleanup. Cast sender
leases/RTSP credentials cannot be manufactured for uploaded files. Only one active
Cast/file session is allowed; the existing consented media/YouTube handoff rules
apply. This is not universal automatic receiver coordination.

## Resource and process boundaries

The private `companion-media` directory beside SQLite holds at most two reserved
files, each at most 256 MiB (512 MiB total). Files use generated names and mode0600;
no library path is writable. Failed/short/oversize transfers remove partial files.
An in-progress cancellation keeps the ID reserved until the writer finishes, so a
late writer cannot resurrect a cancelled file or delete a replacement. Uploads
have a five-minute deadline; unused files expire after ten minutes. Startup removes
only this feature's generated temporary filenames. One process must own the state
directory. Uploads do not survive gateway restart or enter catalog/history.

Container signatures reject arbitrary files/text playlists before probing. Local
FFprobe, conversion and subtitle extraction now restrict demuxers to self-contained
media/text-subtitle formats; concat/HLS/DASH playlists cannot open other gateway
files. Remote manifests retain their separate bounded adaptation graph. Existing
process concurrency, cancellation, protocol restrictions and output limits apply.

MP4, MKV/WebM, MP3, WAV, FLAC and Ogg are candidate input containers, not a claim
that every codec/content variant plays. Known unsupported receiver output rejects
handoff; FFmpeg/decoder/DRM failures are not successful playback evidence.

## Evidence and remaining work

Host tests cover bounded storage, interrupted writes, owner isolation, consent,
idempotent starts, ticket revocation and cleanup. A synthetic FLAC upload runs
through the real FFprobe/planner/FFmpeg path into audio-only AAC fragmented MP4;
completion invalidates its stream. No physical or source-account evidence follows
from this check. URL sending, playlists/queues, large/resumable uploads, artwork,
advanced tracks and expanded codec/DRM coverage remain separate work.
