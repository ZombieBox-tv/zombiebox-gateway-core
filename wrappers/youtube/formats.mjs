// Prefer a combined progressive stream. If unavailable, the gateway muxes an
// explicitly paired H.264/AAC selection; video-only is never a playable result.
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
  for (const quality of ["360p", "480p"]) {
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
      /* A different bounded format or client may still work. */
    }
  }
  let audioUrl = "";
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
  if (!audioUrl) throw new Error("audio_unavailable");
  for (const quality of ["360p", "480p", "720p"]) {
    try {
      const video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
      if (video.has_video && !video.has_audio) {
        const url = await playable(video);
        if (url) return { url, audioUrl, mimeType: "video/mp4" };
      }
    } catch {
      /* Keep resolution bounded and try the next known H.264 tier. */
    }
  }
  throw new Error("video_unavailable");
}
