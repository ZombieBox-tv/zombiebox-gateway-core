import { resolveFormats, getAvailableVariants } from "./formats.mjs";
import { validateMediaRanges } from "./media-ranges.mjs";

const TIERS = ["1080p", "720p", "480p", "360p"];
const DEFAULT_VALIDATION_DEADLINE_MS = 12_000;
const MAX_VALIDATION_CHECKS = 25;

// The anonymous WEB client can return format shells without decipherable URLs.
// Try clients with playable MP4 origins, while preserving the existing format gate.
export async function resolveVideo(
  yt,
  id,
  validate = validateMediaRanges,
  targetQuality = "",
  options = {},
) {
  const deadlineMs = options.deadlineMs ?? DEFAULT_VALIDATION_DEADLINE_MS;
  const startTime = Date.now();
  let lastError;
  let checked = 0;

  // Signed URLs can differ by client even for the same itag. Cache only the
  // exact URL and declared size used by the range validator.
  const validationCache = new Map();

  const boundValidate = async (url, format) => {
    if (!url) return false;

    // Bound resolution latency with a shared deadline:
    if (Date.now() - startTime >= deadlineMs) {
      return false;
    }

    const key = `${url}\n${format?.content_length ?? ""}`;
    if (validationCache.has(key)) {
      return validationCache.get(key);
    }

    if (++checked > MAX_VALIDATION_CHECKS) {
      return false;
    }

    const promise = (async () => {
      try {
        const ok = Boolean(await validate(url, format));
        return ok;
      } catch {
        return false;
      }
    })();

    validationCache.set(key, promise);

    return promise;
  };

  const discoveredVariants = new Set();
  let resolvedResult = null;
  const clients = ["ANDROID", "IOS", "WEB"];

  for (const client of clients) {
    if (resolvedResult && Date.now() - startTime >= deadlineMs) {
      break;
    }

    let info;
    try {
      info = await yt.getBasicInfo(id, { client });
    } catch (error) {
      lastError = error;
      continue;
    }

    if (!resolvedResult) {
      try {
        resolvedResult = await resolveFormats(
          info,
          yt.session.player,
          boundValidate,
          targetQuality,
        );
      } catch (error) {
        lastError = error;
      }
    }

    if (Date.now() - startTime < deadlineMs) {
      try {
        const variants = await getAvailableVariants(info, yt.session.player, boundValidate);
        for (const v of variants) {
          discoveredVariants.add(v);
        }
      } catch {}
    }

    if (resolvedResult) {
      // If we already discovered HD variants (both 1080p and 720p), or completed mobile clients, stop.
      const hasHD = discoveredVariants.has("1080p") && discoveredVariants.has("720p");
      if (hasHD || client === "IOS" || client === "WEB" || Date.now() - startTime >= deadlineMs) {
        break;
      }
    }
  }

  if (!resolvedResult) {
    throw lastError ?? new Error("video_unavailable");
  }

  const variants = TIERS.filter((t) => discoveredVariants.has(t));
  if (variants.length > 0) {
    return { ...resolvedResult, variants };
  }
  return resolvedResult;
}
