# Capability-aware audio replacement

Dev.41 selects a new owned playback session for an explicit audio choice. Only the
chosen audio stream participates in codec policy; an unused DTS/other track does
not force conversion of an otherwise compatible choice. Original metadata remains
unchanged. The request probes at most once and shares that result with its planner.

At position zero, compatible video and selected audio use REMUX. Nonzero position
uses TRANSCODE because the existing adapter requires conversion for accurate seek.
The LOW bandwidth cap and an existing TRANSCODE session retain conversion. Known
fragmented-MP4 failure rejects both outputs; known baseline/AAC failure rejects a
conversion that needs those codecs. Unknown evidence remains a candidate, not a
successful device probe. This is shared by Full and Edge.

Selection is advertised only when the appropriate local/remote adapter exists.
An unavailable path, invalid/non-audio index or incompatible output returns 409
before creating a replacement or stopping the old session. The client can keep
playing and choose another fallback. Sources, provider headers and credentials
remain server-owned. Live/split-input track selection is still unavailable; this
change does not manufacture stable track IDs for those inputs.

Host evidence covers compatible/unsupported selected codecs, fragment/decoder
failure, bandwidth/conversion intent, absent adapters, one-probe decisions,
ownership and session preservation. Real FFmpeg fixtures decode the second audio
track in both remux and transcode outputs and check resumed duration. Legacy media
execution, additional manifest combinations and physical/account acceptance remain
open. These are selection sub-items, not completion of the media work package.
