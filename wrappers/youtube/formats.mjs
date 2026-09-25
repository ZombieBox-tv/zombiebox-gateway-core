// Prefer the highest validated H.264 source up to 1080p. At the same resolution,
// use a combined stream to avoid muxing; adaptive video requires validated AAC.
// Video-only is never a playable result.
export async function resolveFormats(info, player, validate = async () => true) {
  const checked = new Set();
  const playable = async (format) => {
    if (!format) return "";
    const identity = Number.isInteger(format.itag) ? format.itag : format;
    if (checked.has(identity)) return "";
    checked.add(identity);
    const url = await format.decipher(player);
    return url && (await validate(url, format)) ? url : "";
  };
  // Keep the known progressive baseline available if higher origins fail.
  // It is validated once, not returned until better renditions are considered.
  let progressiveFallback = "";
  try {
    const baseline = info.chooseFormat({
      type: "video+audio",
      format: "mp4",
      codec: "avc1",
      quality: "360p",
    });
    if (baseline.has_audio && baseline.has_video) progressiveFallback = await playable(baseline);
  } catch {
    /* Continue with adaptive video and other progressive tiers. */
  }
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
  for (const quality of ["1080p", "720p", "480p", "360p"]) {
    if (quality === "360p" && progressiveFallback)
      return { url: progressiveFallback, mimeType: "video/mp4" };
    if (quality !== "1080p" && quality !== "360p") {
      try {
        const combined = info.chooseFormat({
          type: "video+audio",
          format: "mp4",
          codec: "avc1",
          quality,
        });
        if (combined.has_audio && combined.has_video) {
          const url = await playable(combined);
          if (url) return { url, mimeType: "video/mp4" };
        }
      } catch {
        /* Fall through to a separate H.264/AAC pair at this resolution. */
      }
    }
    try {
      const video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
      if (video.has_video && !video.has_audio) {
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
