export const MAX_SAFE_FPS = 30;

function labelFps(value) {
  if (typeof value !== "string") return 0;
  const match = /^\d{3,4}p\s*(\d{2,3})(?:\s*fps)?(?:\s|$)/i.exec(value.trim());
  return match ? Number(match[1]) : 0;
}

export function isSafeFps(format) {
  if (!format) return false;
  const fps = Number(format.fps);
  if (!Number.isFinite(fps) || fps <= 0 || fps > MAX_SAFE_FPS) return false;
  const label = typeof format.quality_label === "string" ? format.quality_label : "";
  const quality = typeof format.quality === "string" ? format.quality : "";
  if (labelFps(label) > MAX_SAFE_FPS || labelFps(quality) > MAX_SAFE_FPS) return false;
  return true;
}

function hasAVCCodec(format) {
  return (
    typeof format?.mime_type === "string" && /\bavc1(?:\.[a-f0-9]+)?\b/i.test(format.mime_type)
  );
}

function qualityLabelHeight(format) {
  const label = typeof format?.quality_label === "string" ? format.quality_label.trim() : "";
  const match = /^(\d{3,4})p(?:\s*(\d{2,3})(?:\s?fps)?)?$/i.exec(label);
  return match ? Number(match[1]) : 0;
}

function matchesVideoTier(format, tier) {
  const requestedHeight = Number.parseInt(tier, 10);
  if (!Number.isInteger(requestedHeight) || requestedHeight <= 0) return false;

  const height = Number(format?.height);
  const rawHeight = format?.height;
  const hasRawHeight = rawHeight !== undefined && rawHeight !== null && rawHeight !== "";
  const hasHeight = Number.isSafeInteger(height) && height > 0;
  const labelHeight = qualityLabelHeight(format);
  if (hasRawHeight && !hasHeight) return false;
  if (!hasHeight && labelHeight === 0) return false;
  if (hasHeight && height !== requestedHeight) return false;
  if (labelHeight && labelHeight !== requestedHeight) return false;
  return true;
}

function isSupportedVideoTier(format, tier) {
  return Boolean(
    format?.has_video && hasAVCCodec(format) && isSafeFps(format) && matchesVideoTier(format, tier),
  );
}

export const TIER_LABELS = Object.freeze({
  "1080p": ["1080p"],
  "720p": ["720p"],
  "480p": ["480p"],
  "360p": ["360p"],
});

export async function getAvailableVariants(
  info,
  player,
  validate = async () => true,
  tiersToCheck = ["1080p", "720p", "480p", "360p"],
) {
  if (!info || typeof info.chooseFormat !== "function") return [];
  const tiers = Array.isArray(tiersToCheck) ? tiersToCheck : ["1080p", "720p", "480p", "360p"];
  if (tiers.length === 0) return [];
  const checked = new Map();

  const isValid = (format) => {
    if (!format) return Promise.resolve(false);
    const identity = Number.isInteger(format.itag) ? format.itag : format;
    if (checked.has(identity)) return checked.get(identity);
    const promise = (async () => {
      try {
        const url = typeof format.decipher === "function" ? await format.decipher(player) : "";
        if (!url) return false;
        return Boolean(await validate(url, format));
      } catch {
        return false;
      }
    })();
    checked.set(identity, promise);
    return promise;
  };

  let audioPromise = null;
  const getHasValidAudio = () => {
    if (!audioPromise) {
      audioPromise = (async () => {
        for (const quality of ["bestefficiency", "best"]) {
          try {
            const audio = info.chooseFormat({
              type: "audio",
              format: "mp4",
              codec: "mp4a",
              quality,
            });
            if (audio && audio.has_audio && !audio.has_video) {
              if (await isValid(audio)) {
                return true;
              }
            }
          } catch {}
        }
        return false;
      })();
    }
    return audioPromise;
  };

  const checkTier = async (tier) => {
    try {
      const candidateLabels = TIER_LABELS[tier] || [tier];
      for (const quality of candidateLabels) {
        let hasCombined = false;
        try {
          const combined = info.chooseFormat({
            type: "video+audio",
            format: "mp4",
            codec: "avc1",
            quality,
          });
          if (combined && combined.has_audio && isSupportedVideoTier(combined, tier)) {
            hasCombined = await isValid(combined);
          }
        } catch {}

        if (hasCombined) {
          return tier;
        }

        let video = null;
        try {
          video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
        } catch {}

        if (video && !video.has_audio && isSupportedVideoTier(video, tier)) {
          const hasAudio = await getHasValidAudio();
          if (!hasAudio) {
            return null;
          }
          if (await isValid(video)) {
            return tier;
          }
        }
      }
    } catch {}

    return null;
  };

  const results = new Array(tiers.length);
  let nextIndex = 0;
  const concurrency = 2;

  const worker = async () => {
    while (nextIndex < tiers.length) {
      const idx = nextIndex++;
      results[idx] = await checkTier(tiers[idx]);
    }
  };

  const workers = [];
  for (let i = 0; i < Math.min(concurrency, tiers.length); i++) {
    workers.push(worker());
  }
  await Promise.all(workers);

  return results.filter(Boolean);
}

