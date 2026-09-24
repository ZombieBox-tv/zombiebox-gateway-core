import { resolveFormats } from "./formats.mjs";

// The anonymous WEB client can return format shells without decipherable URLs.
// Try clients with playable MP4 origins, while preserving the existing format gate.
export async function resolveVideo(yt, id) {
  let lastError;
  for (const client of ["IOS", "ANDROID", "WEB"]) {
    try {
      const info = await yt.getBasicInfo(id, { client });
      return await resolveFormats(info, yt.session.player);
    } catch (error) {
      lastError = error;
    }
  }
  throw lastError ?? new Error("video_unavailable");
}
