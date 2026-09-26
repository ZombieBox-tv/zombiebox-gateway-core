# Selected media receiver (dev.12)

`internal/receivers/inbox` owns the explicit single-client lease, cancellation,
metadata-only updates and source/session lifetime. It consumes Backend/Sessions
ports; the server adapter uses constructor-injected provider/process boundaries
and ticketed sessions. Full and Edge share the implementation.

A paired client chooses Spotify or AirPlay. Polling renews a 45-second foreground
lease; a five-second sweep stops abandoned streams. The backend reads fresh worker
activity independently of the Home cache. AirPlay prefers an active screen stream,
then audio-only. Spotify metadata/paused state update without replacing the audio
reader. Provider input changes recreate the session. Unknown network state keeps
the last confirmed session; it never invents a sender disconnect.

Local stop dismisses the current incoming item until confirmed idle or explicit
re-arm. Another client cannot steal a live lease, observe its plan, stop it or
control Spotify without the operator code. Authorization prompts still require
the operator. Releasing the stream does not log out the account or stop UxPlay.

This is foreground single-provider selection, not automatic arbitration among all
household devices or background ownership across Android Activities. AirPlay
activity currently derives from recent worker playlists (up to 15 seconds of idle
lag); no unsupported sender identity/metadata/artwork is fabricated. Music artwork,
full reconnect/queue policy and physical A/V evidence remain open.

## Live audio transport selection (unreleased)

For live audio-only AAC HLS, Auto first uses native HLS when current functional
device evidence permits it. Otherwise it tries a copied AAC ADTS stream only with
a fresh, advancing ADTS probe. If that is unplayable, it can decode to stereo
44.1 kHz PCM16 and send it through the Client's AudioTrack HTTP reader, but only
after that device's AudioTrack playback head advanced at least 500 ms in a fresh
probe. Without that evidence the plan uses the external player fallback. An
explicit REMUX request cannot silently select PCM, and an explicit TRANSCODE
request bypasses the copied ADTS rung.

Native HLS avoids conversion, and ADTS preserves the encoded AAC stream. PCM is
the more compatible, higher-bandwidth rung (about 1.4 Mbit/s before HTTP
overhead); it does not restore quality already lost in the source codec. The PCM
plan is live and unseekable, with a strict private MIME contract and bounded
conversion, HTTP reads, AudioTrack buffering and cancellation. The selected
Vizio API 13 passed the silent AudioTrack probe with 2367 ms of playback-head
advance on Client dev.58. That is device output-path evidence, not proof of
audible Apple Music playback or automatic AirPlay handoff.

## AirPlay physical-QA boundary (September 24, 2026)

The optional Full AirPlay worker can advertise and present its separate four-digit
receiver PIN without producing a Client playback session. The paired Client must
also arm AirPlay (or Auto) in Media Receiver options and keep polling in the
foreground. A PIN confirmation on iPad therefore does not establish that audio
or video reached the TV.

In the local QA worker, the private status was idle at inspection time. Its state
volume contained earlier audio HLS segments and later metadata/artwork, but no
video HLS manifest. That is evidence of prior audio bridge output only; it is
not evidence that the Vizio decoded or played the stream. The authenticated
gateway test covers AirPlay video-to-audio-to-idle plan replacement and private
HLS manifest/segment ticketing with a simulated worker, not real iPad playback.

The current worker bridges UxPlay's mirrored H.264/L16 RTP output into HLS.
UxPlay's pinned upstream documents a different, non-mirroring HLS path for
YouTube video from iOS: without `-hls`, that app may send only audio. Merely
adding `-hls` is not a fix: UxPlay then plays HLS through its own GStreamer
playbin instead of feeding the worker's RTP-to-HLS bridge. Supporting that path
requires a bounded, headless video/audio output adapter into the gateway and
separate iOS-to-TV validation. Until then, AirPlay YouTube video is not a
verified feature; test screen mirroring and audio reception separately.
