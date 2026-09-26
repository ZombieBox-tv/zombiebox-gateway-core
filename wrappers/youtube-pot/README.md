# YouTube PO Token HD Resolver Wrapper

This wrapper provides an optional Proof-of-Origin (PO) Token resolver for Full. It asks yt-dlp to use YouTube's `mweb` player client, where the BgUtils plugin supplies a GVS token, then supplements the existing 360p path with validated H.264 <=30fps video and AAC audio formats when available.

## Features

- **mweb GVS PO Token Flow:** Explicitly selects yt-dlp's `mweb` client and uses the `bgutil-ytdlp-pot-provider` plugin to request the GVS token from an internal `bgutil-provider` Botguard daemon. The plugin and yt-dlp versions are pinned in `requirements.lock`.
- **Strict Format Guardrails:** Enforces H.264 (`avc1`/`h264`) video at `<= 30 fps` and AAC (`mp4a`/`aac`) audio. Higher frame rates (50/60fps) and non-hardware-friendly codecs (VP9/AV1/Opus) are rejected.
- **Range Verification:** Verifies 3-point HTTP byte ranges (head 0–1023, mid, tail) to guarantee streams are actually servable without 403 truncations.
- **Truthful Variants:** Only formats with validated ranges and valid audio pairs are reported in `variants`.
- **Exact Manual Quality:** A manual `1080p`, `720p`, `480p`, or `360p` request either returns that validated tier or fails with HTTP 502 `{"error":"quality_unavailable"}`. It never reports baseline 360p as a successful manual HD selection. Successful results include numeric `actualHeight` and `actualQuality` when yt-dlp provides unambiguous height metadata; a label alone is not treated as proof.
- **403 Cooldown & Single-Flight Lock:** Concurrency is locked to 1. If YouTube returns 403 or bot check, a 300s cooldown is activated. Auto and unspecified quality may use the upstream 360p baseline during cooldown, extraction failure, or rendition-validation failure. Manual exact-quality requests fail instead of changing quality silently.
- **Zero Catalog Fork:** `/catalog` and `/browse` are transparently proxied to the upstream YouTube.js worker (`http://youtube:8091`).
- **Internal Only:** Runs unexposed on the internal Docker bridge network without published host/LAN ports.

The provider can still fail when YouTube changes its attestation or playback
checks. A successful metadata extraction or byte-range check does not prove that
the TV can play an HD stream. Do not claim a resolution above 360p until the
selected physical device completes playback at that resolution.

## Configuration

Stored in `/config/pot.json`:

```json
{
  "token": "<worker_token_32_chars>",
  "upstream_url": "http://youtube:8091",
  "bgutil_url": "http://bgutil-provider:4416",
  "cooldown_seconds": 300,
  "timeout_seconds": 12
}
```

The resolver caps yt-dlp extraction at 30 seconds and its captured stdout and
stderr at 4 MiB and 256 KiB. It terminates the yt-dlp process group on timeout
or output overflow so the Node.js challenge runtime cannot remain orphaned.

## Running Tests

```sh
python3 -m unittest discover -s wrappers/youtube-pot/tests
```
