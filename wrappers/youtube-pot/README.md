# YouTube PO Token HD Resolver Wrapper

This wrapper provides an optional YouTube HD resolver for Full. It first tries yt-dlp's default player clients with plugins disabled, then may try YouTube's `mweb` client with a BgUtils GVS PO token when the default candidate cannot provide the requested tier.

## Features

- **Default Client First:** Runs the pinned yt-dlp with `--no-plugin-dirs` so its default player clients can expose directly usable formats without PO-token plugin behavior.
- **mweb GVS PO Token Fallback:** If default-client extraction cannot provide the requested validated tier and the PO path is not in cooldown, it selects yt-dlp's `mweb` client and uses the `bgutil-ytdlp-pot-provider` plugin to request the GVS token from an internal `bgutil-provider` Botguard daemon. The plugin and yt-dlp versions are pinned in `requirements.lock`.
- **Strict Format Guardrails:** Enforces direct HTTPS `/videoplayback` MP4 video with H.264 (`avc1`/`h264`) at `<= 30 fps` and direct AAC (`mp4a`/`aac`) audio. HLS/DASH manifests, higher frame rates (50/60fps) and incompatible codecs (VP9/AV1/Opus) are rejected by this byte-ranged MP4 path.
- **Range Verification:** Verifies 3-point HTTP byte ranges (head 0–1023, mid, tail) to guarantee streams are actually servable without 403 truncations.
- **Truthful Variants:** Only formats with validated ranges and valid audio pairs are reported in `variants`.
- **Exact Manual Quality:** A manual `1080p`, `720p`, `480p`, or `360p` request either returns that validated tier or fails with HTTP 502 `{"error":"quality_unavailable"}`. It never reports baseline 360p as a successful manual HD selection. Successful results include numeric `actualHeight` and `actualQuality` when yt-dlp provides unambiguous height metadata; a label alone is not treated as proof.
- **PO 403 Cooldown & Single-Flight Lock:** Concurrency is locked to 1. A 403 or bot check on the mweb/PO candidate activates a 300s cooldown for that candidate; default clients are still tried during cooldown. A default-client 403 does not block the PO candidate. Auto and unspecified quality may use the upstream 360p baseline after both candidates fail. Manual exact-quality requests fail instead of changing quality silently.
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

The resolver shares an 18-second deadline across default extraction, PO fallback,
range validation and automatic baseline fallback, within the Gateway's 20-second
HTTP client timeout. Each yt-dlp process also has a 30-second hard ceiling, and
stdout and stderr are capped at 4 MiB and 256 KiB. Range requests receive the
remaining resolution budget as their network timeout. The resolver terminates the
yt-dlp process group on timeout or output overflow so the Node.js runtime cannot
remain orphaned. Manual quality probes validate only the requested tier and its AAC
pair; Auto still validates the full inventory before reporting its variants.

## Running Tests

```sh
python3 -m unittest discover -s wrappers/youtube-pot/tests
```
