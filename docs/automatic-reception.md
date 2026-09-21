# Automatic media reception and low-bandwidth recovery

`PUT /v1/media-receiver` accepts `provider: "auto"` to arm Spotify and AirPlay
on one paired display. Manual `spotify` and `airplay` modes remain unchanged.
The configured provider remains `auto` in snapshots; the actual source is identified
by `plan.item.provider` and `nowPlaying.provider`. Disabled or absent optional
workers do not block the other provider. Reads have bounded deadlines/cancellation.

A newly active sender must appear in two consecutive successful polls before it
replaces an existing sender. Metadata updates do not count as new activity. An
initial tie deterministically chooses AirPlay; steady simultaneous streams do not
oscillate. Unknown reads preserve prior activity evidence. New session allocation
succeeds before the old session is released. This confirms gateway session readiness,
not that a physical decoder rendered audio/video. Failed replacement allocation
retains the previous stream and permits retry. Local Stop suppresses that source
until idle; another new sender may still be accepted.

Spotify controls are owned only while Spotify is selected. Cast and YouTube keep
exclusive transport claims; this is not universal protocol handoff.

`PlaybackRequest.quality` optionally selects `STANDARD` or `LOW`. LOW requires
`mode: TRANSCODE`, maps to 426×240 H.264 baseline, 400k video target/500k maximum,
1000k buffer and 64k stereo AAC. STANDARD preserves the existing 640×360 profile.
No request can inject encoder flags or arbitrary sizes. Manual audio selection
preserves the session's quality choice. These are reactive recovery profiles,
not measured network classes or continuously adaptive bitrate streaming.
