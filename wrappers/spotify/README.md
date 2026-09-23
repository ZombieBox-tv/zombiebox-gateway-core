# Spotify Connect worker

Pinned go-librespot runs as a supervised external process. Its loopback API and
PCM FIFO stay inside the worker. One cancellable FFmpeg reader converts fixed
44.1 kHz stereo s16le into MP3 for the gateway's live, non-seekable playback plan.
The gateway translates track metadata and a finite command set into semantic
models. Provider tokens and raw upstream identities never reach Android.

The staged source applies [licensed-vorbis.patch](patches/licensed-vorbis.patch)
to the exact upstream commit. It replaces the unlicensed `xlab/vorbis-go` binding
with MIT-licensed `jfreymuth/oggvorbis` v1.0.5 and `jfreymuth/vorbis` v1.0.2.
This uses a pure-Go Vorbis decoder, reducing native library requirements; CPU and
memory behavior on the actual Android sender/receiver are still unmeasured. Run
`make spotify-patch-check` for synthetic metadata, decoded samples, gain and seek
checks. Corresponding-source packages must carry this patch and both MIT licenses.

Run `make services-build`, then `make spotify-up`. In client Settings → Gateway
services, enter the operator code and choose **Show Spotify pairing code**. Finish
the pairing on your phone. On the client select Settings → Receive Spotify / AirPlay
→ Spotify, then select Zombie Box in Spotify; active audio is routed automatically. `go-librespot` requires an eligible Spotify account; real
account playback remains unverified until credentials are supplied.

Private configuration: `.local/spotify/worker.json` and
`.local/spotify/state/config.yml`; credentials persist in the worker's state
directory. The default device-auth flow does not require multicast networking.
Custom Spotify Connect discovery/zeroconf is an operator configuration, not
advertised by the default bridged container. `volume_steps` must remain 100 for
the current semantic percentage command mapping.

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
