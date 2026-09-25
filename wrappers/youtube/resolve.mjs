import { resolveFormats } from "./formats.mjs";
import { validateMediaRanges } from "./media-ranges.mjs";

// The anonymous WEB client can return format shells without decipherable URLs.
// Try clients with playable MP4 origins, while preserving the existing format gate.
export async function resolveVideo(yt, id, validate = validateMediaRanges) {
  let lastError;
  let checked = 0;
  // Android often offers a combined H.264/AAC stream. It avoids a gateway mux
  // and can remain playable when an iOS adaptive track only serves its head.
  for (const client of ["ANDROID", "IOS", "WEB"]) {
    try {
      const info = await yt.getBasicInfo(id, { client });
      return await resolveFormats(info, yt.session.player, (url, format) => {
        if (++checked > 10) return false;
        return validate(url, format);
      });
    } catch (error) {
      lastError = error;
    }
  }
  throw lastError ?? new Error("video_unavailable");
}
