export async function getAvailableVariants(info, player, validate = async () => true) {
  if (!info || typeof info.chooseFormat !== "function") return [];
  const tiers = ["1080p", "720p", "480p", "360p"];
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

  const checkTier = async (quality) => {
    try {
      let hasCombined = false;
      try {
        const combined = info.chooseFormat({
          type: "video+audio",
          format: "mp4",
          codec: "avc1",
          quality,
        });
        if (combined && combined.has_audio && combined.has_video) {
          hasCombined = await isValid(combined);
        }
      } catch {}

      if (hasCombined) {
        return quality;
      }

      let video = null;
      try {
        video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
      } catch {}

      if (video && video.has_video && !video.has_audio) {
        const hasAudio = await getHasValidAudio();
        if (hasAudio && (await isValid(video))) {
          return quality;
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
    try {
      const combined = info.chooseFormat({
        type: "video+audio",
        format: "mp4",
        codec: "avc1",
        quality: requestedTier,
      });
      if (combined.has_audio && combined.has_video) {
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
        quality: requestedTier,
      });
      if (video.has_video && !video.has_audio) {
        const url = await playable(video);
        if (url) {
          adaptiveVideoFound = true;
          if (await pairedAudio()) return { url, audioUrl, mimeType: "video/mp4" };
        }
      }
    } catch {}
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
    if (baseline?.has_audio && baseline?.has_video) {
      const baselineUrl = await playable(baseline);
      if (baselineUrl) return { url: baselineUrl, mimeType: "video/mp4" };
    }
  } catch {
    /* If baseline is invalid or unavailable, use the existing validated fallback chain. */
  }

  // Validated fallback chain when baseline 360p combined is unavailable/invalid:
  for (const quality of ["1080p", "720p", "480p", "360p"]) {
    if (quality !== "1080p" && quality !== "360p") {
      try {
        const combined = info.chooseFormat({
          type: "video+audio",
          format: "mp4",
          codec: "avc1",
          quality,
        });
        if (combined?.has_audio && combined?.has_video) {
          const url = await playable(combined);
          if (url) return { url, mimeType: "video/mp4" };
        }
      } catch {
        /* Fall through to a separate H.264/AAC pair at this resolution. */
      }
    }
    try {
      const video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
      if (video?.has_video && !video?.has_audio) {
        const url = await playable(video);
        if (url) {
          adaptiveVideoFound = true;
          if (await pairedAudio()) return { url, audioUrl, mimeType: "video/mp4" };
        }
      }
    } catch {
      /* A higher tier may be missing, expired or unsuitable; try a lower one. */
    }
  }

  if (adaptiveVideoFound && !audioUrl) throw new Error("audio_unavailable");
  throw new Error("video_unavailable");
}
