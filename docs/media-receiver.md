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
