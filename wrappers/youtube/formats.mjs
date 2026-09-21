// Prefer a combined progressive stream. If unavailable, the gateway muxes an
// explicitly paired H.264/AAC selection; video-only is never a playable result.
export async function resolveFormats(info, player) {
  try {
    const combined = info.chooseFormat({
      type: "video+audio",
      format: "mp4",
      codec: "avc1",
      quality: "360p",
    });
    if (combined.has_audio && combined.has_video) {
      const url = await combined.decipher(player);
      if (url) return { url, mimeType: "video/mp4" };
    }
  } catch {
    /* Adaptive streams below still need both tracks. */
  }
  let video;
  for (const quality of ["360p", "480p", "720p"]) {
    try {
      video = info.chooseFormat({ type: "video", format: "mp4", codec: "avc1", quality });
      if (video.has_video) break;
    } catch {
      /* Try the next bounded resolution. */
    }
  }
  if (!video?.has_video) throw new Error("video_unavailable");
  const audio = info.chooseFormat({
    type: "audio",
    format: "mp4",
    codec: "mp4a",
    quality: "bestefficiency",
  });
  if (!audio.has_audio || audio.has_video) throw new Error("audio_unavailable");
  const [url, audioUrl] = await Promise.all([video.decipher(player), audio.decipher(player)]);
  if (!url || !audioUrl) throw new Error("stream_unavailable");
  return { url, audioUrl, mimeType: "video/mp4" };
}
