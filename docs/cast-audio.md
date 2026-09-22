# Audio-only Cast contract

`POST /v1/cast` and the companion equivalent accept optional `mode: SCREEN|AUDIO`.
Omission preserves SCREEN and its existing video-budget behavior. Unknown modes
return 400. AUDIO grants echo the mode, omit video and advertise AAC 44.1 kHz,
stereo, 128 kbit/s. A known failed AAC/HLS probe rejects Audio; failed H.264 alone
does not. Unknown transport/decoder support is a candidate, not runtime evidence.

Ready audio sessions expose a live HLS plan with item kind `audio` and title
`Audio sharing`. MediaMTX, stream tickets, receiver consent, handoff consent,
companion target binding, leases, one-session concurrency and revocation use the
existing Cast path. Full and Edge package this same core. A sender must refuse
Audio if an old gateway does not explicitly confirm it; no Screen fallback is safe.

Host tests cover wrong modes, disabled casting, AAC/HLS versus video failures,
audio plan metadata, stream revocation, and companion target isolation for both
Screen and Audio. Android/OEM playback and simultaneous universal reception remain
separate deferred gates. This is live app audio, not a media-file upload endpoint.
