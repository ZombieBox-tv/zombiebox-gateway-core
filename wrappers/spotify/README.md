# Spotify Connect worker

Pinned go-librespot runs as a supervised external process. Its HTTP API and
PCM FIFO stay inside the worker's protected state/network boundary. One
cancellable FFmpeg reader converts fixed 44.1 kHz stereo s16le into MP3 for
the gateway's live, non-seekable playback plan.
The gateway translates track metadata and a finite command set into semantic
models. Provider tokens and raw upstream identities never reach Android.

The staged source applies [licensed-vorbis.patch](patches/licensed-vorbis.patch)
to the exact upstream commit. It replaces the unlicensed `xlab/vorbis-go` binding
with MIT-licensed `jfreymuth/oggvorbis` v1.0.5 and `jfreymuth/vorbis` v1.0.2.
This uses a pure-Go Vorbis decoder, reducing native library requirements; CPU and
memory behavior on the actual Android sender/receiver are still unmeasured. Run
`make spotify-patch-check` for synthetic metadata, decoded samples, gain and seek
checks. Corresponding-source packages must carry this patch and both MIT licenses.

The Full package defaults new installations to Spotify Connect Zeroconf. With
the optional Spotify profile running, choose Zombie Box from Spotify's device
picker on a phone on the same LAN. The phone's signed-in account pairs the
receiver; the separate device-authorization URL/code flow remains available by
selecting `device_auth` mode in Full's configuration helper. Existing
`device_auth` configuration and stored account credentials are preserved during
upgrade. The mode change takes effect after restarting the worker and does not
delete stored account state. The sender account must be eligible for Spotify
Connect; physical phone discovery and playback are not yet verified.

Private configuration: `.local/spotify/worker.json` and
`.local/spotify/state/config.yml`; credentials persist in the worker's state
directory. Full's source-built candidate runs this worker in host-network mode
for mDNS discovery and the built-in Zeroconf TCP pairing listener (default port
3679). Its bearer-protected HTTP API binds only to Docker's host-gateway bridge
address on port 8092, not the LAN address; do not use an older Spotify worker
image with this host-network configuration. The client reports account readiness
separately from worker availability. `volume_steps` must remain 100 for the
current semantic percentage command mapping.

Read status at `GET /v1/player/spotify`. Shared player writes and authorization
codes require a paired device plus `X-Zombie-Admin-Code`, except that dev.12 permits
player commands by the selected receiver lease owner. Authorization codes always
require the operator. Seamless music across separate Android Activities remains open.
Closing a media stream releases its FIFO reader; it does not disconnect the
Spotify account. The next listening session reopens the bridge.

Native Edge: the licensed patch can be staged for an Android/Bionic build. The
legacy source installer still needs to consume that staging path; prebuilt Edge
distribution and physical execution are separate gates. No Linux binary is reused.

Upstream: https://github.com/devgianlu/go-librespot,
commit `57d7278d94a9233060c2a6238f5926ffd1e72de4` (GPL-3.0).
Images carry its license and commit; source is restored by `make references`.
Distribution notices/source obligations remain a beta release gate.