// Prefer the highest validated H.264 source up to 1080p. At the same resolution,
// use a combined stream to avoid muxing; adaptive video requires validated AAC.
// Video-only is never a playable result.
export async function resolveFormats(
  info,
  player,
  validate = async () => true,
  targetQuality = "",
) {
  const checked = new Set();
  const playable = async (format) => {
    if (!format) return "";
    const identity = Number.isInteger(format.itag) ? format.itag : format;
    if (checked.has(identity)) return "";
    checked.add(identity);
    const url = await format.decipher(player);
    return url && (await validate(url, format)) ? url : "";
  };
  let audioUrl = "";
  let audioChecked = false;
  let adaptiveVideoFound = false;
  const pairedAudio = async () => {
    if (audioChecked) return audioUrl;
    audioChecked = true;
    for (const quality of ["bestefficiency", "best"]) {
      try {
        const audio = info.chooseFormat({ type: "audio", format: "mp4", codec: "mp4a", quality });
        if (audio.has_audio && !audio.has_video) {
          audioUrl = await playable(audio);
          if (audioUrl) break;
        }
      } catch {
        /* Try another AAC format before changing clients. */
      }
    }
    return audioUrl;
  };

  const requestedTier = targetQuality && targetQuality !== "auto" ? targetQuality : "";

  if (requestedTier) {
    const candidateQualities = TIER_LABELS[requestedTier] || [requestedTier];
    for (const quality of candidateQualities) {
      try {
        const combined = info.chooseFormat({
          type: "video+audio",
          format: "mp4",
          codec: "avc1",
          quality,
        });
        if (combined.has_audio && isSupportedVideoTier(combined, requestedTier)) {
          const url = await playable(combined);
          if (url) return { url, mimeType: "video/mp4" };
        }
      } catch {
        /* Fall through to a separate H.264/AAC pair at this resolution. */
      }
      try {
        const video = info.chooseFormat({
          type: "video",
          format: "mp4",
          codec: "avc1",
          quality,
        });
        if (!video.has_audio && isSupportedVideoTier(video, requestedTier)) {
          const url = await playable(video);
          if (url) {
            adaptiveVideoFound = true;
            if (await pairedAudio()) return { url, audioUrl, mimeType: "video/mp4" };
          }
        }
      } catch {}
    }
    if (adaptiveVideoFound && !audioUrl) throw new Error("audio_unavailable");
    throw new Error("video_unavailable");
  }

  // Work Item 1: Keep Auto's current progressive combined H.264/AAC 360p as first
  // playback result when valid, even when validated HD variants exist for the menu.
  try {
    const baseline = info.chooseFormat({
      type: "video+audio",
      format: "mp4",
      codec: "avc1",
      quality: "360p",
    });
    if (baseline?.has_audio && isSupportedVideoTier(baseline, "360p")) {
      const baselineUrl = await playable(baseline);
      if (baselineUrl) return { url: baselineUrl, mimeType: "video/mp4" };
    }
  } catch {
    /* If baseline is invalid or unavailable, use the existing validated fallback chain. */
  }

  // Prefer validated combined progressive formats before adaptive sources.
  // This avoids an expensive gateway spool when 360p had a transient validation
  // failure but a progressive 480p or 720p source is available.
  for (const quality of ["720p", "480p"]) {
    try {
      const combined = info.chooseFormat({
        type: "video+audio",
        format: "mp4",
        codec: "avc1",
        quality,
      });
      if (combined?.has_audio && isSupportedVideoTier(combined, quality)) {
        const url = await playable(combined);
        if (url) return { url, mimeType: "video/mp4" };
      }
    } catch {
      /* Try the next progressive tier before considering adaptive sources. */
    }
  }

  // Auto keeps adaptive 1080p as a last resort: lower tiers are less costly to
  // spool and are tried first. Explicit quality requests take the separate
  // exact-tier branch above and retain their selected resolution.
  for (const quality of ["720p", "480p", "360p", "1080p"]) {
    try {
      const video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
      if (!video?.has_audio && isSupportedVideoTier(video, quality)) {
        const url = await playable(video);
        if (url) {
          adaptiveVideoFound = true;
          if (await pairedAudio()) return { url, audioUrl, mimeType: "video/mp4" };
        }
      }
    } catch {
      /* Missing, expired or unsuitable formats fall through to the next tier. */
    }
  }

  if (adaptiveVideoFound && !audioUrl) throw new Error("audio_unavailable");
  throw new Error("video_unavailable");
}
