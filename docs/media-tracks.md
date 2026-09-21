# Local media tracks

With `-media-tools`, the injected FFmpeg adapter provides session-owned track
inventory, audio selection and plain text subtitles. Full and Edge enable the
same core flag; Termux still needs its native FFmpeg package.

`internal/playback` maps probe metadata into semantic tracks; `internal/subtitles`
normalizes bounded SRT/VTT. `internal/media` owns process arguments, capacity,
output limits and cancellation. HTTP handlers enforce session ownership and
never accept paths, provider headers or arbitrary FFmpeg options from clients.

Audio selection creates a replacement transcode plan. The client releases the
previous session, and then starts the replacement with a timeline offset for
progress and subtitle alignment. A bounded two-second worker grace period allows
cancellation to reap the previous process without exceeding one conversion job.
Transcode resume uses accurate input seeking. Remux still starts at zero; exact
keyframe-aware remux resume remains open.

Text extraction supports embedded SRT/VTT, ASS/SSA (simplified), mov_text and text.
Image subtitles are reported unselectable. Remote/live sources and missing tools
return unavailable inventory. Sidecars, language preference automation, native
selection and bitmap burn-in remain pending. No provider credentials leave the
server. See the pinned protocol's `protocol/compatibility.md` for endpoint bounds.

`go test ./internal/media ./internal/subtitles ./internal/server` includes real
synthetic FFmpeg stream selection, audio frequency verification, timing, plain
text normalization, session ownership and cancellation. This is host evidence;
Android output/synchronization still requires physical validation.
